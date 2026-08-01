// Package mcp integrates openagent-go with the Model Context Protocol (MCP).
//
// It provides:
//   - Server: expose openagent.Tool instances as MCP tools
//   - Client: import MCP server tools as openagent.Tool instances
//
// Import as:
//
//	openmcp "github.com/yusheng-g/openagent-go/mcp"
package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	openagent "github.com/yusheng-g/openagent-go"
)

// ToMCPTool converts an openagent FunctionDefinition to an MCP Tool.
// The InputSchema is passed through as-is (json.RawMessage is valid JSON Schema).
func ToMCPTool(def openagent.FunctionDefinition) *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: def.Parameters,
	}
}

// ToFunctionDefinition converts an MCP Tool to an openagent FunctionDefinition.
// The MCP InputSchema (a JSON Schema map) is normalized into the neutral
// Parameters model.
func ToFunctionDefinition(t mcpsdk.Tool) (openagent.FunctionDefinition, error) {
	return openagent.FunctionDefinition{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  openagent.ParametersFromMap(toMap(t.InputSchema)),
	}, nil
}

// toMap type-asserts an arbitrary schema value (MCP InputSchema is any)
// to a map; empty map when it isn't one.
func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
