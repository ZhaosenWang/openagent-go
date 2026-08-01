// Stream example: streaming Agent with real-time output via RunStream.
// Demonstrates text_delta, tool_call, tool_result, retrying, and done events.
//
// Environment variables:
//
//	OPENAGENT_BASE_URL   — API base URL
//	OPENAGENT_MODEL      — model ID
//	OPENAGENT_API_KEY    — API key
//
//	go run ./examples/stream/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/kernel"
	"github.com/yusheng-g/openagent-go/model/openai"
)

func main() {
	apiKey := os.Getenv("OPENAGENT_API_KEY")
	modelID := os.Getenv("OPENAGENT_MODEL")
	baseURL := os.Getenv("OPENAGENT_BASE_URL")

	model := openai.New(apiKey, modelID, baseURL).
		WithContextWindow(128_000)

	cfg := agent.New("calculator",
		agent.WithModel(model),
		agent.WithSystemPrompts("You are a precise calculator. Use the calculator tool for arithmetic. Answer concisely."),
	)
	deps := kernel.Deps{
		Tools: []openagent.Tool{&calculatorTool{}},
	}

	session := openagent.Session{
		ID:     "stream-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	fmt.Printf("Model: %s | Base URL: %s\n\n", modelID, baseURL)

	ctx := context.Background()
	events := kernel.New(cfg, deps).RunStream(ctx, session, openagent.UserMessage("what is 12 + 34?"))

	var inThought bool
	fmt.Print("Assistant: ")
	for event := range events {
		switch event.Type {
		case openagent.StreamThought:
			if !inThought {
				fmt.Println()
				fmt.Print("🧠 Thinking: ")
				inThought = true
			}
			fmt.Print(event.Text)

		case openagent.StreamTextDelta:
			if inThought {
				fmt.Println()
				fmt.Print("Assistant: ")
				inThought = false
			}
			fmt.Print(event.Text) // real-time character output

		case openagent.StreamToolCall:
			for _, tc := range event.Message.ToolCalls {
				fmt.Printf("\n🔧 calling %s(%s)...\n", tc.Function.Name, tc.Function.Arguments)
			}

		case openagent.StreamToolResult:
			fmt.Printf("📦 %s\n", truncate(event.Message.Content, 120))
			fmt.Print("Assistant: ")

		case openagent.StreamRetrying:
			fmt.Printf("\n⏳ retrying: %v\n", event.Error)
			fmt.Print("Assistant: ")

		case openagent.StreamDone:
			r := event.Result
			fmt.Printf("\n\n=== Done ===\n")
			fmt.Printf("Turns: %d | Tokens: prompt=%d completion=%d total=%d\n",
				r.TurnCount, r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.TotalTokens)

		case openagent.StreamError:
			fmt.Fprintf(os.Stderr, "\nERROR: %v\n", event.Error)
			os.Exit(1)
		}
	}
}

// ── Calculator Tool ──

type calculatorTool struct{}

func (t *calculatorTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "calculator",
		Description: "Evaluate a mathematical expression. Input is a valid arithmetic expression like '12+34' or '100/3'.",
		Parameters:  openagent.SchemaOf[CalcParams](),
	}
}

func (t *calculatorTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[CalcParams](args)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
	// Remove spaces — model may produce "12 + 34" or "12+34"
	expr := strings.ReplaceAll(params.Expression, " ", "")
	var a, b int
	var op rune
	fmt.Sscanf(expr, "%d%c%d", &a, &op, &b)
	switch op {
	case '+':
		return &openagent.ToolResult{Content: fmt.Sprintf("%d", a+b)}
	case '-':
		return &openagent.ToolResult{Content: fmt.Sprintf("%d", a-b)}
	case '*':
		return &openagent.ToolResult{Content: fmt.Sprintf("%d", a*b)}
	case '/':
		if b == 0 {
			return openagent.ErrorResult(fmt.Errorf("division by zero"), false, "")
		}
		return &openagent.ToolResult{Content: fmt.Sprintf("%d", a/b)}
	default:
		return openagent.ErrorResult(fmt.Errorf("unsupported operator: %c", op), false, "")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type CalcParams struct {
	Expression string `json:"expression" jsonschema:"description=The math expression to evaluate"`
}
