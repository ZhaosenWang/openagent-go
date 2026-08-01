package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	openagent "github.com/yusheng-g/openagent-go"
)

// wasmTool adapts a WASM tool plugin to openagent.Tool.
type wasmTool struct {
	mod  *module
	meta PluginMeta
}

var _ openagent.Tool = (*wasmTool)(nil)

func (t *wasmTool) Definition() openagent.FunctionDefinition {
	var schemaMap map[string]any
	if len(t.meta.Parameters) > 0 {
		if err := json.Unmarshal(t.meta.Parameters, &schemaMap); err != nil {
			slog.Warn("openagent: wasm tool invalid parameters schema", "tool", t.meta.Name, "error", err)
		}
	}
	return openagent.FunctionDefinition{
		Name:        t.meta.Name,
		Description: t.meta.Description,
		Parameters:  openagent.ParametersFromMap(schemaMap),
	}
}

func (t *wasmTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	input, err := json.Marshal(ToolInput{Args: args})
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("wasm tool %q: marshal input: %w", t.meta.Name, err), false, "")
	}

	output, err := t.mod.invoke(ctx, "execute", input)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("wasm tool %q: %w", t.meta.Name, err), false, "")
	}

	var out ToolOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return openagent.ErrorResult(fmt.Errorf("wasm tool %q: parse output: %w", t.meta.Name, err), false, "")
	}

	if out.Error != "" {
		return openagent.ErrorResult(fmt.Errorf("wasm tool %q: %s", t.meta.Name, out.Error), false, "")
	}
	return &openagent.ToolResult{Content: out.Result}
}
