// Package template provides a generic .tf file generator that reads
// templates from a skill directory.
//
// The generator is cloud-agnostic — it doesn't know about HuaweiCloud,
// AWS, or any specific cloud. It reads .tf.tmpl files from the skill
// directory (provided by the cloud vendor, community, or user), fills
// them with ResourceSpec values using text/template, and writes the
// results to a workDir.
//
// Template lookup: for a ResourceSpec with Type="ecs", the generator
// looks for {skillPath}/ecs.tf.tmpl. provider.tf.tmpl is always written
// first if present.
//
// Template data: templates receive Type, Name, Region at the top level
// and everything else under {{ .Props.xxx }}. Cross-references between
// resources (e.g. a subnet referencing its VPC) are expressed via
// Props — the generator does not hardcode any resource relationship.
package template

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
)

// Generator reads .tf.tmpl templates from a skill directory and generates
// .tf files. One Generator is bound to a single skill directory (SkillInfo.Path).
type Generator struct {
	skillPath string
}

// New creates a Generator bound to the given skill directory path.
// The path should come from openagent.SkillInfo.Path.
func New(skillPath string) *Generator {
	return &Generator{skillPath: skillPath}
}

// Generate writes .tf files for the given specs into workDir.
//
// It writes provider.tf.tmpl first (if present), then one .tf file per
// ResourceSpec. For a spec with Type="ecs" and Name="web", it reads
// {skillPath}/ecs.tf.tmpl and writes {workDir}/web.tf.
//
// Each template receives {Type, Name, Region, Props}. All resource-specific
// attributes (flavor, count, az, cidr, engine, ...) are in Props — the
// generator does not hardcode any field beyond the four universal ones.
// Cross-references between resources are expressed via Props by the
// server LLM when building specs (e.g. subnet Props can name its VPC).
func (g *Generator) Generate(workDir string, specs []provider.ResourceSpec) error {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}

	// Determine region from specs (for provider.tf).
	region := ""
	for _, s := range specs {
		if s.Region != "" {
			region = s.Region
			break
		}
	}

	// Write provider.tf first if the template exists.
	if err := g.writeIfPresent(workDir, "provider.tf.tmpl", "provider.tf", map[string]any{
		"Region": region,
	}); err != nil {
		return fmt.Errorf("provider.tf: %w", err)
	}

	seen := make(map[string]bool)
	for _, s := range specs {
		if seen[s.Name] {
			return fmt.Errorf("duplicate resource name %q — each spec must have a unique Name", s.Name)
		}
		seen[s.Name] = true

		tmplName := s.Type + ".tf.tmpl"
		outputName := s.Name + ".tf"

		data := map[string]any{
			"Type":   s.Type,
			"Name":   s.Name,
			"Region": s.Region,
			"Props":  s.Props,
		}

		if err := g.writeTemplate(workDir, tmplName, outputName, data); err != nil {
			return fmt.Errorf("resource %s (%s): %w", s.Name, s.Type, err)
		}
	}

	return nil
}

// writeIfPresent writes a template only if the .tmpl file exists.
// Returns nil if the template is absent. Returns an error for any
// other filesystem issue (permissions, I/O error, etc.).
func (g *Generator) writeIfPresent(workDir, tmplName, outputName string, data any) error {
	tmplPath := filepath.Join(g.skillPath, tmplName)
	_, err := os.Stat(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // template not present — skip silently
		}
		return fmt.Errorf("stat template %s: %w", tmplName, err)
	}
	return g.writeTemplate(workDir, tmplName, outputName, data)
}

// writeTemplate loads a template from the skill directory, executes it,
// and writes the result to workDir/outputName.
func (g *Generator) writeTemplate(workDir, tmplName, outputName string, data any) error {
	tmplPath := filepath.Join(g.skillPath, tmplName)
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("skill does not support resource type %q — template %s not found in %s",
				strings.TrimSuffix(tmplName, ".tf.tmpl"), tmplName, g.skillPath)
		}
		return fmt.Errorf("read template %s: %w", tmplName, err)
	}

	tmpl, err := template.New(tmplName).Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse template %s: %w", tmplName, err)
	}

	path := filepath.Join(workDir, outputName)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file %s: %w", path, err)
	}
	defer f.Close()

	return tmpl.Execute(f, data)
}
