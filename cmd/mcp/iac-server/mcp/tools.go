// Package mcp defines the deployment tools exposed by iac-server over MCP.
//
// Tools are split into two groups:
//   - LLM tools (propose_architecture, specify_resources, generate_terraform_plan,
//     estimate_cost, troubleshoot_deployment, query_cloud): delegate to agent.Planner
//   - Execution tools (apply, destroy, get_status, list): call iac.Client
//     directly
//
// All tools return JSON strings. Server-side execution is unconditional —
// approval is the client's concern, not the server's.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/agent"
	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
	"github.com/yusheng-g/openagent-go/iac"
)

// Config holds shared dependencies for all tools.
type Config struct {
	Planner         *agent.Planner
	Cloud           provider.CloudProvider
	DeploymentsDir  string   // root dir for deployment workspaces
	DryRun          bool     // pass to iac.Config.DryRun
	BinaryMirrors   []string // terraform binary download mirrors
	ProviderMirrors []string // provider download mirrors (URLs or local paths)
}

// NewTools builds the 11 tools exposed by iac-server.
func NewTools(cfg Config) []openagent.Tool {
	return []openagent.Tool{
		&proposeArchitectureTool{cfg: cfg},
		&specifyResourcesTool{cfg: cfg},
		&generateTerraformPlanTool{cfg: cfg},
		&estimateCostTool{cfg: cfg},
		&troubleshootDeploymentTool{cfg: cfg},
		&applyDeploymentTool{cfg: cfg},
		&destroyDeploymentTool{cfg: cfg},
		&getDeploymentStatusTool{cfg: cfg},
		&listDeploymentsTool{cfg: cfg},
		&queryCloudTool{cfg: cfg},
		&updateDeploymentTool{cfg: cfg},
	}
}

// workDir returns the workspace path for a deployment ID.
func workDir(deploymentsDir, deploymentID string) string {
	return filepath.Join(deploymentsDir, deploymentID)
}

// validDeploymentID reports whether id is a safe deployment identifier:
// non-empty, no path separators, no parent-dir components. This prevents
// deployment_id values like "../etc" from escaping deploymentsDir.
func validDeploymentID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	// Reject any segment that is ".." after cleaning.
	cleaned := filepath.Clean(id)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// iacConfig builds an iac.Config with cloud credentials and mirror settings.
func iacConfig(cloud provider.CloudProvider, dryRun bool, binaryMirrors, providerMirrors []string) iac.Config {
	return iac.Config{
		Env:             cloud.Env(),
		DryRun:          dryRun,
		BinaryMirrors:   binaryMirrors,
		ProviderMirrors: providerMirrors,
	}
}

// ── propose_architecture ──

type proposeArchitectureTool struct{ cfg Config }

func (t *proposeArchitectureTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "propose_architecture",
		Description: "Step 1 of deployment: Analyze a deployment request and recommend a cloud architecture. Returns architecture name, required services, reasoning, and a deployment_id. Does NOT write .tf files. The user should confirm the architecture before calling specify_resources.",
		Parameters:  openagent.SchemaOf[ProposeArchitectureParams](),
	}
}

func (t *proposeArchitectureTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[ProposeArchitectureParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("propose_architecture: %w", err), false, "")
	}
	out, err := t.cfg.Planner.ProposeArchitecture(ctx, params.Request)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── specify_resources ──

type specifyResourcesTool struct{ cfg Config }

func (t *specifyResourcesTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "specify_resources",
		Description: "Step 2 of deployment: Determine concrete resource specs (flavor, image, disk, CIDR, etc.) for each node of the DAG from propose_architecture. Queries available specs via cloud APIs. If too many choices or missing info, returns status need_input with questions — call again with answers filled in. Optional adjustments let the user modify specs. The user should confirm the resources before calling generate_terraform_plan.",
		Parameters:  openagent.SchemaOf[SpecifyResourcesParams](),
	}
}

func (t *specifyResourcesTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[SpecifyResourcesParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("specify_resources: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("specify_resources: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	out, err := t.cfg.Planner.SpecifyResources(ctx, params.DeploymentID, params.Answers, params.Adjustments)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── generate_terraform_plan ──

type generateTerraformPlanTool struct{ cfg Config }

func (t *generateTerraformPlanTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "generate_terraform_plan",
		Description: "Step 3 of deployment: Write .tf files from the deployment DAG (nodes = resources with confirmed specs, edges = dependencies), then run terraform init + plan. Returns the .tf files and a plan preview. Requires specify_resources to have completed. The user should review the plan before calling estimate_cost.",
		Parameters:  openagent.SchemaOf[GenerateTerraformPlanParams](),
	}
}

func (t *generateTerraformPlanTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[GenerateTerraformPlanParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("generate_terraform_plan: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("generate_terraform_plan: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	out, err := t.cfg.Planner.GenerateTerraformPlan(ctx, params.DeploymentID)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── update_deployment ──

type updateDeploymentTool struct{ cfg Config }

func (t *updateDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "update_deployment",
		Description: "Modify an existing deployment. Re-runs specify_resources (with user answers/adjustments) and generate_terraform_plan. Use this when the user wants to adjust an existing deployment (e.g. \"change ECS flavor to s6.xlarge.2\"). Returns the updated plan with the same deployment_id. The previous cost estimate is invalidated — call estimate_cost again before apply_deployment.",
		Parameters:  openagent.SchemaOf[UpdateDeploymentParams](),
	}
}

func (t *updateDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[UpdateDeploymentParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("update_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("update_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	out, err := t.cfg.Planner.UpdateDeployment(ctx, params.DeploymentID, params.Answers, params.ChangeRequest)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── estimate_cost ──

type estimateCostTool struct{ cfg Config }

func (t *estimateCostTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "estimate_cost",
		Description: "Step 4 of deployment: Estimate the cost of a PLANNED deployment (resources not yet created) from the deployment DAG. MUST be called after generate_terraform_plan (and after any update_deployment) and before apply_deployment — apply is rejected without it. pricing_mode: \"on-demand\" (按需) or \"monthly\" (包月); if the user did not state a preference, omit it. This forecasts FUTURE costs — it does NOT query past billing. For existing bills/costs, use query_cloud.",
		Parameters:  openagent.SchemaOf[EstimateCostParams](),
	}
}

func (t *estimateCostTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[EstimateCostParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("estimate_cost: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("estimate_cost: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	out, err := t.cfg.Planner.EstimateCost(ctx, params.DeploymentID, params.PricingMode)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── troubleshoot_deployment ──

type troubleshootDeploymentTool struct{ cfg Config }

func (t *troubleshootDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "troubleshoot_deployment",
		Description: "Diagnose a deployment error and suggest fixes. Reads the .tf files and error message, researches solutions via examples and web search, and returns a diagnosis with recommended actions.",
		Parameters:  openagent.SchemaOf[TroubleshootParams](),
	}
}

func (t *troubleshootDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[TroubleshootParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("troubleshoot_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("troubleshoot_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	out, err := t.cfg.Planner.Troubleshoot(ctx, params.DeploymentID, params.Error)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── apply_deployment ──

type applyDeploymentTool struct{ cfg Config }

func (t *applyDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "apply_deployment",
		Description: "Step 5 of deployment: Apply a saved terraform plan. This creates/modifies real cloud resources. The deployment must have been planned (generate_terraform_plan succeeded) AND cost-estimated (estimate_cost succeeded) first — apply is rejected with an error if the deployment has no current cost estimate.",
		Parameters:  openagent.SchemaOf[ApplyDeploymentParams](),
	}
}

func (t *applyDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[ApplyDeploymentParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("apply_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("apply_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)

	// Cost gate: apply is only allowed after estimate_cost ran for the
	// current deployment state (any DAG/.tf mutation invalidates the marker).
	if !agent.HasCost(dir) {
		return openagent.ErrorResult(fmt.Errorf("apply_deployment: deployment %s has not been cost-estimated for its current state — call estimate_cost first", params.DeploymentID), false, "")
	}

	client, err := iac.NewClient(ctx, dir, iacConfig(t.cfg.Cloud, t.cfg.DryRun, t.cfg.BinaryMirrors, t.cfg.ProviderMirrors))
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("apply_deployment: %w", err), false, "")
	}

	result, err := client.Apply(ctx)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("apply_deployment: %w", err), false, "")
	}

	data, err := json.Marshal(result)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("apply_deployment: marshal: %w", err), false, "")
	}
	return &openagent.ToolResult{Content: string(data)}
}

// ── destroy_deployment ──

type destroyDeploymentTool struct{ cfg Config }

func (t *destroyDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "destroy_deployment",
		Description: "Destroy all resources in a deployment. This permanently deletes cloud resources. Use with caution.",
		Parameters:  openagent.SchemaOf[DestroyDeploymentParams](),
	}
}

func (t *destroyDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[DestroyDeploymentParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("destroy_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("destroy_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
	client, err := iac.NewClient(ctx, dir, iacConfig(t.cfg.Cloud, t.cfg.DryRun, t.cfg.BinaryMirrors, t.cfg.ProviderMirrors))
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("destroy_deployment: %w", err), false, "")
	}

	resources, err := client.Destroy(ctx)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("destroy_deployment: %w", err), false, "")
	}

	result := map[string]any{
		"destroyed": true,
		"resources": resources,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("destroy_deployment: marshal: %w", err), false, "")
	}
	return &openagent.ToolResult{Content: string(data)}
}

// ── get_deployment_status ──

type getDeploymentStatusTool struct{ cfg Config }

func (t *getDeploymentStatusTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "get_deployment_status",
		Description: "Read the terraform state for a deployment and return a status summary. Does not call terraform binary — reads the state file directly.",
		Parameters:  openagent.SchemaOf[GetDeploymentStatusParams](),
	}
}

func (t *getDeploymentStatusTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[GetDeploymentStatusParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("get_deployment_status: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return openagent.ErrorResult(fmt.Errorf("get_deployment_status: invalid deployment_id %q", params.DeploymentID), false, "")
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
	statePath := filepath.Join(dir, "terraform.tfstate")

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return openagent.ErrorResult(fmt.Errorf("get_deployment_status: no state file for deployment %s — has it been planned/applied?", params.DeploymentID), false, "")
		}
		return openagent.ErrorResult(fmt.Errorf("get_deployment_status: %w", err), false, "")
	}

	// Parse the state file to extract a summary.
	var state struct {
		Resources []struct {
			Address string `json:"address"`
			Type    string `json:"type"`
			Name    string `json:"name"`
		} `json:"resources"`
		Outputs map[string]struct {
			Value any `json:"value"`
			Type  any `json:"type"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return openagent.ErrorResult(fmt.Errorf("get_deployment_status: parse state: %w", err), false, "")
	}

	summary := map[string]any{
		"deployment_id":  params.DeploymentID,
		"resource_count": len(state.Resources),
		"resources":      state.Resources,
		"outputs":        state.Outputs,
	}
	result, err := json.Marshal(summary)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("get_deployment_status: marshal: %w", err), false, "")
	}
	return &openagent.ToolResult{Content: string(result)}
}

// ── query_cloud ──

type queryCloudTool struct{ cfg Config }

func (t *queryCloudTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "query_cloud",
		Description: "Query EXISTING cloud resources, specs, bills, costs, or quotas. Use this for any read-only query about the current cloud account state — e.g. \"list all ECS instances\", \"what specs does s6.large.2 have\", \"how much did I spend this month\", \"show my bills for 2025-07\". This queries real cloud APIs for already-existing resources and past billing data. Does NOT modify any resources. For estimating FUTURE costs of a planned deployment, use estimate_cost.",
		Parameters:  openagent.SchemaOf[QueryCloudParams](),
	}
}

func (t *queryCloudTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[QueryCloudParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("query_cloud: %w", err), false, "")
	}
	out, err := t.cfg.Planner.QueryCloud(ctx, params.Query)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	return &openagent.ToolResult{Content: out}
}

// ── list_deployments ──

type listDeploymentsTool struct{ cfg Config }

func (t *listDeploymentsTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "list_deployments",
		Description: "List all deployments by scanning the deployments directory. Returns deployment IDs and whether each has a state file.",
		Parameters:  openagent.SchemaOf[struct{}](),
	}
}

func (t *listDeploymentsTool) Execute(ctx context.Context, _ json.RawMessage) *openagent.ToolResult {
	entries, err := os.ReadDir(t.cfg.DeploymentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &openagent.ToolResult{Content: "[]"} // no deployments yet
		}
		return openagent.ErrorResult(fmt.Errorf("list_deployments: %w", err), false, "")
	}

	type deployment struct {
		ID       string `json:"id"`
		HasState bool   `json:"has_state"`
		HasPlan  bool   `json:"has_plan"`
	}

	var deployments []deployment
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(t.cfg.DeploymentsDir, entry.Name())
		_, stateErr := os.Stat(filepath.Join(dir, "terraform.tfstate"))
		_, planErr := os.Stat(filepath.Join(dir, "tfplan"))
		deployments = append(deployments, deployment{
			ID:       entry.Name(),
			HasState: stateErr == nil,
			HasPlan:  planErr == nil,
		})
	}

	// Sort by ID for stable output.
	sort.Slice(deployments, func(i, j int) bool {
		return deployments[i].ID < deployments[j].ID
	})

	data, err := json.Marshal(deployments)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("list_deployments: marshal: %w", err), false, "")
	}
	return &openagent.ToolResult{Content: string(data), JSON: data}
}

// ── Tool parameter schemas ──
//
// Definitions and parsing share these types (SchemaOf generates the
// schema, ParseArgs parses the arguments), so the schema the model sees
// always matches what Execute accepts.

// ProposeArchitectureParams are the arguments to propose_architecture.
type ProposeArchitectureParams struct {
	Request string `json:"request" jsonschema:"description=Free-text deployment request, e.g. deploy a WordPress site to cn-east-3, single instance, budget 100/month"`
}

// SpecifyResourcesParams are the arguments to specify_resources.
type SpecifyResourcesParams struct {
	DeploymentID string   `json:"deployment_id" jsonschema:"description=Deployment ID from propose_architecture"`
	Answers      []string `json:"answers,omitempty" jsonschema:"description=Answers to the questions returned by a previous specify_resources call, one per question in order"`
	Adjustments  string   `json:"adjustments,omitempty" jsonschema:"description=Optional free-text adjustments, e.g. use s6.xlarge.2 instead or add a 100GB data disk"`
}

// GenerateTerraformPlanParams are the arguments to generate_terraform_plan.
type GenerateTerraformPlanParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID from propose_architecture"`
}

// UpdateDeploymentParams are the arguments to update_deployment.
type UpdateDeploymentParams struct {
	DeploymentID  string   `json:"deployment_id" jsonschema:"description=Deployment ID to update"`
	Answers       []string `json:"answers,omitempty" jsonschema:"description=Answers to the questions returned by a previous specify_resources call, one per question in order"`
	ChangeRequest string   `json:"change_request" jsonschema:"description=Free-text change request, e.g. change ECS flavor to s6.xlarge.2 or rename vpc.test to vpc.main"`
}

// EstimateCostParams are the arguments to estimate_cost.
type EstimateCostParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID from generate_terraform_plan"`
	PricingMode  string `json:"pricing_mode,omitempty" jsonschema:"description=Billing mode the user asked for: \"on-demand\" (按需/pay-as-you-go) or \"monthly\" (包月). Omit to default to on-demand. Pass the user's stated preference verbatim."`
}

// TroubleshootParams are the arguments to troubleshoot_deployment.
type TroubleshootParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to troubleshoot"`
	Error        string `json:"error" jsonschema:"description=The error message from the failed operation"`
}

// ApplyDeploymentParams are the arguments to apply_deployment.
type ApplyDeploymentParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to apply"`
}

// DestroyDeploymentParams are the arguments to destroy_deployment.
type DestroyDeploymentParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to destroy"`
}

// GetDeploymentStatusParams are the arguments to get_deployment_status.
type GetDeploymentStatusParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to check"`
}

// QueryCloudParams are the arguments to query_cloud.
type QueryCloudParams struct {
	Query string `json:"query" jsonschema:"description=Natural language query, e.g. list all ECS instances in cn-east-3 or how much did I spend this month"`
}
