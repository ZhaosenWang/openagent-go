// Package agent provides server-side LLM reasoning for iac-server.
//
// The Planner uses a separate LLM (configured via LLM_* env vars) to:
//   - Read embedded terraform examples and generate .tf files for a request
//   - Query cloud pricing via the BSS API and web search
//   - Diagnose deployment errors and suggest fixes
//
// Skills (SKILL.md guides) are statically loaded via the SkillLoader and
// injected directly into each agent's system prompt — no runtime load_skill
// tool call needed. The LLM browses reference files (examples, API swagger)
// with standard read/grep/ls tools.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
	sloghooks "github.com/yusheng-g/openagent-go/hooks/slog"
	"github.com/yusheng-g/openagent-go/iac"
	opentool "github.com/yusheng-g/openagent-go/tool"
)

// Planner holds the dependencies for server-side LLM reasoning.
type Planner struct {
	model           openagent.Model
	cloud           provider.CloudProvider
	loader          openagent.SkillLoader // loads skills from extracted skills dir
	memory          openagent.Memory     // shared across calls, scoped by deployment_id
	workDir         string               // cloud home dir (parent of skills/ and deployments/), workDir for read/grep/ls
	deploymentsDir  string
	dryRun          bool
	binaryMirrors   []string // terraform binary download mirrors
	providerMirrors []string // provider download mirrors
}

// New creates a Planner. workDir should be the cloud home directory
// (parent of skills/ and deployments/) so read/grep/ls can access both.
// memory is shared across all LLM calls and scoped by deployment_id —
// estimate_cost can see plan_deployment's reasoning, troubleshoot can see
// prior attempts. nil disables memory (each call is isolated).
func New(model openagent.Model, cloud provider.CloudProvider, loader openagent.SkillLoader, memory openagent.Memory, workDir, deploymentsDir string, dryRun bool, binaryMirrors, providerMirrors []string) *Planner {
	return &Planner{
		model:           model,
		cloud:           cloud,
		loader:          loader,
		memory:          memory,
		workDir:         workDir,
		deploymentsDir:  deploymentsDir,
		dryRun:          dryRun,
		binaryMirrors:   binaryMirrors,
		providerMirrors: providerMirrors,
	}
}

// sessionID returns the Memory session key for a deployment.
func sessionID(deploymentID string) string {
	return "dep-" + deploymentID
}

// planResult is the JSON returned by plan_deployment.
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
- The MCP CLIENT (e.g. Claude Code, Cursor, openagent) calls one of the 9 MCP tools (plan_deployment, update_deployment, estimate_cost, apply_deployment, destroy_deployment, get_deployment_status, list_deployments, troubleshoot_deployment, query_cloud) and forwards the user's request to you.
- Your output is returned to the client as the tool result. The client then decides what to show the user and whether to proceed.
- You do NOT need user approval for any action — approval is the client's concern, not yours.

## Workflow
The typical deployment flow is:
  1. plan_deployment    — you generate .tf files and run terraform plan
  2. estimate_cost      — you query cloud pricing for the planned resources
  3. apply_deployment    — terraform apply (executed by the server, not you)
  4. (troubleshoot_deployment if anything fails)

update_deployment modifies an existing planned deployment in-place. destroy_deployment and get_deployment_status do not involve you.

## Credentials
Cloud credentials (e.g. HW_ACCESS_KEY, HW_SECRET_KEY, HW_REGION) are injected by the server into the terraform subprocess environment. NEVER hardcode credentials in .tf files, NEVER ask for them, NEVER put them in variables or tfvars.

## Tools
- read / grep / ls — browse the workspace: skills/ (references, guides) and deployments/ (.tf files)
- http_request — send authenticated HTTP requests to cloud APIs (signing is automatic, do NOT pass credentials). Use ONLY for read-only queries (List/Show/Get APIs). NEVER call Create/Update/Delete/Post/Put APIs to create or modify cloud resources directly — resource provisioning is done through terraform (plan_deployment → apply_deployment), not through API calls.
- WebSearch / WebFetch — query official cloud docs and pricing pages
- load_skill / reload_skills — (query_cloud only) dynamically load cloud-service skills on demand

## Skills
For plan/update/estimate/troubleshoot: the relevant skill guide (SKILL.md) is already loaded into your system prompt — you do not need to call any tool to load it. Use read/grep/ls to browse the skill's references/ directory for detailed examples and API definitions.
For query_cloud: use the load_skill tool to load the relevant cloud-service skill on demand (the skill catalog is in your system prompt).

## Output contract
Return ONLY valid JSON as specified by each tool's instructions. Do not wrap in markdown fences. Do not add conversational text outside the JSON. The server parses your output programmatically — any non-JSON text will cause a parse failure.`

// Plan analyzes a free-text deployment request and produces a terraform plan.
//
// The huaweicloud-deploy skill is statically loaded into the system prompt.
// The LLM browses references with read/grep/ls and generates .tf files. If
// information is incomplete, returns need_input with questions. If complete,
// writes the .tf files, runs terraform init+plan, and retries up to 3 times
// on failure.
func (p *Planner) Plan(ctx context.Context, request string) (string, error) {
	skillBody := p.loadSkillBody(ctx, "huaweicloud-deploy")
	agent := openagent.NewAgent("iac-planner",
		openagent.WithModel(p.model),
		openagent.WithTools(p.fileTools()...),
		openagent.WithMemory(p.memory),
		openagent.WithRunHooks(sloghooks.New(slog.Default())),
		openagent.WithSystemPrompts(
			serverContext,
			skillBody,
			"You are a HuaweiCloud infrastructure deployment expert. "+
				"Use read/grep/ls to browse the skills/huaweicloud-deploy/references/ directory "+
				"and generate .tf configuration files that match the user's request. "+
				"If information is incomplete, return {\"questions\": [...]} listing what is missing. "+
				"If complete, return {\"files\": {\"providers.tf\": \"...\", ...}, \"reasoning\": \"...\"}."),
		openagent.WithMaxTurns(10),
	)

	// Pre-allocate the deployment ID so all retry attempts share the same
	// Memory session (session.ID = "dep-<depID>"). If the LLM returns
	// need_input or no files, the empty directory is cleaned up below.
	depID, dir, err := deploymentID(p.deploymentsDir)
	if err != nil {
		return "", fmt.Errorf("plan: %w", err)
	}
	session := openagent.Session{ID: sessionID(depID)}

	msg := openagent.UserMessage(request)
	var reasoning string
	for attempt := 0; attempt < 3; attempt++ {
		result, err := agent.Run(ctx, session, msg)
		if err != nil {
			return "", fmt.Errorf("plan: LLM run (attempt %d): %w", attempt+1, err)
		}

		// Parse LLM output: either {questions: [...]} or {files: {...}, reasoning: "..."}.
		var llmOutput struct {
			Questions []string          `json:"questions"`
			Files     map[string]string `json:"files"`
			Reasoning string            `json:"reasoning"`
		}
		if err := json.Unmarshal([]byte(extractJSON(result.FinalOutput)), &llmOutput); err != nil {
			return marshalResult(planResult{
				Status: "need_input",
				Questions: []string{
					"Could not parse the request. Please provide more details about what you want to deploy, the region, and any requirements.",
				},
			})
		}

		// Information incomplete — ask the client for clarification.
		if len(llmOutput.Questions) > 0 {
			os.RemoveAll(dir) // clean up the pre-allocated empty directory
			return marshalResult(planResult{
				Status:    "need_input",
				Questions: llmOutput.Questions,
			})
		}

		// No files generated — bail out.
		if len(llmOutput.Files) == 0 {
			os.RemoveAll(dir)
			return marshalResult(planResult{
				Status: "need_input",
				Questions: []string{
					"No terraform files were generated. Please specify what you want to deploy.",
				},
			})
		}

		reasoning = llmOutput.Reasoning

		// Write .tf files to the deployment directory.
		for name, content := range llmOutput.Files {
			// Sanitize filename — LLM output is untrusted. Reject path
			// separators and parent-dir components to prevent escaping dir.
			if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
				return "", fmt.Errorf("plan: invalid filename %q", name)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				return "", fmt.Errorf("plan: write %s: %w", name, err)
			}
		}

		// terraform init + plan.
		client, err := iac.NewClient(ctx, dir, iac.Config{
			Env:             p.cloud.Env(),
			DryRun:          p.dryRun,
			BinaryMirrors:   p.binaryMirrors,
			ProviderMirrors: p.providerMirrors,
		})
		if err != nil {
			return "", fmt.Errorf("plan: create terraform client: %w", err)
		}
		if err := client.Init(ctx); err != nil {
			msg = retryMessage(request, "terraform init", err, p.workDir, dir)
			continue
		}
		plan, err := client.Plan(ctx)
		if err == nil {
			return marshalResult(planResult{
				Status:         "ready",
				DeploymentID:   depID,
				Plan:           plan,
				Recommendation: reasoning,
			})
		}
		msg = retryMessage(request, "terraform plan", err, p.workDir, dir)
	}

	// Exhausted retries — clean up the partial deployment directory.
	os.RemoveAll(dir)
	return "", fmt.Errorf("plan: terraform plan failed after 3 attempts")
}

// UpdateDeployment modifies an existing deployment's .tf files based on a
// change request, then re-runs terraform plan. The deployment directory
// is reused — no new deployment ID is created.
//
// Use this when the user wants to adjust an already-planned deployment
// (e.g. change a flavor, rename a resource, add a tag) without starting
// from scratch.
func (p *Planner) UpdateDeployment(ctx context.Context, deploymentID, changeRequest string) (string, error) {
	dir := filepath.Join(p.deploymentsDir, deploymentID)

	// Check deployment directory exists.
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("update_deployment: deployment %s not found", deploymentID)
	}

	// Read existing .tf files.
	tfFiles, _ := readTFFiles(dir)

	// Backup existing .tf files so we can restore on failure.
	backup, err := backupTFFiles(dir)
	if err != nil {
		return "", fmt.Errorf("update_deployment: backup: %w", err)
	}

	skillBody := p.loadSkillBody(ctx, "huaweicloud-deploy")
	agent := openagent.NewAgent("iac-updater",
		openagent.WithModel(p.model),
		openagent.WithTools(p.fileTools()...),
		openagent.WithMemory(p.memory),
		openagent.WithRunHooks(sloghooks.New(slog.Default())),
		openagent.WithSystemPrompts(
			serverContext,
			skillBody,
			"You are a HuaweiCloud infrastructure deployment expert. "+
				"Modify existing terraform configuration files based on the change request. "+
				"Return the COMPLETE modified files, not just diffs. "+
				"Return {\"files\": {\"providers.tf\": \"...\", ...}, \"reasoning\": \"...\"}."),
		openagent.WithMaxTurns(10),
	)

	// user message = change request + existing .tf files
	// Use a path relative to workDir so read/grep/ls resolve correctly
	// and we don't leak the server's absolute path to the LLM.
	relDir, _ := filepath.Rel(p.workDir, dir)
	msg := openagent.UserMessage(fmt.Sprintf(
		"Change request: %s\n\nCurrent .tf files (directory: %s):\n\n%s\n\n"+
			"Modify the .tf files according to the change request, return the complete files as JSON:\n"+
			`{"files": {"providers.tf": "...", "variables.tf": "...", "main.tf": "...", "terraform.tfvars": "..."}, "reasoning": "..."}`,
		changeRequest, relDir, tfFiles))

	session := openagent.Session{ID: sessionID(deploymentID)}
	var reasoning string
	for attempt := 0; attempt < 3; attempt++ {
		result, err := agent.Run(ctx, session, msg)
		if err != nil {
			return "", fmt.Errorf("update_deployment: LLM run (attempt %d): %w", attempt+1, err)
		}

		var llmOutput struct {
			Questions []string          `json:"questions"`
			Files     map[string]string `json:"files"`
			Reasoning string            `json:"reasoning"`
		}
		if err := json.Unmarshal([]byte(extractJSON(result.FinalOutput)), &llmOutput); err != nil {
			return marshalResult(planResult{
				Status: "need_input",
				Questions: []string{
					"Could not parse the change request. Please describe what you want to change.",
				},
			})
		}

		if len(llmOutput.Questions) > 0 {
			return marshalResult(planResult{
				Status:    "need_input",
				Questions: llmOutput.Questions,
			})
		}

		if len(llmOutput.Files) == 0 {
			return marshalResult(planResult{
				Status: "need_input",
				Questions: []string{
					"No terraform files were generated. Please describe what you want to change.",
				},
			})
		}

		reasoning = llmOutput.Reasoning

		// Write modified .tf files (overwrite existing).
		for name, content := range llmOutput.Files {
			if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
				return "", fmt.Errorf("update_deployment: invalid filename %q", name)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				return "", fmt.Errorf("update_deployment: write %s: %w", name, err)
			}
		}

		// terraform init + plan.
		client, err := iac.NewClient(ctx, dir, iac.Config{
			Env:             p.cloud.Env(),
			DryRun:          p.dryRun,
			BinaryMirrors:   p.binaryMirrors,
			ProviderMirrors: p.providerMirrors,
		})
		if err != nil {
			return "", fmt.Errorf("update_deployment: create terraform client: %w", err)
		}
		if err := client.Init(ctx); err != nil {
			msg = retryMessage(changeRequest, "terraform init", err, p.workDir, dir)
			continue
		}
		plan, err := client.Plan(ctx)
		if err == nil {
			return marshalResult(planResult{
				Status:         "ready",
				DeploymentID:   deploymentID,
				Plan:           plan,
				Recommendation: reasoning,
			})
		}
		msg = retryMessage(changeRequest, "terraform plan", err, p.workDir, dir)
	}

	// Exhausted retries — restore original .tf files so the deployment
	// is not left in a broken state.
	restoreTFFiles(dir, backup)
	return "", fmt.Errorf("update_deployment: terraform plan failed after 3 attempts (deployment %s, original .tf files restored)", deploymentID)
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

// EstimateCost reads the saved terraform plan for a deployment and queries
// cloud pricing via the LLM. The LLM loads the pricing skill and uses
// http_request (BSS API, auto-signed) to query prices, with WebSearch/WebFetch
// as a fallback for public pricing pages. This MUST be called before
// apply_deployment so the user sees the cost.
func (p *Planner) EstimateCost(ctx context.Context, deploymentID string) (string, error) {
	dir := filepath.Join(p.deploymentsDir, deploymentID)

	// Check that a plan exists — ShowPlan needs the tfplan file.
	planPath := filepath.Join(dir, "tfplan")
	if _, err := os.Stat(planPath); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("estimate_cost: no plan found for deployment %s — call plan_deployment first", deploymentID)
		}
		return "", fmt.Errorf("estimate_cost: check plan: %w", err)
	}

	// Read the saved plan to get exact resource specs.
	client, err := iac.NewClient(ctx, dir, iac.Config{
		Env:             p.cloud.Env(),
		DryRun:          p.dryRun,
		BinaryMirrors:   p.binaryMirrors,
		ProviderMirrors: p.providerMirrors,
	})
	if err != nil {
		return "", fmt.Errorf("estimate_cost: create terraform client: %w", err)
	}
	plan, err := client.ShowPlan(ctx)
	if err != nil {
		return "", fmt.Errorf("estimate_cost: read plan: %w", err)
	}

	// Serialize plan changes (resource type + exact specs) for the LLM.
	planJSON, err := json.Marshal(plan.Changes)
	if err != nil {
		return "", fmt.Errorf("estimate_cost: marshal plan: %w", err)
	}

	// Check if plan changes have exact specs (After field). In dry-run mode
	// the simulated plan has no After, so the LLM can only estimate by type.
	hasSpecs := false
	for _, c := range plan.Changes {
		if len(c.After) > 0 {
			hasSpecs = true
			break
		}
	}

	skillBody := p.loadSkillBody(ctx, "huaweicloud-bss")
	agent := openagent.NewAgent("iac-pricing",
		openagent.WithModel(p.model),
		openagent.WithTools(p.fileTools()...),
		openagent.WithMemory(p.memory),
		openagent.WithRunHooks(sloghooks.New(slog.Default())),
		openagent.WithSystemPrompts(
			serverContext,
			skillBody,
			"You are a HuaweiCloud pricing expert. "+
				"Use read/grep/ls to browse the skills/huaweicloud-bss/references/ directory "+
				"for BSS API definitions, use http_request to call the BSS pricing APIs (signing is automatic), "+
				"and use WebSearch/WebFetch as a fallback for public pricing pages. "+
				"You are given the planned resources with exact specs from terraform plan. "+
				"Query the monthly price for each resource. "+
				"Mark prices that cannot be determined as null — do NOT fabricate. "+
				"Return {\"items\": [{\"resource\": \"...\", \"spec\": \"...\", \"monthly\": price or null}], \"total_monthly\": ... or null, \"currency\": \"CNY\", \"note\": \"...\"}."),
		openagent.WithMaxTurns(8),
	)

	var userMsg string
	if hasSpecs {
		userMsg = "Query the prices for these planned resources (with exact specs):\n\n" + string(planJSON)
	} else {
		userMsg = "Query the prices for these planned resources (specs not available — estimate by resource type only):\n\n" + string(planJSON)
	}

	session := openagent.Session{ID: sessionID(deploymentID)}
	result, err := agent.Run(ctx, session, openagent.UserMessage(userMsg))
	if err != nil {
		return "", fmt.Errorf("estimate_cost: LLM run: %w", err)
	}

	// Parse the LLM output and add deployment_id.
	raw := extractJSON(result.FinalOutput)
	var cost struct {
		Items        []any `json:"items"`
		TotalMonthly any   `json:"total_monthly"`
		Currency     string `json:"currency"`
		Note         string `json:"note"`
	}
	if json.Unmarshal([]byte(raw), &cost) != nil {
		cost.Note = result.FinalOutput
	}
	out := map[string]any{
		"deployment_id": deploymentID,
		"items":         cost.Items,
		"total_monthly": cost.TotalMonthly,
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
	dir := filepath.Join(p.deploymentsDir, deploymentID)

	tfFiles, err := readTFFiles(dir)
	if err != nil {
		return "", fmt.Errorf("troubleshoot: read .tf: %w", err)
	}

	skillBody := p.loadSkillBody(ctx, "huaweicloud-troubleshoot")
	agent := openagent.NewAgent("iac-troubleshooter",
		openagent.WithModel(p.model),
		openagent.WithTools(p.fileTools()...),
		openagent.WithMemory(p.memory),
		openagent.WithRunHooks(sloghooks.New(slog.Default())),
		openagent.WithSystemPrompts(
			serverContext,
			skillBody,
			"You are a HuaweiCloud infrastructure troubleshooting expert. "+
				"Use read/grep/ls to find correct patterns in skills/huaweicloud-deploy/references/, "+
				"use WebSearch/WebFetch to search for error solutions. "+
				"You are given the error message and the .tf files that failed. "+
				"Diagnose the root cause and suggest specific fixes. "+
				"Return {\"diagnosis\": \"...\", \"suggestion\": \"...\", \"alternatives\": [\"...\", ...]}."),
		openagent.WithMaxTurns(8),
	)

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
	result, err := agent.Run(ctx, session, openagent.UserMessage(userMsg))
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
// loading (WithSkillLoader) — the LLM sees the skill catalog and calls
// load_skill to load the relevant cloud-service skill on demand.
func (p *Planner) QueryCloud(ctx context.Context, query string) (string, error) {
	agent := openagent.NewAgent("iac-queryer",
		openagent.WithModel(p.model),
		openagent.WithTools(p.fileTools()...),
		openagent.WithMemory(p.memory),
		openagent.WithRunHooks(sloghooks.New(slog.Default())),
		openagent.WithSkillLoader(p.loader),
		openagent.WithSystemPrompts(
			serverContext,
			"You are a HuaweiCloud cloud query expert. "+
				"Use load_skill to load the relevant skill for the cloud service being queried "+
				"(e.g. load_skill(\"huaweicloud-ecs\") for ECS instances/flavors, "+
				"load_skill(\"huaweicloud-vpc\") for VPCs/subnets/security groups, "+
				"load_skill(\"huaweicloud-bss\") for billing/pricing/orders). "+
				"Then use http_request to call the API with the correct endpoint and parameters. "+
				"CRITICAL: Only call read-only APIs (List/Show/Get). NEVER call Create/Update/Delete APIs — "+
				"this tool is for querying existing resources only, not for creating or modifying them. "+
				"Return {\"results\": [...], \"note\": \"...\"}."),
		openagent.WithMaxTurns(10),
	)

	session := openagent.Session{ID: "query"}
	result, err := agent.Run(ctx, session, openagent.UserMessage(query))
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
// If the cloud provider exposes an http_request tool (e.g. huaweicloud
// with SDK-HMAC-SHA256 signing), it is included for calling cloud APIs.
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
