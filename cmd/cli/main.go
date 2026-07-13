// openagent CLI — single-agent chat and autonomous goal mode.
//
//	go run ./cmd/cli/ run "What is 15+27?"
//	go run ./cmd/cli/ goal "Fix all failing tests"
//
// Or build and install:
//
//	go build -o openagent ./cmd/cli/
//	./openagent run "Hello"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/memory/sqlite"
	"github.com/yusheng-g/openagent-go/model/openai"
	"github.com/yusheng-g/openagent-go/sandbox/native"
	opentool "github.com/yusheng-g/openagent-go/tool"
)

func main() {
	log.SetFlags(0)

	// ── Flags ──
	var (
		modelID  = envOr("OPENAGENT_MODEL", "gpt-4o")
		apiKey   = envOr("OPENAGENT_API_KEY", "")
		baseURL  = os.Getenv("OPENAGENT_BASE_URL")
		memPath  string
		workDir  string
		maxTurns int
		toolList string
	)
	flag.StringVar(&modelID, "model", modelID, "Model ID")
	flag.StringVar(&apiKey, "api-key", apiKey, "API key")
	flag.StringVar(&baseURL, "base-url", baseURL, "Base URL")
	flag.StringVar(&memPath, "memory", "", "SQLite memory path (empty = no persistence)")
	flag.StringVar(&workDir, "workspace", "", "Workspace root (empty = current dir)")
	flag.IntVar(&maxTurns, "max-turns", 0, "Max turns (0 = default 20, goal mode uses 50)")
	flag.StringVar(&toolList, "tools", "shell,read,write,ls,grep", "Built-in tools to enable (comma-separated)")
	flag.Parse()

	if apiKey == "" {
		log.Fatal("OPENAGENT_API_KEY not set. Use --api-key or set the environment variable.")
	}

	cmd := flag.Arg(0)
	if cmd != "run" && cmd != "goal" {
		fmt.Fprintf(os.Stderr, "Usage: openagent <run|goal> [flags] <message>\n\n")
		fmt.Fprintf(os.Stderr, "  openagent run \"What is 15+27?\"\n")
		fmt.Fprintf(os.Stderr, "  openagent goal \"Fix all failing tests\"\n")
		os.Exit(2)
	}

	input := strings.Join(flag.Args()[1:], " ")
	if input == "" {
		log.Fatal("no input message provided")
	}

	// ── Setup ──
	if workDir == "" {
		workDir, _ = filepath.Abs(".")
	}

	llm := openai.New(apiKey, modelID, baseURL).WithContextWindow(128_000)

	var mem openagent.Memory
	if memPath != "" {
		var err error
		mem, err = sqlite.New(memPath)
		if err != nil {
			log.Fatalf("open memory: %v", err)
		}
	}

	tools := buildTools(workDir, toolList)

	instructions := `You are a precise, no-nonsense assistant running in a CLI.
- Answer concisely. Don't explain unless asked.
- If you need to explore, do it in one or two well-targeted commands — don't retry the same thing.
- When a tool returns an error, read the error and adjust. Don't repeat the same failing call.
- Use relative paths only (the workspace is the current directory).
- Stop when the task is done. Don't keep exploring.`

	agent := openagent.NewAgent("cli",
		openagent.WithModel(llm),
		openagent.WithInstructions(instructions),
		openagent.WithMemory(mem),
		openagent.WithTools(tools...),
		openagent.WithMaxTurns(20),
	)

	session := openagent.Session{
		ID:   "cli",
		UserID: "user",

		ModelID: modelID,
		ProjectContext: fmt.Sprintf("Workspace: %s", workDir),
	}

	// ── Execute ──
	ctx := context.Background()

	switch cmd {
	case "run":
		printStream(agent.RunStream(ctx, session, openagent.UserMessage(input)), true)

	case "goal":
		if maxTurns == 0 {
			maxTurns = 50
		}
		agent = openagent.NewAgent("cli",
			openagent.WithModel(llm),
			openagent.WithInstructions(instructions),
			openagent.WithMemory(mem),
			openagent.WithTools(tools...),
			openagent.WithMaxTurns(maxTurns),
		)
		printStream(agent.RunGoalStream(ctx, session, input), false)
	}
}

func printStream(ch <-chan openagent.StreamEvent, fatal bool) {
	for evt := range ch {
		switch evt.Type {
		case openagent.StreamTextDelta:
			fmt.Print(evt.Text)
		case openagent.StreamToolCall:
			if len(evt.Message.ToolCalls) > 0 {
				fmt.Printf("\n🔧 %s", evt.Message.ToolCalls[0].Function.Name)
			}
		case openagent.StreamToolResult:
			text := evt.Message.Content
			if len(text) > 500 {
				text = text[:500] + "..."
			}
			fmt.Printf(" → %s\n", text)
		case openagent.StreamDone:
			fmt.Println()
		case openagent.StreamError:
			if fatal { log.Fatalf("error: %v", evt.Error) } else { log.Printf("error: %v", evt.Error) }
		case openagent.StreamAborted:
			if fatal { log.Fatalf("aborted: %v", evt.Error) } else { log.Printf("aborted: %v", evt.Error) }
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func buildTools(workDir, toolList string) []openagent.Tool {
	var tools []openagent.Tool
	enabled := make(map[string]bool)
	for _, name := range strings.Split(toolList, ",") {
		enabled[strings.TrimSpace(name)] = true
	}

	// Shell tool needs native sandbox.
	if enabled["shell"] {
		sandbox, err := native.New(workDir)
		if err == nil {
			tools = append(tools, opentool.NewShell(sandbox, workDir))
		} else {
			log.Printf("WARNING: sandbox unavailable (%v), shell tool disabled", err)
		}
	}

	// File tools.
	if enabled["read"] {
		tools = append(tools, opentool.NewReadFile(workDir))
	}
	if enabled["write"] {
		tools = append(tools, opentool.NewWriteFile(workDir))
	}
	if enabled["ls"] {
		tools = append(tools, opentool.NewListDir(workDir))
	}
	if enabled["grep"] {
		tools = append(tools, opentool.NewGrep(workDir))
	}

	return tools
}
