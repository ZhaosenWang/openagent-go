// Hooks example: demonstrates RunHooks with log/slog for structured observability.
//
//	go run ./examples/hooks/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	sloghooks "github.com/yusheng-g/openagent-go/hooks/slog"
	"github.com/yusheng-g/openagent-go/kernel"
	"github.com/yusheng-g/openagent-go/model/openai"
)

func main() {
	apiKey := os.Getenv("OPENAGENT_API_KEY")
	modelID := os.Getenv("OPENAGENT_MODEL")
	baseURL := os.Getenv("OPENAGENT_BASE_URL")

	model := openai.New(apiKey, modelID, baseURL).
		WithContextWindow(128_000)

	// ── RunHooks: structured logging via log/slog ──
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hooks := sloghooks.New(logger)

	cfg := agent.New("calculator",
		agent.WithModel(model),
		agent.WithSystemPrompts("You are a precise calculator. Use the calculator tool for arithmetic. Answer concisely."),
	)
	deps := kernel.Deps{
		Tools: []openagent.Tool{&calculatorTool{}},
		Hooks: hooks,
	}

	session := openagent.Session{
		ID:     "hooks-session-1",
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
}

// ── Calculator Tool ──

type calculatorTool struct{}

func (t *calculatorTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "calculator",
		Description: "Evaluate a mathematical expression like '12+34' or '100/3'.",
		Parameters:  openagent.SchemaOf[CalcParams](),
	}
}

func (t *calculatorTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	params, err := openagent.ParseArgs[CalcParams](args)
	if err != nil {
		return openagent.ErrorResult(err, false, "")
	}
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

type CalcParams struct {
	Expression string `json:"expression" jsonschema:"description=The math expression to evaluate"`
}
