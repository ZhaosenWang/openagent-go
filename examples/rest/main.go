// REST example: demonstrates rest.Handler (single agent), rest.TeamHandler
// (multi-agent team), and rest.PlanHandler (goal → DAG → execution) on the
// same http.ServeMux.
//
//	go run ./examples/rest/
//
//	# Single agent
//	curl -X POST localhost:8080/sessions
//	curl -X POST localhost:8080/sessions/<id>/chat -d '{"message":"hello"}'
//
//	# Team
//	curl -X POST localhost:8080/team/sessions
//	curl -X POST localhost:8080/team/sessions/<id>/chat -d '{"message":"hello"}'
//
//	# Plan
//	curl -X POST localhost:8080/plan/sessions
//	curl -X POST localhost:8080/plan/sessions/<id>/generate -d '{"goal":"Write a Go function that reverses a string and includes a test"}'
//	#   → review the returned PlanDef, edit if needed via PUT
//	curl -X POST localhost:8080/plan/sessions/<id>/execute
//	curl -X GET  localhost:8080/plan/sessions/<id>/events    # SSE stream
//	#   → if a step fails, retry or replan:
//	curl -X POST localhost:8080/plan/sessions/<id>/steps/<stepID>/retry
//	curl -X POST localhost:8080/plan/sessions/<id>/replan -d '{"feedback":"avoid file system calls"}'
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/memory/sqlite"
	"github.com/yusheng-g/openagent-go/model/openai"
	"github.com/yusheng-g/openagent-go/rest"
	"github.com/yusheng-g/openagent-go/sandbox/native"
	"github.com/yusheng-g/openagent-go/tool"
)

func main() {
	apiKey := os.Getenv("OPENAGENT_API_KEY")
	modelID := os.Getenv("OPENAGENT_MODEL")
	baseURL := os.Getenv("OPENAGENT_BASE_URL")
	if apiKey == "" || modelID == "" {
		log.Fatal("set OPENAGENT_API_KEY and OPENAGENT_MODEL")
	}

	llm := openai.New(apiKey, modelID, baseURL).WithContextWindow(128_000)

	// ── Memory ──
	mem, err := sqlite.New("./rest-memory.db")
	if err != nil {
		log.Fatalf("memory: %v", err)
	}
	defer mem.Close()

	// ── Sandbox + tools ──
	workDir, _ := filepath.Abs(".")
	sandbox, err := native.New(workDir)
	var sandboxTools []openagent.Tool
	if err == nil {
		sandboxTools = []openagent.Tool{
			tool.NewShell(sandbox, workDir),
			tool.NewReadFile(workDir),
			tool.NewWriteFile(workDir),
			tool.NewListDir(workDir),
			tool.NewGrep(workDir),
		}
	} else {
		log.Printf("WARNING: sandbox unavailable: %v", err)
	}

	// ── Single agent ──
	agent := openagent.NewAgent("assistant",
		openagent.WithModel(llm),
		openagent.WithMemory(mem),
		openagent.WithInstructions("You are a capable assistant. Use shell, read, write, ls, and grep tools to explore, build, and edit the codebase. Be concise and action-oriented."),
		openagent.WithTools(sandboxTools...),
		openagent.WithMaxTurns(10),
	)

	handler := rest.NewHandler(agent)

	// ── Team ──
	researcher := openagent.NewAgent("researcher",
		openagent.WithModel(llm),
		openagent.WithInstructions("Analyze the question and provide key facts. Be thorough but concise. Only respond with your analysis — do not ask follow-up questions."),
		openagent.WithMaxTurns(1),
	)

	writer := openagent.NewAgent("writer",
		openagent.WithModel(llm),
		openagent.WithInstructions("Write a clear, well-structured response based on the task. Use a professional but accessible tone. Only respond with your writing — do not ask follow-up questions."),
		openagent.WithMaxTurns(1),
	)

	teamHandler := rest.NewTeamHandler(nil,
		rest.TeamAgentTemplate{Name: "researcher", Description: "Analyzes questions and finds key facts", Agent: researcher},
		rest.TeamAgentTemplate{Name: "writer", Description: "Writes clear, well-structured reports", Agent: writer},
	)

	// ── Plan ──
	planResearcher := openagent.NewAgent("researcher",
		openagent.WithModel(llm),
		openagent.WithInstructions("You research technical topics thoroughly. Use read/ls/grep tools to explore the codebase, shell to run commands. Be objective and data-driven."),
		openagent.WithMaxTurns(2),
		openagent.WithTools(sandboxTools...),
	)

	planArchitect := openagent.NewAgent("architect",
		openagent.WithModel(llm),
		openagent.WithInstructions("You design software architecture. Use read/ls tools to understand existing code. Only output your design — no follow-up questions."),
		openagent.WithMaxTurns(1),
		openagent.WithTools(sandboxTools...),
	)

	planCoder := openagent.NewAgent("coder",
		openagent.WithModel(llm),
		openagent.WithInstructions("You write production-quality Go code. Use read/write to edit files, grep to search, shell to build and test. Output ONLY code — no explanations outside code comments."),
		openagent.WithMaxTurns(3),
		openagent.WithTools(sandboxTools...),
	)

	planReviewer := openagent.NewAgent("reviewer",
		openagent.WithModel(llm),
		openagent.WithInstructions("You review code for correctness, style, and potential bugs. Use read/grep to examine the code. List specific issues and suggestions. Be constructive."),
		openagent.WithMaxTurns(1),
		openagent.WithTools(sandboxTools...),
	)

	planWriter := openagent.NewAgent("writer",
		openagent.WithModel(llm),
		openagent.WithInstructions("You write clear documentation. Use read/ls to understand the codebase. Use markdown formatting."),
		openagent.WithMaxTurns(1),
		openagent.WithTools(sandboxTools...),
	)

	planHandler := rest.NewPlanHandler(nil, llm,
		rest.PlanAgentTemplate{Name: "researcher", Description: "Researches technical topics — provides comprehensive analysis with pros/cons, alternatives, and data-driven recommendations", Runner: planResearcher},
		rest.PlanAgentTemplate{Name: "architect", Description: "Designs software architecture — produces structured design documents with components, interfaces, and data flow", Runner: planArchitect},
		rest.PlanAgentTemplate{Name: "coder", Description: "Writes production-quality Go code with error handling and comments", Runner: planCoder},
		rest.PlanAgentTemplate{Name: "reviewer", Description: "Reviews code for correctness, style, and potential bugs — produces a list of issues and suggestions", Runner: planReviewer},
		rest.PlanAgentTemplate{Name: "writer", Description: "Writes clear, professional documentation: README, API docs, reports. Uses markdown formatting", Runner: planWriter},
	)

	mux := http.NewServeMux()
	handler.Register(mux)
	teamHandler.Register(mux)
	planHandler.Register(mux)

	log.Println("REST API listening on :8080")
	log.Println()
	log.Println("  Single agent:")
	log.Println("    POST   /sessions              — create session")
	log.Println("    GET    /sessions              — list sessions")
	log.Println("    POST   /sessions/{id}/chat    — send message (SSE)")
	log.Println("    DELETE /sessions/{id}         — delete session")
	log.Println()
	log.Println("  Team:")
	log.Println("    POST   /team/sessions              — create team session")
	log.Println("    GET    /team/sessions              — list team sessions")
	log.Println("    POST   /team/sessions/{id}/chat    — send message (SSE)")
	log.Println("    DELETE /team/sessions/{id}         — delete team session")
	log.Println("    GET    /team/sessions/{id}/agents  — list agents")
	log.Println("    POST   /team/sessions/{id}/agents  — add agent")
	log.Println("    DELETE /team/sessions/{id}/agents  — remove agent")
	log.Println()
	log.Println("  Plan:")
	log.Println("    POST   /plan/sessions                     — create plan session")
	log.Println("    GET    /plan/sessions                     — list plan sessions")
	log.Println("    POST   /plan/sessions/{id}/generate       — {goal: \"...\"} → PlanDef")
	log.Println("    GET    /plan/sessions/{id}/plan           — get current PlanDef")
	log.Println("    PUT    /plan/sessions/{id}/plan           — edit PlanDef")
	log.Println("    POST   /plan/sessions/{id}/execute        — trigger execution (202)")
	log.Println("    GET    /plan/sessions/{id}/events         — SSE progress stream")
	log.Println("    POST   /plan/sessions/{id}/cancel         — cancel execution")
	log.Println("    POST   /plan/sessions/{id}/approve        — tool approval")
	log.Println("    POST   /plan/sessions/{id}/steps/{id}/retry — retry failed step")
	log.Println("    POST   /plan/sessions/{id}/replan         — {feedback} → replan with feedback")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
