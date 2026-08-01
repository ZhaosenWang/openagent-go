// Package fs implements the filesystem skill backend: a directory tree of
// SKILL.md files. It is consumed via provider/skill.NewFSBridge.
//
// Directory layout:
//
//	<root>/
//	  example-skill/
//	    SKILL.md
//	    scripts/...
//	  another-skill/
//	    SKILL.md
//
// Each SKILL.md begins with YAML frontmatter (--- ... ---) containing at minimum
// name and description. All frontmatter fields are preserved in Frontmatter.
// The body (after the closing ---) is loaded on demand via Load().
package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	openagent "github.com/yusheng-g/openagent-go"
)

// Loader discovers and loads skills from a directory tree.
type Loader struct {
	root string
}

// New creates a Loader rooted at the given directory.
func New(root string) *Loader {
	return &Loader{root: root}
}

// Discover scans root for subdirectories containing SKILL.md, reads each
// file's YAML frontmatter, and returns a SkillInfo for each valid skill.
// Skills missing name or description are skipped.
func (l *Loader) Discover(ctx context.Context) ([]openagent.SkillInfo, error) {
	entries, err := os.ReadDir(l.root)
	if err != nil {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	var skills []openagent.SkillInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(l.root, entry.Name())
		mdPath := filepath.Join(skillDir, "SKILL.md")

		fm, body, err := parseFrontmatter(mdPath)
		if err != nil {
			continue
		}

		name, _ := fm["name"].(string)
		desc, _ := fm["description"].(string)
		if name == "" || desc == "" {
			continue
		}

		skills = append(skills, openagent.SkillInfo{
			Name:        name,
			Description: desc,
			Frontmatter: fm,
			Path:        skillDir,
		})
		_ = body
	}

	return skills, nil
}

// Load reads the SKILL.md for the given skill and returns the body
// (content after the closing YAML frontmatter).
func (l *Loader) Load(ctx context.Context, skill openagent.SkillInfo) (string, error) {
	mdPath := filepath.Join(skill.Path, "SKILL.md")
	_, body, err := parseFrontmatter(mdPath)
	return body, err
}

// parseFrontmatter splits a markdown file into YAML frontmatter (map) and body.
func parseFrontmatter(path string) (map[string]any, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}

	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("no frontmatter")
	}

	// Find closing --- on its own line
	idx := strings.Index(text[4:], "\n---\n")
	closeLen := 5 // "\n---\n"
	if idx == -1 {
		// Closing separator at EOF without a trailing newline ("...\n---").
		// The body after it is empty — slice to len(text), not beyond it.
		if strings.HasSuffix(text[4:], "\n---") {
			idx = len(text[4:]) - 4
			closeLen = 4 // "\n---"
		} else {
			return nil, "", fmt.Errorf("unclosed frontmatter")
		}
	}

	yamlBlock := text[4 : 4+idx]
	body := text[4+idx+closeLen:] // skip the closing separator

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return nil, "", fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	if fm == nil {
		fm = make(map[string]any)
	}

	return fm, body, nil
}
