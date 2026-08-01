// Full-stack backend example: serves REST + SSE endpoints for single-agent,
// team, and plan modes. Designed to work with separate frontend deployment
// (CORS enabled).
//
//	go run ./examples/backend/
//
//	# Environment:
//	OPENAGENT_API_KEY=sk-... OPENAGENT_MODEL=gpt-4o go run ./examples/backend/
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/kernel"
	"github.com/yusheng-g/openagent-go/model/openai"
	memorysqlite "github.com/yusheng-g/openagent-go/provider/memory/sqlite"
	"github.com/yusheng-g/openagent-go/rest"
	"github.com/yusheng-g/openagent-go/sandbox/native"
	sessionsqlite "github.com/yusheng-g/openagent-go/session/sqlite"
	"github.com/yusheng-g/openagent-go/tool"
)

// readOnlyTools returns a filtered set of tools with only read operations —
// no shell, no write. Used by the reviewer agent to prevent accidental
// modifications during code review.
func readOnlyTools(all []openagent.Tool) []openagent.Tool {
	return toolsWithout(all, "shell", "write")
}

// toolsWithout returns a filtered set of tools excluding the named tools.
func toolsWithout(all []openagent.Tool, names ...string) []openagent.Tool {
	skip := make(map[string]bool, len(names))
	for _, n := range names {
		skip[n] = true
	}
	out := make([]openagent.Tool, 0, len(all))
	for _, t := range all {
		if !skip[t.Definition().Name] {
			out = append(out, t)
		}
	}
	return out
}

func main() {
	apiKey := os.Getenv("OPENAGENT_API_KEY")
	modelID := os.Getenv("OPENAGENT_MODEL")
	baseURL := os.Getenv("OPENAGENT_BASE_URL")
	port := os.Getenv("OPENAGENT_PORT")
	if apiKey == "" || modelID == "" {
		log.Fatal("set OPENAGENT_API_KEY and OPENAGENT_MODEL")
	}
	if port == "" {
		port = "8080"
	}

	llm := openai.New(apiKey, modelID, baseURL).WithContextWindow(128_000)

	// ── Memory ──
	ms, err := sessionsqlite.NewMessageStore("./backend-memory.db")
	if err != nil {
		log.Fatalf("message store: %v", err)
	}
	defer ms.Close()
	knowledge, err := memorysqlite.New("./backend-memory.db")
	if err != nil {
		log.Fatalf("knowledge store: %v", err)
	}
	defer knowledge.Close()

	// ── Session metadata store (survives restarts) ──
	sessionStore, err := sessionsqlite.New(ms.DB())
	if err != nil {
		log.Fatalf("session store: %v", err)
	}
	defer sessionStore.Close()

	// ── Sandbox + tools ──
	workDir, _ := filepath.Abs(".")
	sandbox, err := native.New(workDir)
	var sandboxTools []openagent.Tool
	if err == nil {
		sandboxTools = []openagent.Tool{
			tool.NewShell(sandbox),
			tool.NewReadFile(workDir),
			tool.NewWriteFile(workDir),
			tool.NewListDir(workDir),
			tool.NewGrep(workDir),
		}
	} else {
		log.Printf("WARNING: sandbox unavailable: %v", err)
	}

	// ── Single agent ──
	agentCfg := agent.New(
		"assistant", agent.WithModel(llm),
		agent.WithSystemPrompts("You are a capable assistant. Be concise and action-oriented."),
		agent.WithMaxTurns(10),
	)
	agentDeps := kernel.Deps{
		SessionStore:   ms,
		Compressor:     ms,
		MemoryProvider: knowledge,
		Tools:          sandboxTools,
	}
	handler := rest.NewHandler(agentCfg, agentDeps).WithSessionStore(sessionStore)
	// Register additional models so the frontend model selector shows >1 option.
	// These share the same API key and base URL — only the model ID differs.
	if apiKey != "" && baseURL != "" {
		opts := []struct{ id, label string }{
			{"deepseek-v4-flash", "deepseek"},
			{"deepseek-v4-pro", "deepseek"},
		}
		for _, o := range opts {
			handler.RegisterModel(o.id, openai.New(apiKey, o.id, baseURL).WithContextWindow(128_000), o.label, apiKey, baseURL)
		}
	}

	// ── Team ──
	analystCfg := agent.New(
		"analyst", agent.WithModel(llm),
		agent.WithSystemPrompts("You are a requirements analyst. Understand the user's request, break it into clear requirements, then hand off to the designer with a structured spec. Include constraints and acceptance criteria. Use transfer_to_designer when done."),
		agent.WithMaxTurns(2),
	)
	analystDeps := kernel.Deps{
		SessionStore:   ms,
		Compressor:     ms,
		MemoryProvider: knowledge,
		Tools:          readOnlyTools(sandboxTools),
	}
	designerCfg := agent.New(
		"designer", agent.WithModel(llm),
		agent.WithSystemPrompts("You are a software designer. Take the specification from the analyst, design the architecture with components and interfaces. Write a design document to disk. Then hand off to the coder with clear file paths to the design doc. Use transfer_to_coder when done."),
		agent.WithMaxTurns(2),
	)
	designerDeps := kernel.Deps{
		SessionStore:   ms,
		Compressor:     ms,
		MemoryProvider: knowledge,
		Tools:          toolsWithout(sandboxTools, "shell"),
	}
	coderCfg := agent.New(
		"coder", agent.WithModel(llm),
		agent.WithSystemPrompts("You are a software developer. Take the design and IMPLEMENT it by writing actual files to disk. When done, hand off to the reviewer with the list of files you created or modified (full paths). Use transfer_to_reviewer when done."),
		agent.WithMaxTurns(5),
	)
	coderDeps := kernel.Deps{
		SessionStore:   ms,
		Compressor:     ms,
		MemoryProvider: knowledge,
		Tools:          sandboxTools,
	}
	reviewerCfg := agent.New(
		"reviewer", agent.WithModel(llm),
		agent.WithSystemPrompts("You are a code reviewer. Use read to examine the actual code files on disk. Check for correctness, style, and bugs. If issues found, hand off back to the coder with specific feedback referencing exact file paths and line numbers. If approved, produce the final summary. You are the final gate — do NOT hand off further."),
		agent.WithMaxTurns(2),
	)
	reviewerDeps := kernel.Deps{
		SessionStore:   ms,
		Compressor:     ms,
		MemoryProvider: knowledge,
		Tools:          readOnlyTools(sandboxTools),
	}
	teamHandler := rest.NewTeamHandler(ms,
		rest.TeamAgentTemplate{Name: "analyst", Description: "Understands requirements and produces specifications", Agent: analystCfg, Deps: analystDeps},
		rest.TeamAgentTemplate{Name: "designer", Description: "Designs architecture, components, and data flow", Agent: designerCfg, Deps: designerDeps},
		rest.TeamAgentTemplate{Name: "coder", Description: "Writes clean, well-structured code with error handling", Agent: coderCfg, Deps: coderDeps},
		rest.TeamAgentTemplate{Name: "reviewer", Description: "Reviews code for correctness, style, and security", Agent: reviewerCfg, Deps: reviewerDeps},
	).WithSessionStore(sessionStore)
	// Register the same models on the team handler so the frontend model
	// selector works in team mode too. The frontend fetches the list via
	// GET /models (served by the single handler) and sends the chosen
	// modelId/provider on POST /team/sessions/{id}/chat; teamHandler's
	// registry must contain the same entries for lookupModel to resolve.
	if apiKey != "" && baseURL != "" {
		opts := []struct{ id, label string }{
			{"deepseek-v4-flash", "deepseek"},
			{"deepseek-v4-pro", "deepseek"},
		}
		for _, o := range opts {
			teamHandler.RegisterModel(o.id, openai.New(apiKey, o.id, baseURL).WithContextWindow(128_000), o.label)
		}
	}

	// ── Plan agents ──
	planResearcherCfg := agent.New(
		"researcher", agent.WithModel(llm),
		agent.WithSystemPrompts("You research technical topics thoroughly. Use read/ls/grep tools to explore the codebase, shell to run commands. Be objective and data-driven."),
		agent.WithMaxTurns(2),
	)
	planResearcherDeps := kernel.Deps{
		Tools: sandboxTools,
	}
	planArchitectCfg := agent.New(
		"architect", agent.WithModel(llm),
		agent.WithSystemPrompts("You design software architecture. Use read/ls tools to understand existing code. Only output your design — no follow-up questions."),
		agent.WithMaxTurns(1),
	)
	planArchitectDeps := kernel.Deps{
		Tools: sandboxTools,
	}
	planCoderCfg := agent.New(
		"coder", agent.WithModel(llm),
		agent.WithSystemPrompts("You write production-quality code. Use read/write to edit files, grep to search, shell to build and test. Output ONLY code — no explanations outside code comments."),
		agent.WithMaxTurns(3),
	)
	planCoderDeps := kernel.Deps{
		Tools: sandboxTools,
	}
	planReviewerCfg := agent.New(
		"reviewer", agent.WithModel(llm),
		agent.WithSystemPrompts("You review code for correctness, style, and potential bugs. Use read/grep to examine the code. List specific issues and suggestions. Be constructive."),
		agent.WithMaxTurns(1),
	)
	planReviewerDeps := kernel.Deps{
		Tools: sandboxTools,
	}
	planWriterCfg := agent.New(
		"writer", agent.WithModel(llm),
		agent.WithSystemPrompts("You write clear documentation. Use read/ls to understand the codebase. Use markdown formatting."),
		agent.WithMaxTurns(1),
	)
	planWriterDeps := kernel.Deps{
		Tools: sandboxTools,
	}
	orchHandler := rest.NewOrchestrateHandler(nil, llm,
		rest.OrchestrateAgentTemplate{Name: "researcher", Description: "Researches technical topics — provides comprehensive analysis with pros/cons, alternatives, and data-driven recommendations", Cfg: planResearcherCfg, Deps: planResearcherDeps},
		rest.OrchestrateAgentTemplate{Name: "architect", Description: "Designs software architecture — produces structured design documents with components, interfaces, and data flow", Cfg: planArchitectCfg, Deps: planArchitectDeps},
		rest.OrchestrateAgentTemplate{Name: "coder", Description: "Writes production-quality code with error handling and comments", Cfg: planCoderCfg, Deps: planCoderDeps},
		rest.OrchestrateAgentTemplate{Name: "reviewer", Description: "Reviews code for correctness, style, and potential bugs — produces a list of issues and suggestions", Cfg: planReviewerCfg, Deps: planReviewerDeps},
		rest.OrchestrateAgentTemplate{Name: "writer", Description: "Writes clear, professional documentation: README, API docs, reports. Uses markdown formatting", Cfg: planWriterCfg, Deps: planWriterDeps},
	).WithSessionStore(sessionStore)

	// ── Routes ──
	mux := http.NewServeMux()

	// Health check.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	handler.Register(mux)
	teamHandler.Register(mux)
	orchHandler.Register(mux)

	// Frontend static files (optional, env FRONTEND_DIR)
	if frontendDir := os.Getenv("FRONTEND_DIR"); frontendDir != "" {
		root := http.FileServer(http.Dir(frontendDir))
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filepath.Join(frontendDir, "index.html"))
		})
		mux.Handle("GET /{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.PathValue("path")
			if path == "" {
				http.ServeFile(w, r, filepath.Join(frontendDir, "index.html"))
				return
			}
			full := filepath.Join(frontendDir, path)
			if _, err := os.Stat(full); os.IsNotExist(err) {
				http.ServeFile(w, r, filepath.Join(frontendDir, "index.html"))
				return
			}
			root.ServeHTTP(w, r)
		}))
		log.Printf("  Frontend: serving %s", frontendDir)
	}

	// ── Middleware stack (outermost first) ──
	var h http.Handler = mux
	h = corsMiddleware(h)
	h = loggingMiddleware(h)
	h = recoveryMiddleware(h)

	log.Println("Backend listening on :" + port)
	log.Println("  Health:  GET  /health")
	log.Println("  Single:  POST /sessions, GET /sessions, POST /sessions/{id}/chat")
	log.Println("  Team:    POST /team/sessions, ...")
	log.Println("  Plan:    POST /plan/sessions, ...")
	log.Fatal(http.ListenAndServe(":"+port, h))
}

// ── Middleware ──

// recoveryMiddleware catches panics in downstream handlers, logs the stack
// trace, and returns 500 instead of letting the connection hang.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ── Middleware ──

// corsMiddleware sets permissive CORS headers for development.
// In production, restrict Access-Control-Allow-Origin to the frontend's origin.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs each request with method, path, status, and duration.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s → %d (%s)", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Microsecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher so SSE streaming works through the
// logging middleware. http.Flusher is not part of http.ResponseWriter,
// so embedding the interface does not automatically promote it.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
