// Package agent provides server-side LLM reasoning for iac-server.
//
// The Planner uses a separate LLM (configured via LLM_* env vars) to:
//   - Read embedded terraform examples and generate .tf files for a request
//   - Query cloud pricing via the BSS API and web search
//   - Diagnose deployment errors and suggest fixes
//
// Skills (SKILL.md guides) are statically loaded via the SkillProvider and
// injected directly into each agent's system prompt. Agents that need
// per-service API knowledge on demand (query_cloud, specify_resources) also
// get the SkillProvider so they can load_skill at runtime. The LLM browses
// reference files (examples, API swagger) with standard read/grep/ls tools.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
	sloghooks "github.com/yusheng-g/openagent-go/hooks/slog"
	"github.com/yusheng-g/openagent-go/iac"
	"github.com/yusheng-g/openagent-go/kernel"
	"github.com/yusheng-g/openagent-go/provider/skill"
	"github.com/yusheng-g/openagent-go/session"
	opentool "github.com/yusheng-g/openagent-go/tool"
)

// Planner holds the dependencies for server-side LLM reasoning.
type Planner struct {
	model           openagent.Model
	cloud           provider.CloudProvider
	loader          skill.Provider        // loads skills from extracted skills dir
	memory          session.SessionStore  // conversation scoped by deployment_id
	knowledge       ctxpkg.MemoryProvider // durable knowledge
	workDir         string                // cloud home dir (parent of skills/ and deployments/), workDir for read/grep/ls
	deploymentsDir  string
	prompts         map[provider.PromptRole]provider.AgentConfig // cloud-specific agent prompts/skills
	dryRun          bool
	binaryMirrors   []string // terraform binary download mirrors
	providerMirrors []string // provider download mirrors
}

// New creates a Planner. workDir should be the cloud home directory
// (parent of skills/ and deployments/) so read/grep/ls can access both.
// memory is shared across all LLM calls and scoped by deployment_id —
// estimate_cost can see specify_resources' reasoning, troubleshoot can see
// prior attempts. nil disables memory (each call is isolated).
func New(model openagent.Model, cloud provider.CloudProvider, loader skill.Provider, memory session.SessionStore, knowledge ctxpkg.MemoryProvider, workDir, deploymentsDir string, dryRun bool, binaryMirrors, providerMirrors []string) *Planner {
	return &Planner{
		model:           model,
		cloud:           cloud,
		loader:          loader,
		memory:          memory,
		knowledge:       knowledge,
		workDir:         workDir,
		deploymentsDir:  deploymentsDir,
		prompts:         cloud.Agents(),
		dryRun:          dryRun,
		binaryMirrors:   binaryMirrors,
		providerMirrors: providerMirrors,
	}
}

// sessionID returns the Memory session key for a deployment.
func sessionID(deploymentID string) string {
	return "dep-" + deploymentID
}

// planResult is the JSON returned by propose/specify need_input flows.
type planResult struct {
	Status         string   `json:"status"` // "need_input" or "ready"
	Questions      []string `json:"questions,omitempty"`
	DeploymentID   string   `json:"deployment_id,omitempty"`
	Plan           any      `json:"plan,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
}

// serverContext is the shared context injected into every server-side LLM
// agent. It explains the MCP server's role, the client, the interaction
// model, and the output contract — without this the LLM doesn't know who
// it is serving or how its output is consumed.
const serverContext = `You are the server-side LLM of an MCP server (iac-server) that provides cloud infrastructure deployment and query capabilities over the MCP protocol.

## Your role
- You run on the SERVER side. You never talk to the end user directly.
- The MCP CLIENT (e.g. Claude Code, Cursor, openagent) calls one of the 11 MCP tools and forwards the user's request to you.
- Your output is returned to the client as the tool result. The client then decides what to show the user and whether to proceed.
- You do NOT need user approval for any action — approval is the client's concern, not yours.

## Deployment workflow (6 steps, user confirms between each)
  1. propose_architecture  — recommend a cloud architecture as a DAG (nodes = resources, depends_on = edges), no .tf files
  2. specify_resources     — determine concrete resource specs (flavor, image, CIDR, etc.), enrich the DAG
  3. generate_terraform_plan — write .tf files + run terraform plan from the DAG, return preview
  4. estimate_cost         — query cloud pricing for the planned resources (required before apply)
  5. apply_deployment       — terraform apply (executed by the server, not you)
  6. troubleshoot_deployment — diagnose errors if any step fails

The deployment DAG (dag.json) is the contract between steps: each step reads it from your input message, and your output must respect existing node ids. update_deployment re-runs specify_resources + generate_terraform_plan with user answers/adjustments. destroy_deployment, get_deployment_status, and list_deployments do not involve you. query_cloud is for read-only queries about existing resources/bills.

## Credentials
Cloud credentials (e.g. HW_ACCESS_KEY, HW_SECRET_KEY, HW_REGION) are injected by the server into the terraform subprocess environment. NEVER hardcode credentials in .tf files, NEVER ask for them, NEVER put them in variables or tfvars.

## Tools
- read / grep / ls — browse the workspace: skills/ (references, guides) and deployments/ (.tf files)
- http_request — send authenticated HTTP requests to cloud APIs (signing is automatic, do NOT pass credentials). Use ONLY for read-only queries (List/Show/Get APIs). NEVER call Create/Update/Delete/Post/Put APIs to create or modify cloud resources directly — resource provisioning is done through terraform, not through API calls.
- WebSearch / WebFetch — query official cloud docs and pricing pages
- load_skill / reload_skills — (query_cloud, specify_resources) dynamically load cloud-service skills on demand

## Skills
For propose/specify/generate/estimate/troubleshoot: the relevant skill guide (SKILL.md) is already loaded into your system prompt — you do not need to call any tool to load it. Use read/grep/ls to browse the skill's references/ directory for detailed examples and API definitions.
For query_cloud and specify_resources: use the load_skill tool to load the relevant cloud-service skill on demand (the skill catalog is in your system prompt). specify_resources loads per-service skills to find API endpoints for spec queries (e.g. load_skill on the compute service skill for flavor listings).

## Output contract
Return ONLY valid JSON as specified by each tool's instructions. Do not wrap in markdown fences. Do not add conversational text outside the JSON. The server parses your output programmatically — any non-JSON text will cause a parse failure.`

// ProposeArchitecture analyzes a deployment request and recommends a cloud
// architecture (step 1 of the 6-step deployment flow). It does NOT write .tf
// files or browse reference examples — it only looks at the service category
// list and matches the request to a known architecture pattern.
//
// Returns a deployment_id (pre-allocated), the proposed architecture, required
// services, and reasoning. If information is incomplete, returns questions.
// The user confirms the architecture before calling specify_resources.
func (p *Planner) ProposeArchitecture(ctx context.Context, request string) (string, error) {
	progress := openagent.ProgressFromContext(ctx)

	// Pre-allocate the deployment ID so all subsequent steps share the same
	// Memory session and deployment directory.
	depID, dir, err := deploymentID(p.deploymentsDir)
	if err != nil {
		return "", fmt.Errorf("propose_architecture: %w", err)
	}

	progress("Loading deployment skill...", 0, 2)
	skillBody := p.loadSkillBody(ctx, p.agentSkill(provider.RoleArchitect))
	cfg := agent.New("iac-architect",
		agent.WithModel(p.model),
		agent.WithSystemPrompts(
			serverContext,
			skillBody,
			p.agentPrompt(provider.RoleArchitect),
			`## Output contract
Return JSON:
{
  "architecture": "short name of the architecture",
  "services": ["...", "..."],
  "reasoning": "why this architecture was chosen",
  "questions": ["..."],  // only if information is incomplete
  "dag": {
    "nodes": [
      {"id": "...", "type": "...", "name": "...", "depends_on": []},
      {"id": "...", "type": "...", "name": "...", "depends_on": ["...", "..."]}
    ]
  }
}
- Node ids are short stable ids, unique across the DAG. type must be a terraform resource type of this cloud; name is the terraform resource label.
- If information is incomplete, return questions instead of the DAG.`),
		agent.WithMaxTurns(32),
	)
	rt := kernel.New(cfg, kernel.Deps{
		Tools:          p.fileTools(),
		SessionStore:   p.memory,
		Compressor:     ctxpkg.CompressorOf(p.memory),
		MemoryProvider: p.knowledge,
		Hooks:          openagent.MultiHooks(sloghooks.New(slog.Default()), newProgressHook()),
	})

	progress("Analyzing deployment request...", 1, 2)
	session := openagent.Session{ID: sessionID(depID)}
	result, err := rt.Run(ctx, session, openagent.UserMessage(request))
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("propose_architecture: LLM run: %w", err)
	}

	raw := extractJSON(result.FinalOutput)
	if raw == "" {
		os.RemoveAll(dir)
		return "", fmt.Errorf("propose_architecture: LLM returned empty output (FinalOutput=%q)", result.FinalOutput)
	}

	var arch struct {
		Architecture string   `json:"architecture"`
		Services     []string `json:"services"`
		Reasoning    string   `json:"reasoning"`
		Questions    []string `json:"questions"`
		Dag          Dag      `json:"dag"`
	}
	if err := json.Unmarshal([]byte(raw), &arch); err != nil {
		os.RemoveAll(dir)
		return marshalResult(planResult{
			Status: "need_input",
			Questions: []string{
				"Could not parse the request. Please provide more details about what you want to deploy, the region, and any requirements.",
			},
		})
	}

	// Information incomplete — ask the client for clarification.
	if len(arch.Questions) > 0 {
		os.RemoveAll(dir)
		return marshalResult(planResult{
			Status:    "need_input",
			Questions: arch.Questions,
		})
	}

	// Persist the DAG — it is the contract for every subsequent step.
	arch.Dag.Version = DagVersion
	arch.Dag.DeploymentID = depID
	arch.Dag.Architecture = arch.Architecture
	arch.Dag.Status = DagProposed
	if err := validateDag(&arch.Dag); err != nil {
		os.RemoveAll(dir)
		return marshalResult(planResult{
			Status: "need_input",
			Questions: []string{
				"Could not build a valid resource DAG from the request: " + err.Error(),
			},
		})
	}
	if err := saveDag(dir, &arch.Dag); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("propose_architecture: save dag: %w", err)
	}

	// Summarize nodes for the client LLM (id + type + name).
	nodes := make([]map[string]any, 0, len(arch.Dag.Nodes))
	for _, n := range arch.Dag.Nodes {
		nodes = append(nodes, map[string]any{
			"id":   n.ID,
			"type": n.Type,
			"name": n.Name,
		})
	}

	out := map[string]any{
		"deployment_id": depID,
		"architecture":  arch.Architecture,
		"services":      arch.Services,
		"nodes":         nodes,
		"reasoning":     arch.Reasoning,
		"next_step":     "Call specify_resources with this deployment_id to determine resource specs. User should confirm the architecture first.",
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("propose_architecture: marshal: %w", err)
	}
	return string(data), nil
}

// SpecifyResources determines concrete resource specs for a proposed
// architecture (step 2 of the 6-step deployment flow). It reads the DAG
// persisted by propose_architecture, queries cloud APIs for available specs
// (flavors, images, etc.) via dynamic skill loading + http_request, and
// returns the enriched DAG. If choices are too many or information is
// insufficient, it returns questions and the client re-calls with answers.
//
// The user confirms the resources before calling generate_terraform_plan.
func (p *Planner) SpecifyResources(ctx context.Context, deploymentID string, answers []string, adjustments string) (string, error) {
	progress := openagent.ProgressFromContext(ctx)

	dir := filepath.Join(p.deploymentsDir, deploymentID)
	dag, err := loadDag(dir)
	if err != nil {
		return "", fmt.Errorf("specify_resources: %w", err)
	}

	progress("Loading deployment skill...", 0, 3)
	skillBody := p.loadSkillBody(ctx, p.agentSkill(provider.RoleSpecifier))
	cfg := agent.New("iac-specifier",
		agent.WithModel(p.model),
		agent.WithSystemPrompts(
			serverContext,
			skillBody,
			p.agentPrompt(provider.RoleSpecifier),
			`## Output contract
Return JSON:
{
  "status": "ready",
  "resources": [
    {"id": "...", "type": "...", "name": "...", "spec": {"...": "..."}},
    {"id": "...", "type": "...", "name": "...", "spec": {"...": "..."}}
  ],
  "reasoning": "why these specs were chosen"
}

or, when you need more input:
{"status": "need_input", "questions": ["...", "..."]}

- Every node id from the input DAG must appear in "resources" with a filled spec. You may add new nodes with new ids.
`),
		agent.WithMaxTurns(32),
	)
	rt := kernel.New(cfg, kernel.Deps{
		Tools:          p.fileTools(),
		SessionStore:   p.memory,
		Compressor:     ctxpkg.CompressorOf(p.memory),
		MemoryProvider: p.knowledge,
		SkillProvider:  p.loader,
		Hooks:          openagent.MultiHooks(sloghooks.New(slog.Default()), newProgressHook()),
	})

	progress("Determining resource specs...", 1, 3)
	session := openagent.Session{ID: sessionID(deploymentID)}

	dagStr, err := dagInput(dag)
	if err != nil {
		return "", fmt.Errorf("specify_resources: %w", err)
	}
	userMsg := "Here is the deployment DAG (read-only contract — preserve node ids, fill specs for every node):\n\n" + dagStr + "\n\nDetermine concrete specs for every node. Return JSON with the resources array."
	if len(answers) > 0 {
		userMsg += "\n\nUser answers to your previous questions:\n- " + strings.Join(answers, "\n- ")
	}
	if adjustments != "" {
		userMsg += "\n\nUser adjustments: " + adjustments
	}

	result, err := rt.Run(ctx, session, openagent.UserMessage(userMsg))
	if err != nil {
		return "", fmt.Errorf("specify_resources: LLM run: %w", err)
	}

	raw := extractJSON(result.FinalOutput)
	if raw == "" {
		return "", fmt.Errorf("specify_resources: LLM returned empty output (FinalOutput=%q)", result.FinalOutput)
	}

	var spec struct {
		Status    string `json:"status"`
		Resources []struct {
			ID        string         `json:"id"`
			Type      string         `json:"type"`
			Name      string         `json:"name"`
			Spec      map[string]any `json:"spec"`
			DependsOn []string       `json:"depends_on"`
		} `json:"resources"`
		Questions []string `json:"questions"`
		Reasoning string   `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return "", fmt.Errorf("specify_resources: parse: %w (raw=%q)", err, raw)
	}

	// Need more input — ask the client, keep the DAG at last-good state.
	if spec.Status == "need_input" || (spec.Status == "" && len(spec.Questions) > 0) {
		progress("Done", 3, 3)
		return marshalResult(planResult{
			Status:    "need_input",
			Questions: spec.Questions,
		})
	}
	if len(spec.Resources) == 0 {
		return "", fmt.Errorf("specify_resources: no resources returned")
	}

	// Merge the LLM output back onto the DAG by node id.
	type llmNode struct {
		ID        string
		Type      string
		Name      string
		Spec      map[string]any
		DependsOn []string
	}
	byID := make(map[string]llmNode, len(spec.Resources))
	for _, r := range spec.Resources {
		if r.ID == "" {
			return "", fmt.Errorf("specify_resources: resource without id in LLM output")
		}
		byID[r.ID] = llmNode{ID: r.ID, Type: r.Type, Name: r.Name, Spec: r.Spec, DependsOn: r.DependsOn}
	}
	existing := make(map[string]bool, len(dag.Nodes))
	for i := range dag.Nodes {
		n := &dag.Nodes[i]
		existing[n.ID] = true
		r, ok := byID[n.ID]
		if !ok {
			return "", fmt.Errorf("specify_resources: LLM output missing node %q — every DAG node must get a spec", n.ID)
		}
		if r.Type != "" && r.Type != n.Type {
			return "", fmt.Errorf("specify_resources: node %q type changed %q -> %q — keep the DAG contract", n.ID, n.Type, r.Type)
		}
		if r.Name != "" && r.Name != n.Name {
			return "", fmt.Errorf("specify_resources: node %q name changed %q -> %q — keep the DAG contract", n.ID, n.Name, r.Name)
		}
		if len(r.Spec) == 0 {
			return "", fmt.Errorf("specify_resources: node %q has no spec — every DAG node must get a concrete spec", n.ID)
		}
		n.Spec = r.Spec
	}
	// New nodes the LLM added (with new ids) are appended.
	for _, r := range byID {
		if existing[r.ID] {
			continue
		}
		if r.Type == "" || r.Name == "" {
			return "", fmt.Errorf("specify_resources: new node %q missing type or name", r.ID)
		}
		dag.Nodes = append(dag.Nodes, DagNode{
			ID:        r.ID,
			Type:      r.Type,
			Name:      r.Name,
			Spec:      r.Spec,
			DependsOn: r.DependsOn,
		})
	}
	dag.Status = DagSpecified
	if err := saveDag(dir, dag); err != nil {
		return "", fmt.Errorf("specify_resources: save dag: %w", err)
	}
	if err := invalidateCost(dir); err != nil {
		return "", fmt.Errorf("specify_resources: %w", err)
	}

	resources := make([]map[string]any, 0, len(dag.Nodes))
	for _, n := range dag.Nodes {
		resources = append(resources, map[string]any{
			"id":   n.ID,
			"type": n.Type,
			"name": n.Name,
			"spec": n.Spec,
		})
	}
	out := map[string]any{
		"deployment_id": deploymentID,
		"resources":     resources,
		"reasoning":     spec.Reasoning,
		"next_step":     "Call generate_terraform_plan with this deployment_id to write .tf files and run terraform plan. User should confirm the resources first.",
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("specify_resources: marshal: %w", err)
	}
	progress("Done", 3, 3)
	return string(data), nil
}

// GenerateTerraformPlan writes .tf files and runs terraform plan (step 3 of
// the 6-step deployment flow). Reads the fully-specified DAG persisted by
// specify_resources, browses ONLY the relevant reference examples, generates
// .tf files, and runs terraform init + plan. Retries up to 3 times on plan
// failure. On success marks the DAG as planned and invalidates any previous
// cost estimate.
//
// The user reviews the plan preview before calling estimate_cost.
func (p *Planner) GenerateTerraformPlan(ctx context.Context, deploymentID string) (string, error) {
	progress := openagent.ProgressFromContext(ctx)

	dir := filepath.Join(p.deploymentsDir, deploymentID)
	dag, err := loadDag(dir)
	if err != nil {
		return "", fmt.Errorf("generate_terraform_plan: %w", err)
	}
	if !nodeSpecsFilled(dag) {
		return "", fmt.Errorf("generate_terraform_plan: deployment %s is not fully specified — call specify_resources first", deploymentID)
	}

	progress("Loading deployment skill...", 0, 4)
	skillBody := p.loadSkillBody(ctx, p.agentSkill(provider.RolePlanner))
	cfg := agent.New("iac-planner",
		agent.WithModel(p.model),
		agent.WithSystemPrompts(
			serverContext,
			skillBody,
			p.agentPrompt(provider.RolePlanner),
			`## Output contract
Return JSON:
{
  "files": {
    "providers.tf": "...",
    "variables.tf": "...",
    "main.tf": "...",
    "terraform.tfvars": "..."
  },
  "reasoning": "why these .tf configs were generated"
}

- One resource block per DAG node (address = type.name), dependencies wired per the DAG depends_on edges (references or depends_on). Do NOT deviate from the DAG.
`),
		agent.WithMaxTurns(32),
	)
	rt := kernel.New(cfg, kernel.Deps{
		Tools:          p.fileTools(),
		SessionStore:   p.memory,
		Compressor:     ctxpkg.CompressorOf(p.memory),
		MemoryProvider: p.knowledge,
		Hooks:          openagent.MultiHooks(sloghooks.New(slog.Default()), newProgressHook()),
	})

	session := openagent.Session{ID: sessionID(deploymentID)}
	dagStr, err := dagInput(dag)
	if err != nil {
		return "", fmt.Errorf("generate_terraform_plan: %w", err)
	}
	msg := openagent.UserMessage("Generate terraform .tf files from this deployment DAG (one resource block per node, address = type.name, dependencies per depends_on):\n\n" + dagStr)

	var reasoning string
	for attempt := 0; attempt < 3; attempt++ {
		progress(fmt.Sprintf("Generating .tf files (attempt %d/3)...", attempt+1), float64(attempt), 3)
		result, err := rt.Run(ctx, session, msg)
		if err != nil {
			return "", fmt.Errorf("generate_terraform_plan: LLM run (attempt %d): %w", attempt+1, err)
		}

		var llmOutput struct {
			Files     map[string]string `json:"files"`
			Reasoning string            `json:"reasoning"`
		}
		raw := extractJSON(result.FinalOutput)
		if raw == "" {
			return "", fmt.Errorf("generate_terraform_plan: LLM returned empty output (attempt %d, FinalOutput=%q)", attempt+1, result.FinalOutput)
		}
		if err := json.Unmarshal([]byte(raw), &llmOutput); err != nil {
			return "", fmt.Errorf("generate_terraform_plan: parse (attempt %d): %w (raw=%q)", attempt+1, err, raw)
		}

		if len(llmOutput.Files) == 0 {
			return "", fmt.Errorf("generate_terraform_plan: no files generated (attempt %d)", attempt+1)
		}

		reasoning = llmOutput.Reasoning

		// Write .tf files to the deployment directory.
		for name, content := range llmOutput.Files {
			if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
				return "", fmt.Errorf("generate_terraform_plan: invalid filename %q", name)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				return "", fmt.Errorf("generate_terraform_plan: write %s: %w", name, err)
			}
		}

		// terraform init + plan.
		progress("Running terraform init...", 2, 4)
		client, err := iac.NewClient(ctx, dir, iac.Config{
			Env:             p.cloud.Env(),
			DryRun:          p.dryRun,
			BinaryMirrors:   p.binaryMirrors,
			ProviderMirrors: p.providerMirrors,
		})
		if err != nil {
			return "", fmt.Errorf("generate_terraform_plan: create terraform client: %w", err)
		}
		if err := client.Init(ctx); err != nil {
			msg = retryMessage("generate .tf files", "terraform init", err, p.workDir, dir)
			continue
		}
		progress("Running terraform plan...", 3, 4)
		plan, err := client.Plan(ctx)
		if err == nil {
			dag.Status = DagPlanned
			if err := saveDag(dir, dag); err != nil {
				return "", fmt.Errorf("generate_terraform_plan: save dag: %w", err)
			}
			if err := invalidateCost(dir); err != nil {
				return "", fmt.Errorf("generate_terraform_plan: %w", err)
			}
			out := map[string]any{
				"deployment_id":  deploymentID,
				"files":          llmOutput.Files,
				"plan_preview":   plan,
				"resource_count": len(llmOutput.Files),
				"reasoning":      reasoning,
				"next_step":      "Call estimate_cost with this deployment_id to get pricing. User should review the plan first.",
			}
			data, err := json.Marshal(out)
			if err != nil {
				return "", fmt.Errorf("generate_terraform_plan: marshal: %w", err)
			}
			return string(data), nil
		}
		msg = retryMessage("generate .tf files", "terraform plan", err, p.workDir, dir)
	}

	return "", fmt.Errorf("generate_terraform_plan: terraform plan failed after 3 attempts")
}

// UpdateDeployment modifies an existing deployment by re-running specify_resources
// (with user answers/adjustments) + generate_terraform_plan. The deployment
// directory is reused; both steps invalidate any previous cost estimate, so
// apply requires a fresh estimate_cost.
//
// Use this when the user wants to adjust an already-planned deployment
// (e.g. change a flavor, rename a resource, add a tag) without starting
// from scratch. The change_request is passed as adjustments to specify_resources.
func (p *Planner) UpdateDeployment(ctx context.Context, deploymentID string, answers []string, changeRequest string) (string, error) {
	// Re-specify resources with the user's answers/adjustments, then regenerate the plan.
	if _, err := p.SpecifyResources(ctx, deploymentID, answers, changeRequest); err != nil {
		return "", fmt.Errorf("update_deployment: specify_resources: %w", err)
	}
	return p.GenerateTerraformPlan(ctx, deploymentID)
}

// retryMessage builds the user message for a plan retry attempt.
// workDir is the read/grep/ls workspace root; dir is the deployment
// directory. The LLM is told a path relative to workDir so read/grep/ls
// resolve correctly and we don't leak the server's absolute path.
func retryMessage(request, command string, planErr error, workDir, dir string) openagent.Message {
	tfFiles, _ := readTFFiles(dir)
	relDir, _ := filepath.Rel(workDir, dir)
	return openagent.UserMessage(fmt.Sprintf(`Original request: %s

%s failed with this error:

%s

The current .tf files are in directory: %s

%s

Fix the .tf files and return the corrected versions as JSON:
{"files": {"providers.tf": "...", "variables.tf": "...", "main.tf": "...", "terraform.tfvars": "..."}, "reasoning": "..."}`,
		request, command, planErr.Error(), relDir, tfFiles))
}

// EstimateCost prices a planned deployment from its DAG (step 4 of the
// 6-step deployment flow). The LLM loads the pricing skill and uses
// http_request (BSS API, auto-signed) to query prices, with WebSearch/WebFetch
// as a fallback for public pricing pages. pricingMode ("on-demand" or
// "monthly") comes from the user's stated preference; "" defaults to
// on-demand. The result is persisted to cost.json and the DAG is marked
// cost_estimated — apply_deployment gates on the marker, so this MUST be
// called (again after any change) before apply.
func (p *Planner) EstimateCost(ctx context.Context, deploymentID, pricingMode string) (string, error) {
	progress := openagent.ProgressFromContext(ctx)

	dir := filepath.Join(p.deploymentsDir, deploymentID)

	// The DAG carries the exact specs the user confirmed — it is the pricing
	// contract (the tfplan After field is noisy and absent in dry-run).
	dag, err := loadDag(dir)
	if err != nil {
		return "", fmt.Errorf("estimate_cost: %w", err)
	}
	if !nodeSpecsFilled(dag) {
		return "", fmt.Errorf("estimate_cost: deployment %s is not fully specified — call specify_resources first", deploymentID)
	}

	// Normalize the billing mode: "" defaults to on-demand.
	mode := pricingMode
	if mode == "" {
		mode = "on-demand"
	}
	if mode != "on-demand" && mode != "monthly" {
		return "", fmt.Errorf(`estimate_cost: unsupported pricing_mode %q (expected "on-demand" or "monthly")`, pricingMode)
	}

	progress("Loading pricing skill...", 0, 3)
	skillBody := p.loadSkillBody(ctx, p.agentSkill(provider.RolePricer))
	cfg := agent.New("iac-pricing",
		agent.WithModel(p.model),
		agent.WithSystemPrompts(
			serverContext,
			skillBody,
			p.agentPrompt(provider.RolePricer),
			`## Output contract
You are given the planned resources with exact specs from the deployment DAG.
Return JSON:
{
  "items": [
    {
      "resource": "...", 
	  "spec": "...",
	  "price": price or null}
  ], 
  "total": ... or null, 
  "currency": "CNY", 
  "note": "..."
}
`),
		agent.WithMaxTurns(32),
	)

	rt := kernel.New(cfg, kernel.Deps{
		Tools:          p.fileTools(),
		SessionStore:   p.memory,
		Compressor:     ctxpkg.CompressorOf(p.memory),
		MemoryProvider: p.knowledge,
		Hooks:          openagent.MultiHooks(sloghooks.New(slog.Default()), newProgressHook()),
	})

	dagStr, err := dagInput(dag)
	if err != nil {
		return "", fmt.Errorf("estimate_cost: %w", err)
	}
	userMsg := "Billing mode: " + mode + ". Query the prices for these planned resources (with exact specs):\n\n" + dagStr

	session := openagent.Session{ID: sessionID(deploymentID)}
	progress("Querying cloud pricing...", 1, 3)
	result, err := rt.Run(ctx, session, openagent.UserMessage(userMsg))
	if err != nil {
		return "", fmt.Errorf("estimate_cost: LLM run: %w", err)
	}

	// Parse the LLM output and persist the estimate marker.
	raw := extractJSON(result.FinalOutput)
	var cost struct {
		Items    []any  `json:"items"`
		Total    any    `json:"total"`
		Currency string `json:"currency"`
		Note     string `json:"note"`
	}
	if json.Unmarshal([]byte(raw), &cost) != nil {
		cost.Note = result.FinalOutput
	}
	est := &CostEstimate{
		Version:      CostVersion,
		DeploymentID: deploymentID,
		PricingMode:  mode,
		EstimatedAt:  time.Now().UTC().Format(time.RFC3339),
		Items:        cost.Items,
		TotalMonthly: cost.Total,
		Currency:     cost.Currency,
		Note:         cost.Note,
	}
	if err := saveCost(dir, est); err != nil {
		return "", fmt.Errorf("estimate_cost: %w", err)
	}
	dag.Status = DagCostEstimated
	if err := saveDag(dir, dag); err != nil {
		return "", fmt.Errorf("estimate_cost: save dag: %w", err)
	}
	progress("Done", 2, 3)

	out := map[string]any{
		"deployment_id": deploymentID,
		"pricing_mode":  mode,
		"items":         cost.Items,
		"total_monthly": cost.Total,
		"currency":      cost.Currency,
		"note":          cost.Note,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("estimate_cost: marshal: %w", err)
	}
	return string(data), nil
}

// Troubleshoot diagnoses a deployment error and suggests fixes.
//
// The LLM loads the troubleshoot skill, browses examples for correct
// patterns, and searches the web for error solutions.
func (p *Planner) Troubleshoot(ctx context.Context, deploymentID, errorMsg string) (string, error) {
	progress := openagent.ProgressFromContext(ctx)

	dir := filepath.Join(p.deploymentsDir, deploymentID)

	tfFiles, err := readTFFiles(dir)
	if err != nil {
		return "", fmt.Errorf("troubleshoot: read .tf: %w", err)
	}

	skillBody := p.loadSkillBody(ctx, p.agentSkill(provider.RoleTroubleshooter))
	progress("Loading troubleshoot skill...", 0, 2)
	cfg := agent.New("iac-troubleshooter",
		agent.WithModel(p.model),
		agent.WithSystemPrompts(
			serverContext,
			skillBody,
			p.agentPrompt(provider.RoleTroubleshooter),
			`## Output contract
Return JSON:
{
  "diagnosis": "...",
  "suggestion": "...",
  "alternatives": ["...", ...]
}
`),
		agent.WithMaxTurns(32),
	)
	rt := kernel.New(cfg, kernel.Deps{
		Tools:          p.fileTools(),
		SessionStore:   p.memory,
		Compressor:     ctxpkg.CompressorOf(p.memory),
		MemoryProvider: p.knowledge,
		Hooks:          openagent.MultiHooks(sloghooks.New(slog.Default()), newProgressHook()),
	})

	// user message = dynamic content (error + .tf file path + .tf files)
	// Include the deployment directory path (relative to workDir) so the
	// LLM can use read/grep/ls to inspect the .tf files itself.
	relDir, _ := filepath.Rel(p.workDir, dir)
	userMsg := fmt.Sprintf("A deployment on %s failed with this error:\n\n%s\n\n"+
		"The terraform files are in directory: %s\n\n%s\n\n"+
		"You can use read/grep/ls with the path above to inspect the files. "+
		"Diagnose the error and suggest fixes.",
		p.cloud.Name(), errorMsg, relDir, tfFiles)

	session := openagent.Session{ID: sessionID(deploymentID)}
	progress("Diagnosing error...", 1, 2)
	result, err := rt.Run(ctx, session, openagent.UserMessage(userMsg))
	if err != nil {
		return "", fmt.Errorf("troubleshoot: LLM run: %w", err)
	}

	// Parse the LLM output into a structured diagnosis.
	raw := extractJSON(result.FinalOutput)
	var diag struct {
		Diagnosis    string   `json:"diagnosis"`
		Suggestion   string   `json:"suggestion"`
		Alternatives []string `json:"alternatives"`
	}
	if json.Unmarshal([]byte(raw), &diag) != nil || (diag.Diagnosis == "" && diag.Suggestion == "" && len(diag.Alternatives) == 0) {
		diag.Diagnosis = result.FinalOutput
	}
	data, err := json.Marshal(diag)
	if err != nil {
		return "", fmt.Errorf("troubleshoot: marshal: %w", err)
	}
	return string(data), nil
}

// QueryCloud answers read-only queries about existing cloud resources, specs,
// bills, or quotas. Unlike the other 4 agents, this one uses dynamic skill
// loading (SkillProvider) — the LLM sees the skill catalog and calls
// load_skill to load the relevant cloud-service skill on demand.
func (p *Planner) QueryCloud(ctx context.Context, query string) (string, error) {
	progress := openagent.ProgressFromContext(ctx)

	progress("Setting up query agent...", 0, 2)
	cfg := agent.New("iac-queryer",
		agent.WithModel(p.model),
		agent.WithSystemPrompts(
			serverContext,
			p.agentPrompt(provider.RoleQueryer),
			`## Output contract
Return JSON:
{
  "results": [...],
  "note": "..."
}
`),
		agent.WithMaxTurns(32),
	)
	rt := kernel.New(cfg, kernel.Deps{
		Tools:          p.fileTools(),
		SessionStore:   p.memory,
		Compressor:     ctxpkg.CompressorOf(p.memory),
		MemoryProvider: p.knowledge,
		SkillProvider:  p.loader,
		Hooks:          openagent.MultiHooks(sloghooks.New(slog.Default()), newProgressHook()),
	})

	session := openagent.Session{ID: "query"}
	progress("Querying cloud resources...", 1, 2)
	result, err := rt.Run(ctx, session, openagent.UserMessage(query))
	if err != nil {
		return "", fmt.Errorf("query_cloud: LLM run: %w", err)
	}

	// Parse the LLM output. If it's already valid JSON, pass through.
	raw := extractJSON(result.FinalOutput)
	var qc struct {
		Results []any  `json:"results"`
		Note    string `json:"note"`
	}
	if json.Unmarshal([]byte(raw), &qc) != nil {
		qc.Note = result.FinalOutput
	}
	out := map[string]any{
		"results": qc.Results,
		"note":    qc.Note,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("query_cloud: marshal: %w", err)
	}
	return string(data), nil
}

// agentPrompt returns the cloud-specific expert prompt for a role
// ("" when the cloud provides none for this role).
func (p *Planner) agentPrompt(role provider.PromptRole) string {
	if cfg, ok := p.prompts[role]; ok {
		return cfg.Prompt
	}
	return ""
}

// agentSkill returns the static skill name for a role ("" = the role
// loads skills dynamically via load_skill).
func (p *Planner) agentSkill(role provider.PromptRole) string {
	if cfg, ok := p.prompts[role]; ok {
		return cfg.SkillName
	}
	return ""
}

// loadSkillBody statically loads a skill's SKILL.md body by name.
// The body is injected directly into the agent's system prompt instead of
// relying on the LLM to call load_skill at runtime — this is deterministic,
// saves a tool-call round-trip, and avoids injecting the skill catalog +
// load_skill/reload_skills tool definitions.
func (p *Planner) loadSkillBody(ctx context.Context, name string) string {
	skills, err := p.loader.Discover(ctx)
	if err != nil {
		return ""
	}
	for _, s := range skills {
		if s.Name == name {
			body, err := p.loader.Load(ctx, s)
			if err != nil {
				return ""
			}
			return body
		}
	}
	return ""
}

// fileTools returns the standard file + web tools for all LLM agents.
// read/grep/ls operate with workDir = cloud home so the LLM can browse
// both skills/ (references, guides) and deployments/ (.tf files).
// If the cloud provider exposes an http_request tool (signed requests to
// cloud APIs), it is included for calling cloud APIs.
func (p *Planner) fileTools() []openagent.Tool {
	tools := []openagent.Tool{
		opentool.NewReadFile(p.workDir),
		opentool.NewGrep(p.workDir),
		opentool.NewListDir(p.workDir),
		opentool.NewWebSearch(),
		opentool.NewWebFetch(),
	}
	if ht, ok := p.cloud.(interface{ HTTPRequest() openagent.Tool }); ok {
		tools = append(tools, ht.HTTPRequest())
	}
	return tools
}

// readTFFiles reads all .tf files in a directory and returns them as a string.
func readTFFiles(dir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		b.WriteString(fmt.Sprintf("--- %s ---\n%s\n\n", filepath.Base(f), data))
	}
	return b.String(), nil
}

// backupTFFiles reads all .tf and .tfvars files in a directory into a map
// of filename → content. Used by UpdateDeployment to restore on failure.
func backupTFFiles(dir string) (map[string]string, error) {
	patterns := []string{filepath.Join(dir, "*.tf"), filepath.Join(dir, "*.tfvars")}
	backup := make(map[string]string)
	for _, pattern := range patterns {
		files, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			backup[filepath.Base(f)] = string(data)
		}
	}
	return backup, nil
}

// restoreTFFiles writes backed-up files back to a directory.
func restoreTFFiles(dir string, backup map[string]string) {
	for name, content := range backup {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	}
}

// extractJSON finds the first JSON object in a string (LLM output may have
// surrounding text or markdown fences).
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end <= start {
		return s
	}
	return s[start : end+1]
}

// marshalResult marshals a planResult to JSON string.
func marshalResult(r planResult) (string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(data), nil
}

// deploymentID allocates a unique deployment ID by atomically creating its
// directory. Race-safe: two concurrent callers cannot get the same ID.
func deploymentID(deploymentsDir string) (string, string, error) {
	entries, _ := os.ReadDir(deploymentsDir)
	maxNum := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "d-") {
			var num int
			fmt.Sscanf(name, "d-%d", &num)
			if num > maxNum {
				maxNum = num
			}
		}
	}
	for n := maxNum + 1; n < maxNum+1000; n++ {
		id := fmt.Sprintf("d-%03d", n)
		dir := filepath.Join(deploymentsDir, id)
		if err := os.Mkdir(dir, 0755); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", "", fmt.Errorf("create deployment dir: %w", err)
		}
		return id, dir, nil
	}
	return "", "", fmt.Errorf("no free deployment ID found under %s", deploymentsDir)
}
