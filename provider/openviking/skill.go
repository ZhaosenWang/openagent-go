package openviking

import (
	"context"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/skill/fs"
)

// Skill implements provider/skill.Provider backed by OpenViking's skill
// index. Discover returns the full catalog (GET /api/v1/skills);
// Load returns the content the index carries for the skill (the full
// SKILL.md body when the server indexed it, an abstract otherwise — the
// framework contract is "whatever content the provider has", mirroring
// the filesystem provider's body load).
type Skill struct {
	client *Client
	loader *fs.Loader // optional local fallback for full instructions
}

// NewSkill creates the skill provider. loader is optional (used when a
// match carries no embedded content).
func NewSkill(client *Client, loader *fs.Loader) *Skill {
	return &Skill{client: client, loader: loader}
}

// Match implements provider/skill.Provider.
func (s *Skill) Match(ctx context.Context, intent string, limit int) ([]openagent.SkillInfo, error) {
	items, err := s.client.Search(ctx, intent, limit, "skill")
	if err != nil {
		return nil, err
	}
	out := make([]openagent.SkillInfo, 0, len(items))
	for _, it := range items {
		info := openagent.SkillInfo{
			Name:        it.Kind,
			Description: it.Content,
		}
		if id, ok := it.Meta["path"].(string); ok {
			info.Path = id
		}
		if name, ok := it.Meta["name"].(string); ok {
			info.Name = name
		}
		out = append(out, info)
	}
	return out, nil
}

// Discover implements provider/skill.Provider: the full skill catalog
// from OpenViking's GET /api/v1/skills listing endpoint.
func (s *Skill) Discover(ctx context.Context) ([]openagent.SkillInfo, error) {
	entries, err := s.client.ListSkills(ctx, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]openagent.SkillInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, openagent.SkillInfo{
			Name:        e.Name,
			Description: e.Description,
			Path:        e.URI,
		})
	}
	return out, nil
}

// Load implements provider/skill.Provider.
func (s *Skill) Load(ctx context.Context, skill openagent.SkillInfo) (string, error) {
	if s.loader != nil {
		return s.loader.Load(ctx, skill)
	}
	// Fallback: content already embedded in the match (kind carried it).
	return skill.Description, nil
}
