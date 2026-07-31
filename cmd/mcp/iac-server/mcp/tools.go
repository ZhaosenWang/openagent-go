// Package mcp defines the 8 deployment tools exposed by iac-server over MCP.
//
// Tools are split into two groups:
//   - LLM tools (plan_deployment, update_deployment, estimate_cost,
//     troubleshoot_deployment): delegate to agent.Planner
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

// NewTools builds the 9 tools exposed by iac-server.
func NewTools(cfg Config) []openagent.Tool {
	return []openagent.Tool{
		&planDeploymentTool{cfg: cfg},
		&updateDeploymentTool{cfg: cfg},
		&estimateCostTool{cfg: cfg},
		&troubleshootDeploymentTool{cfg: cfg},
		&applyDeploymentTool{cfg: cfg},
		&destroyDeploymentTool{cfg: cfg},
		&getDeploymentStatusTool{cfg: cfg},
		&listDeploymentsTool{cfg: cfg},
		&queryCloudTool{cfg: cfg},
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

// ── plan_deployment ──

type planDeploymentTool struct{ cfg Config }

func (t *planDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "plan_deployment",
		Description: "Analyze a deployment request (free-text) and produce a terraform plan. Returns need_input (with questions) if information is incomplete, or ready (with deployment_id and plan) if complete. Does NOT show pricing — call estimate_cost before apply_deployment.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"request": {
					"type": "string",
					"description": "Free-text deployment request, e.g. \"deploy a WordPress site to cn-east-3, HA, budget 500/month\""
				}
			},
			"required": ["request"]
		}`),
	}
}

func (t *planDeploymentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("plan_deployment: %w", err)
	}
	if params.Request == "" {
		return "", fmt.Errorf("plan_deployment: request is required")
	}
	return t.cfg.Planner.Plan(ctx, params.Request)
}

// ── update_deployment ──

type updateDeploymentTool struct{ cfg Config }

func (t *updateDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "update_deployment",
		Description: "Modify an existing planned deployment. Accepts a change request (e.g. \"change ECS flavor to s6.xlarge.2\") and updates the .tf files in-place, then re-runs terraform plan. Use this instead of plan_deployment when the user wants to adjust an existing deployment. Returns the updated plan with the same deployment_id. After updating, call estimate_cost again to see the new pricing before apply_deployment.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"deployment_id": {
					"type": "string",
					"description": "Deployment ID to update"
				},
				"change_request": {
					"type": "string",
					"description": "Free-text change request, e.g. \"change ECS flavor to s6.xlarge.2\" or \"rename vpc.test to vpc.main\""
				}
			},
			"required": ["deployment_id", "change_request"]
		}`),
	}
}

func (t *updateDeploymentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		DeploymentID  string `json:"deployment_id"`
		ChangeRequest string `json:"change_request"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("update_deployment: %w", err)
	}
	if params.DeploymentID == "" || params.ChangeRequest == "" {
		return "", fmt.Errorf("update_deployment: deployment_id and change_request are required")
	}
	if !validDeploymentID(params.DeploymentID) {
		return "", fmt.Errorf("update_deployment: invalid deployment_id %q", params.DeploymentID)
	}
	return t.cfg.Planner.UpdateDeployment(ctx, params.DeploymentID, params.ChangeRequest)
}

// ── estimate_cost ──

type estimateCostTool struct{ cfg Config }

func (t *estimateCostTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "estimate_cost",
		Description: "Estimate the monthly cost of a PLANNED deployment (resources not yet created). MUST be called after plan_deployment and before apply_deployment. This forecasts future costs based on the terraform plan — it does NOT query past billing. For existing bills/costs, use query_cloud.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"deployment_id": {
					"type": "string",
					"description": "Deployment ID from plan_deployment"
				}
			},
			"required": ["deployment_id"]
		}`),
	}
}

func (t *estimateCostTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("estimate_cost: %w", err)
	}
	if params.DeploymentID == "" {
		return "", fmt.Errorf("estimate_cost: deployment_id is required")
	}
	if !validDeploymentID(params.DeploymentID) {
		return "", fmt.Errorf("estimate_cost: invalid deployment_id %q", params.DeploymentID)
	}
	return t.cfg.Planner.EstimateCost(ctx, params.DeploymentID)
}

// ── troubleshoot_deployment ──

type troubleshootDeploymentTool struct{ cfg Config }

func (t *troubleshootDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "troubleshoot_deployment",
		Description: "Diagnose a deployment error and suggest fixes. Reads the .tf files and error message, researches solutions via examples and web search, and returns a diagnosis with recommended actions.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"deployment_id": {
					"type": "string",
					"description": "Deployment ID to troubleshoot"
				},
				"error": {
					"type": "string",
					"description": "The error message from the failed operation"
				}
			},
			"required": ["deployment_id", "error"]
		}`),
	}
}

func (t *troubleshootDeploymentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		DeploymentID string `json:"deployment_id"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("troubleshoot_deployment: %w", err)
	}
	if params.DeploymentID == "" || params.Error == "" {
		return "", fmt.Errorf("troubleshoot_deployment: deployment_id and error are required")
	}
	if !validDeploymentID(params.DeploymentID) {
		return "", fmt.Errorf("troubleshoot_deployment: invalid deployment_id %q", params.DeploymentID)
	}
	return t.cfg.Planner.Troubleshoot(ctx, params.DeploymentID, params.Error)
}

// ── apply_deployment ──

type applyDeploymentTool struct{ cfg Config }

func (t *applyDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "apply_deployment",
		Description: "Apply a saved terraform plan. This creates/modifies real cloud resources. The deployment must have been planned first (plan_deployment returned status=ready). Call estimate_cost first so the user sees pricing.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"deployment_id": {
					"type": "string",
					"description": "Deployment ID to apply"
				}
			},
			"required": ["deployment_id"]
		}`),
	}
}

func (t *applyDeploymentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("apply_deployment: %w", err)
	}
	if params.DeploymentID == "" {
		return "", fmt.Errorf("apply_deployment: deployment_id is required")
	}
	if !validDeploymentID(params.DeploymentID) {
		return "", fmt.Errorf("apply_deployment: invalid deployment_id %q", params.DeploymentID)
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
	client, err := iac.NewClient(ctx, dir, iacConfig(t.cfg.Cloud, t.cfg.DryRun, t.cfg.BinaryMirrors, t.cfg.ProviderMirrors))
	if err != nil {
		return "", fmt.Errorf("apply_deployment: %w", err)
	}

	result, err := client.Apply(ctx)
	if err != nil {
		return "", fmt.Errorf("apply_deployment: %w", err)
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("apply_deployment: marshal: %w", err)
	}
	return string(data), nil
}

// ── destroy_deployment ──

type destroyDeploymentTool struct{ cfg Config }

func (t *destroyDeploymentTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "destroy_deployment",
		Description: "Destroy all resources in a deployment. This permanently deletes cloud resources. Use with caution.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"deployment_id": {
					"type": "string",
					"description": "Deployment ID to destroy"
				}
			},
			"required": ["deployment_id"]
		}`),
	}
}

func (t *destroyDeploymentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("destroy_deployment: %w", err)
	}
	if params.DeploymentID == "" {
		return "", fmt.Errorf("destroy_deployment: deployment_id is required")
	}
	if !validDeploymentID(params.DeploymentID) {
		return "", fmt.Errorf("destroy_deployment: invalid deployment_id %q", params.DeploymentID)
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
	client, err := iac.NewClient(ctx, dir, iacConfig(t.cfg.Cloud, t.cfg.DryRun, t.cfg.BinaryMirrors, t.cfg.ProviderMirrors))
	if err != nil {
		return "", fmt.Errorf("destroy_deployment: %w", err)
	}

	resources, err := client.Destroy(ctx)
	if err != nil {
		return "", fmt.Errorf("destroy_deployment: %w", err)
	}

	result := map[string]any{
		"destroyed": true,
		"resources": resources,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("destroy_deployment: marshal: %w", err)
	}
	return string(data), nil
}

// ── get_deployment_status ──

type getDeploymentStatusTool struct{ cfg Config }

func (t *getDeploymentStatusTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "get_deployment_status",
		Description: "Read the terraform state for a deployment and return a status summary. Does not call terraform binary — reads the state file directly.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"deployment_id": {
					"type": "string",
					"description": "Deployment ID to check"
				}
			},
			"required": ["deployment_id"]
		}`),
	}
}

func (t *getDeploymentStatusTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("get_deployment_status: %w", err)
	}
	if params.DeploymentID == "" {
		return "", fmt.Errorf("get_deployment_status: deployment_id is required")
	}
	if !validDeploymentID(params.DeploymentID) {
		return "", fmt.Errorf("get_deployment_status: invalid deployment_id %q", params.DeploymentID)
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
	statePath := filepath.Join(dir, "terraform.tfstate")

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("get_deployment_status: no state file for deployment %s — has it been planned/applied?", params.DeploymentID)
		}
		return "", fmt.Errorf("get_deployment_status: %w", err)
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
		return "", fmt.Errorf("get_deployment_status: parse state: %w", err)
	}

	summary := map[string]any{
		"deployment_id":  params.DeploymentID,
		"resource_count": len(state.Resources),
		"resources":      state.Resources,
		"outputs":        state.Outputs,
	}
	result, err := json.Marshal(summary)
	if err != nil {
		return "", fmt.Errorf("get_deployment_status: marshal: %w", err)
	}
	return string(result), nil
}

// ── query_cloud ──

type queryCloudTool struct{ cfg Config }

func (t *queryCloudTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "query_cloud",
		Description: "Query EXISTING cloud resources, specs, bills, costs, or quotas. Use this for any read-only query about the current cloud account state — e.g. \"list all ECS instances\", \"what specs does s6.large.2 have\", \"how much did I spend this month\", \"show my bills for 2025-07\". This queries real cloud APIs for already-existing resources and past billing data. Does NOT modify any resources. For estimating FUTURE costs of a planned deployment, use estimate_cost.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Natural language query, e.g. \"list all ECS instances in cn-east-3\" or \"how much did I spend this month\""
				}
			},
			"required": ["query"]
		}`),
	}
}

func (t *queryCloudTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("query_cloud: %w", err)
	}
	if params.Query == "" {
		return "", fmt.Errorf("query_cloud: query is required")
	}
	return t.cfg.Planner.QueryCloud(ctx, params.Query)
}

// ── list_deployments ──

type listDeploymentsTool struct{ cfg Config }

func (t *listDeploymentsTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "list_deployments",
		Description: "List all deployments by scanning the deployments directory. Returns deployment IDs and whether each has a state file.",
		Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (t *listDeploymentsTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	entries, err := os.ReadDir(t.cfg.DeploymentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "[]", nil // no deployments yet
		}
		return "", fmt.Errorf("list_deployments: %w", err)
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
		return "", fmt.Errorf("list_deployments: marshal: %w", err)
	}
	return string(data), nil
}
