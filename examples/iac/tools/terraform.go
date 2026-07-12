// Package tools provides IaC-specific tool implementations.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	openagent "github.com/yusheng-g/openagent-go"
)

// TerraformTool wraps terraform-exec for use as openagent.Tool.
// In dry-run mode, commands are simulated without calling tf binary.
type TerraformTool struct {
	workDir string
	dryRun  bool
	tf      *tfexec.Terraform // lazy-init on first real call
}

// NewTerraformTool creates a Terraform tool rooted at workDir.
func NewTerraformTool(workDir string, dryRun bool) *TerraformTool {
	return &TerraformTool{workDir: workDir, dryRun: dryRun}
}

func (t *TerraformTool) tfDir() string { return filepath.Join(t.workDir, "terraform") }

func (t *TerraformTool) ensureTF() error {
	if t.tf != nil {
		return nil
	}
	tf, err := tfexec.NewTerraform(t.tfDir(), "terraform")
	if err != nil {
		return fmt.Errorf("terraform not found: %w", err)
	}
	// Disable interactive prompts and color in output.
	tf.SetStderr(os.Stderr)
	t.tf = tf
	return nil
}

// ── Self-approval ──

func (t *TerraformTool) CanSelfApprove(name string, _ json.RawMessage) bool {
	switch name {
	case "terraform_init", "terraform_plan", "terraform_output":
		return true
	default:
		return false
	}
}

// ── Tool: terraform_init ──

func (t *TerraformTool) terraformInitDef() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terraform_init",
		Description: "Initialize a Terraform working directory. Downloads providers and modules. Run this before any other terraform commands.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	}
}

func (t *TerraformTool) terraformInit(ctx context.Context) (string, error) {
	if t.dryRun {
		return "[DRY RUN] terraform init — would download providers and initialize working directory", nil
	}
	if err := t.ensureTF(); err != nil {
		return "", err
	}
	if err := t.tf.Init(ctx, tfexec.Upgrade(false)); err != nil {
		return "", fmt.Errorf("terraform init: %w", err)
	}
	return "Terraform has been successfully initialized.", nil
}

// ── Tool: terraform_output ──

func (t *TerraformTool) terraformOutputDef() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terraform_output",
		Description: "Get outputs from the applied Terraform state (e.g., public IPs, connection strings).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	}
}

func (t *TerraformTool) terraformOutput(ctx context.Context) (string, error) {
	if t.dryRun {
		return t.simulatedOutput(), nil
	}
	if err := t.ensureTF(); err != nil {
		return "", err
	}
	outputs, err := t.tf.Output(ctx)
	if err != nil {
		return "", fmt.Errorf("terraform output: %w", err)
	}
	b, _ := json.MarshalIndent(outputs, "", "  ")
	return string(b), nil
}

// ── Tool: terraform_plan ──

func (t *TerraformTool) terraformPlanDef() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name: "terraform_plan",
		Description: "Generate a Terraform execution plan. Shows what resources will be created, modified, or destroyed. Safe to run — no changes are applied.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	}
}

func (t *TerraformTool) terraformPlan(ctx context.Context) (string, error) {
	if t.dryRun {
		return t.simulatedPlan(), nil
	}
	if err := t.ensureTF(); err != nil {
		return "", err
	}

	// Generate the plan and save to tfplan.
	hasChanges, err := t.tf.Plan(ctx, tfexec.Out("tfplan"))
	if err != nil {
		return "", fmt.Errorf("terraform plan: %w", err)
	}

	var b strings.Builder
	if !hasChanges {
		b.WriteString("No changes. Your infrastructure matches the configuration.\n")
	} else {
		b.WriteString("Plan: changes detected.\n\n")
	}

	// Get structured plan JSON.
	plan, err := t.tf.ShowPlanFile(ctx, "tfplan")
	if err != nil {
		b.WriteString("(structured plan details unavailable)\n")
		return b.String(), nil
	}

	b.WriteString(formatPlan(plan))
	return b.String(), nil
}

// ── Tool: terraform_apply ──

func (t *TerraformTool) terraformApplyDef() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name: "terraform_apply",
		Description: "Apply the Terraform execution plan. This CREATES or MODIFIES real cloud resources. Requires approval. Use only after reviewing terraform_plan output.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	}
}

func (t *TerraformTool) terraformApply(ctx context.Context) (string, error) {
	if t.dryRun {
		return "[DRY RUN] terraform apply — would create/modify cloud resources per the plan", nil
	}
	if err := t.ensureTF(); err != nil {
		return "", err
	}
	if err := t.tf.Apply(ctx, tfexec.DirOrPlan("tfplan")); err != nil {
		return "", fmt.Errorf("terraform apply: %w", err)
	}
	return "Apply complete. Run terraform_output to get endpoints.", nil
}

// ── Tool: terraform_destroy ──

func (t *TerraformTool) terraformDestroyDef() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terraform_destroy",
		Description: "Destroy all resources managed by this Terraform configuration. DESTRUCTIVE — requires approval.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	}
}

func (t *TerraformTool) terraformDestroy(ctx context.Context) (string, error) {
	if t.dryRun {
		return "[DRY RUN] terraform destroy — would destroy all resources", nil
	}
	if err := t.ensureTF(); err != nil {
		return "", err
	}
	if err := t.tf.Destroy(ctx); err != nil {
		return "", fmt.Errorf("terraform destroy: %w", err)
	}
	return "Destroy complete. All resources removed.", nil
}

// ── openagent.Tool interface ──

func (t *TerraformTool) AsTools() []openagent.Tool {
	return []openagent.Tool{
		&tfInitTool{t},
		&tfPlanTool{t},
		&tfApplyTool{t},
		&tfOutputTool{t},
		&tfDestroyTool{t},
	}
}

type tfInitTool struct{ tf *TerraformTool }

func (tt *tfInitTool) Definition() openagent.FunctionDefinition          { return tt.tf.terraformInitDef() }
func (tt *tfInitTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) { return tt.tf.terraformInit(ctx) }

type tfPlanTool struct{ tf *TerraformTool }

func (tt *tfPlanTool) Definition() openagent.FunctionDefinition          { return tt.tf.terraformPlanDef() }
func (tt *tfPlanTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) { return tt.tf.terraformPlan(ctx) }

type tfApplyTool struct{ tf *TerraformTool }

func (tt *tfApplyTool) Definition() openagent.FunctionDefinition          { return tt.tf.terraformApplyDef() }
func (tt *tfApplyTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) { return tt.tf.terraformApply(ctx) }

type tfOutputTool struct{ tf *TerraformTool }

func (tt *tfOutputTool) Definition() openagent.FunctionDefinition          { return tt.tf.terraformOutputDef() }
func (tt *tfOutputTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) { return tt.tf.terraformOutput(ctx) }

type tfDestroyTool struct{ tf *TerraformTool }

func (tt *tfDestroyTool) Definition() openagent.FunctionDefinition          { return tt.tf.terraformDestroyDef() }
func (tt *tfDestroyTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) { return tt.tf.terraformDestroy(ctx) }

// ── Structured plan formatting ──

func formatPlan(plan *tfjson.Plan) string {
	if plan == nil {
		return ""
	}
	var b strings.Builder
	resources := plan.ResourceChanges
	if len(resources) == 0 {
		b.WriteString("No resource changes.\n")
		return b.String()
	}

	actions := map[tfjson.Action]int{}
	for _, rc := range resources {
		for _, a := range rc.Change.Actions {
			actions[a]++
		}
	}
	if actions[tfjson.ActionCreate] > 0 {
		b.WriteString(fmt.Sprintf("  + %d to create\n", actions[tfjson.ActionCreate]))
	}
	if actions[tfjson.ActionUpdate] > 0 {
		b.WriteString(fmt.Sprintf("  ~ %d to update\n", actions[tfjson.ActionUpdate]))
	}
	if actions[tfjson.ActionDelete] > 0 {
		b.WriteString(fmt.Sprintf("  - %d to delete\n", actions[tfjson.ActionDelete]))
	}
	if actions[tfjson.ActionNoop] > 0 {
		b.WriteString(fmt.Sprintf("  · %d unchanged\n", actions[tfjson.ActionNoop]))
	}

	b.WriteString("\nResource details:\n")
	for _, rc := range resources {
		acts := rc.Change.Actions
		if len(acts) == 1 && acts[0] == tfjson.ActionNoop {
			continue
		}
		strs := make([]string, len(acts))
		for i, a := range acts {
			strs[i] = string(a)
		}
		action := strings.Join(strs, "/")
		b.WriteString(fmt.Sprintf("  [%s] %s (%s)\n", action, rc.Address, rc.Type))
	}

	if len(plan.Variables) > 0 {
		b.WriteString("\nVariables:\n")
		for k, v := range plan.Variables {
			b.WriteString(fmt.Sprintf("  %s = %v\n", k, v.Value))
		}
	}

	return b.String()
}

// ── Dry-run simulations ──

func (t *TerraformTool) simulatedPlan() string {
	files, _ := filepath.Glob(filepath.Join(t.tfDir(), "*.tf"))
	if len(files) == 0 {
		return "[DRY RUN] No .tf files found.\n"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[DRY RUN] %d .tf files.\n\nPlan: 3 to add, 0 to change, 0 to destroy.\n\nResources:\n", len(files)))
	for _, f := range files {
		base := filepath.Base(f)
		if base == "provider.tf" {
			continue
		}
		b.WriteString(fmt.Sprintf("  + %s\n", strings.TrimSuffix(base, ".tf")))
	}
	return b.String()
}

func (t *TerraformTool) simulatedOutput() string {
	return `{
  "web_server_public_ip": {"value": "123.60.xxx.xxx"},
  "rds_private_ip": {"value": "192.168.1.xxx"},
  "rds_port": {"value": 5432},
  "obs_bucket_domain": {"value": "myapp-assets.obs.cn-north-4.myhuaweicloud.com"}
}
[DRY RUN] These are simulated outputs.`
}

// EnsureDir creates the terraform directory if it doesn't exist.
func (t *TerraformTool) EnsureDir() error {
	return os.MkdirAll(t.tfDir(), 0755)
}

// Embedded templates set by the caller.
var (
	ProviderTemplate []byte
	ECSTemplate      []byte
	RDSTemplate      []byte
	OBSTemplate      []byte
	CDNTemplate      []byte
)
