package server

import (
	"context"
	"fmt"
	"os"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
	"github.com/yusheng-g/openagent-go/kernel"
	"github.com/yusheng-g/openagent-go/sandbox/native"
	"github.com/yusheng-g/openagent-go/summarizer"
)

// RunCLI runs a one-shot chat turn with streaming output to stdout.
// Same memory wiring as the servers: the conversation is persisted under a
// fresh session id (each run is its own session), and durable knowledge is
// extracted after the run and recalled across runs (user-level scope), so
// "run" participates in the knowledge closed loop.
func RunCLI(ctx context.Context, cfg *config.Config, message string) error {
	// 1. Build model from config (unexported: buildModels, firstModel)
	models, _ := buildModels(cfg.Provider)
	m := firstModel(models)
	if m == nil {
		return fmt.Errorf("no models configured. Please add a provider in ~/.openagent/settings.json")
	}

	// 2. Memory + knowledge (same wiring as RunACP/RunREST).
	// Capabilities come from settings.json like every other mode — run is
	// not special-cased to defaults-on.
	caps := cfg.Capabilities
	profilesDir := resolveProfilesDir(cfg.Profiles)
	ms, knowledge, _, cleanup, err := buildMemory(profilesDir, cfg.Embedding, caps.OnEmbedder())
	if err != nil {
		return err
	}
	defer cleanup()

	// 3. System prompts (unexported: resolveProfiles)
	prompts := resolveProfiles(cfg.Profiles, "")

	// 4. Sandbox + standard tools (unexported: sandboxPolicy, buildTools)
	workDir, _ := os.Getwd()
	policy := sandboxPolicy(cfg.Sandbox)
	sb, err := native.NewWithPolicy(workDir, policy)
	var tools []openagent.Tool
	if err == nil {
		tools = buildTools(sb, workDir, []string{"shell", "read", "write", "ls", "grep"})
	} else {
		fmt.Fprintf(os.Stderr, "sandbox unavailable, tools disabled: %v\n", err)
	}

	// 5. Construct agent config (pure) + runtime deps.
	opts := []agent.Option{
		agent.WithModel(m),
		agent.WithSystemPrompts(prompts...),
		agent.WithMaxTurns(50),
	}
	opts, skillProvider := buildOpts(opts, caps, m)
	agentCfg := agent.New("openagent", opts...)

	deps := buildRuntimeDeps(caps, cfg.Sensitive)
	deps.Tools = tools
	deps.SkillProvider = skillProvider
	if caps.OnMemory() {
		deps.SessionStore = ms
		deps.Compressor = ms
		deps.MemoryProvider = knowledge
		// One shared background extractor: knowledge from this run is
		// stored and recalled by later runs (and by the servers sharing
		// this db).
		deps.Extractor = ctxpkg.NewAsyncExtractor(ctxpkg.NewLLMExtractor(m, knowledge))
	}
	if caps.OnMemory() && caps.OnSummarizer() && m != nil {
		ms.WithSummarizer(summarizer.New(m).WithMaxTokens(agentCfg.MaxCompressedTokens))
	}

	// 6. Fresh session per run (no cross-run conversation history, but
	// durable knowledge is user-level and carries across runs).
	session := openagent.Session{
		ID:        fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		CreatedAt: time.Now(),
	}

	// 7. Run and stream events to terminal
	ch := kernel.New(agentCfg, deps).RunStream(ctx, session, openagent.UserMessage(message))
	for evt := range ch {
		switch evt.Type {
		case openagent.StreamTextDelta:
			fmt.Print(evt.Text)

		case openagent.StreamDone:
			fmt.Println()
			if evt.Result != nil {
				u := evt.Result.Usage
				fmt.Fprintf(os.Stderr, "─── %d prompt + %d completion = %d tokens, %d turns\n",
					u.PromptTokens, u.CompletionTokens, u.TotalTokens,
					evt.Result.TurnCount)
			}

		case openagent.StreamError:
			return evt.Error

		case openagent.StreamAborted:
			if evt.Error != nil {
				return evt.Error
			}
			return ctx.Err()
		}
	}
	return nil
}
