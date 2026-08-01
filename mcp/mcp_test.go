package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	openagent "github.com/yusheng-g/openagent-go"
)

// echoTool is a simple openagent.Tool for testing.
type echoTool struct{}

func (t *echoTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "echo",
		Description: "Echoes the input message back.",
		Parameters:  openagent.SchemaOf[Test1Params](),
	}
}

func (t *echoTool) Execute(_ context.Context, args json.RawMessage) *openagent.ToolResult {
	var p struct{ Message string }
	json.Unmarshal(args, &p)
	return &openagent.ToolResult{Content: "echo: " + p.Message}
}

func TestServerAddTool(t *testing.T) {
	s := NewServer("test", "1.0.0", nil)
	if err := s.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	// Adding a tool with empty name should fail.
	if err := s.AddTool(&emptyNameTool{}); err == nil {
		t.Fatal("expected error for empty tool name")
	}
}

type emptyNameTool struct{}

func (t *emptyNameTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{Name: ""}
}
func (t *emptyNameTool) Execute(_ context.Context, args json.RawMessage) *openagent.ToolResult {
	return &openagent.ToolResult{}
}

func TestServerClientRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Create server and register a tool.
	server := NewServer("test-server", "1.0.0", nil)
	if err := server.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}

	// Create in-memory transports (bidirectional).
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()

	// Start server in background.
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(ctx, serverTransport)
	}()

	// Connect client.
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, clientTransport)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	// List tools.
	tools, err := session.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Definition().Name != "echo" {
		t.Fatalf("expected tool name 'echo', got %q", tools[0].Definition().Name)
	}

	// Call the tool.
	args, _ := json.Marshal(map[string]string{"message": "hello"})
	result := tools[0].Execute(ctx, args)
	if result.Error != nil {
		t.Fatalf("Execute: %v", result.Error)
	}
	if result.Content != "echo: hello" {
		t.Fatalf("expected 'echo: hello', got %q", result.Content)
	}

	// Close client session.
	session.Close()

	// Server should exit (transport closed).
	if err := <-serverDone; err != nil {
		t.Logf("server exited with: %v", err)
	}
}

func TestSchemaConversion(t *testing.T) {
	def := openagent.FunctionDefinition{
		Name:        "test",
		Description: "a test tool",
		Parameters:  openagent.SchemaOf[Test2Params](),
	}

	mcpTool := ToMCPTool(def)
	if mcpTool.Name != "test" {
		t.Fatalf("name: got %q, want %q", mcpTool.Name, "test")
	}
	if mcpTool.Description != "a test tool" {
		t.Fatalf("desc: got %q, want %q", mcpTool.Description, "a test tool")
	}

	// Round-trip back.
	converted, err := ToFunctionDefinition(*mcpTool)
	if err != nil {
		t.Fatalf("ToFunctionDefinition: %v", err)
	}
	if converted.Name != def.Name {
		t.Fatalf("round-trip name: got %q, want %q", converted.Name, def.Name)
	}
	if converted.Description != def.Description {
		t.Fatalf("round-trip desc: got %q, want %q", converted.Description, def.Description)
	}
}

func TestAddTools(t *testing.T) {
	s := NewServer("test", "1.0.0", nil)
	err := s.AddTools([]openagent.Tool{&echoTool{}})
	if err != nil {
		t.Fatalf("AddTools: %v", err)
	}
}

func TestClientSessionClose(t *testing.T) {
	ctx := context.Background()

	server := NewServer("test", "1.0.0", nil)
	server.AddTool(&echoTool{})

	st, ct := mcpsdk.NewInMemoryTransports()

	go server.Run(ctx, st)

	client := NewClient("test", "1.0.0")
	session, err := client.ConnectTransport(ctx, ct)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type Test1Params struct {
	Message string `json:"message" jsonschema:"description=The message to echo"`
}

type Test2Params struct {
	X int `json:"x,omitempty"`
}
