package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/keyring"
	"github.com/yusheng-g/openagent-go/rest"
	"github.com/yusheng-g/openagent-go/sandbox/native"
	"github.com/yusheng-g/openagent-go/summarizer"
	opentool "github.com/yusheng-g/openagent-go/tool"

	wasm "github.com/yusheng-g/openagent-go/plugin/agent/wasm"
	"github.com/yusheng-g/openagent-go/plugin/wasmhost"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
)

// ── REST server ──

// RunREST starts the REST API server (HTTP + SSE).
func RunREST(ctx context.Context, cfg *config.Config, caps config.Capabilities) error {
	models, modelInfos := buildModels(cfg.Provider)
	m := firstModel(models)

	workDir, _ := os.Getwd()
	sb, err := native.NewWithPolicy(workDir, sandboxPolicy(cfg.Sandbox))
	var tools []openagent.Tool
	if err == nil {
		tools = buildTools(sb, workDir, []string{"shell", "read", "write", "edit", "ls", "grep", "websearch", "webfetch"})
	} else {
		slog.Warn("sandbox unavailable, tools disabled", "error", err)
	}

	// MCP tools from config. Gated by --mcp (default on, --mcp=off disables).
	if caps.OnMCP() {
		mcpTools, mcpCleanup := connectMcpFromConfig(ctx, cfg.McpServers)
		tools = append(tools, mcpTools...)
		defer mcpCleanup()
	}

	profilesDir := resolveProfilesDir(cfg.Profiles)
	ms, knowledge, store, cleanup, err := buildMemory(profilesDir, cfg.Embedding, caps.OnEmbedder())
	if err != nil {
		return err
	}
	defer cleanup()

	opts := []agent.Option{
		agent.WithModel(m),
		agent.WithSystemPrompts(resolveProfiles(cfg.Profiles, "")...),
		agent.WithMaxTurns(100),
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
	}

	if caps.OnSummarizer() && m != nil && caps.OnMemory() {
		ms.WithSummarizer(summarizer.New(m).WithMaxTokens(agentCfg.MaxCompressedTokens))
	}

	if err := applyContextProviders(cfg, &deps); err != nil {
		return err
	}
	// The extractor captures the MemoryProvider it writes to — build it
	// AFTER applyContextProviders so the effective provider is used.
	// Building it earlier would fork writes to the local sqlite store
	// while Recall reads the OpenViking index (silent knowledge loss).
	if caps.OnMemory() && m != nil && deps.MemoryProvider != nil {
		deps.Extractor = ctxpkg.NewAsyncExtractor(ctxpkg.NewLLMExtractor(m, deps.MemoryProvider))
	}
	handler := rest.NewHandler(agentCfg, deps).
		WithSessionStore(store).
		WithCleanupDir(func(sessionID string) {
			// Matches the artifact hook's layout
			// (<ArtifactRoot()>/sess-<sessionID>/) and the REST
			// handler's process dir layout (sess-<id>), so a single
			// call cleans both artifacts and process output.
			// Sanitized like the artifact writer (result.go): a hostile
			// session id must not escape the /tmp/openagent tree.
			dir := filepath.Join(opentool.ArtifactRoot(), "sess-"+openagent.SanitizeName(sessionID))
			_ = os.RemoveAll(dir)
		}).
		WithApproverEnabled(caps.OnApprover()).
		WithProcessDir(opentool.ArtifactRoot())
	handler.StartJanitor(ctx, 1*time.Hour, 24*time.Hour)
	for _, mi := range modelInfos {
		handler.RegisterModel(mi.ID, mi.Model, mi.Provider, mi.APIKey, mi.BaseURL)
	}

	// Plugin manager — loads agent:tools and agent:observers plugins.
	pluginDir := filepath.Join(profilesDir, "plugins")
	mgr := wasm.NewManager(pluginDir).WithHostAPI(wasmhost.NewHostAPI(keyring.NewKeyring()))
	if err := mgr.Discover(ctx); err != nil {
		slog.Warn("plugin discover failed", "error", err)
	} else {
		handler.WithPluginManager(mgr)
	}

	// Start IM channels in the background.
	if err := RunChannels(ctx, agentCfg, deps, cfg.Channels); err != nil {
		slog.Warn("channel error", "error", err)
	}

	mux := http.NewServeMux()
	handler.Register(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{Addr: addr, Handler: withMiddleware(mux)}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		// Bounded shutdown: SSE long-pollers can block Close() forever.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("REST server listening", "addr", addr)
	err = srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// ── Middleware ──

func withMiddleware(next http.Handler) http.Handler {
	return recoveryMiddleware(corsMiddleware(next))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "method", r.Method, "path", r.URL.Path, "error", rec)
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
