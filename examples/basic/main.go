// Basic example: non-streaming Agent with a real Model and one Tool.
// Verifies the full 8-node mainline loop against an actual LLM.
//
// Environment variables:
//
//	OPENAGENT_BASE_URL   — API base URL
//	OPENAGENT_MODEL      — model ID
//	OPENAGENT_API_KEY    — API key
//
//	go run ./examples/basic/
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
		ID:     "basic-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	ctx := context.Background()
	result, err := kernel.New(cfg, deps).Run(ctx, session, openagent.UserMessage("what is 12 + 34?"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=== Run Result ===")
	fmt.Printf("Final output: %s\n", result.FinalOutput)
	fmt.Printf("Turns: %d\n", result.TurnCount)
	fmt.Printf("Tokens: prompt=%d completion=%d total=%d\n",
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
	fmt.Println("\n=== Messages ===")
	for i, msg := range result.Messages {
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				fmt.Printf("[%d] %s → tool_call: %s(%s)\n", i, msg.Role, tc.Function.Name, tc.Function.Arguments)
			}
		} else {
			fmt.Printf("[%d] %s: %s\n", i, msg.Role, truncate(msg.Content, 80))
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
