package tool

import (
	"context"
	"encoding/json"
	"fmt"

	openagent "github.com/yusheng-g/openagent-go"
	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
)

// ── ACPTerminalCreate ──

// ACPTerminalCreate spawns a command in a new terminal on the client side.
type ACPTerminalCreate struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalCreate(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalCreate {
	return &ACPTerminalCreate{client: client, sessionID: sid}
}

func (t *ACPTerminalCreate) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terminal_create",
		Description: "Create a new terminal on the client's machine and start a command. Returns a terminal ID for use with terminal_output, terminal_wait, terminal_kill, and terminal_release.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command":  { "type": "string", "description": "The command to execute." },
    "args":     { "type": "array", "items": { "type": "string" }, "description": "Command arguments." },
    "cwd":      { "type": "string", "description": "Working directory (must be absolute)." },
    "outputByteLimit": { "type": "integer", "description": "Maximum bytes of output to retain." }
  },
  "required": ["command"]
}`),
	}
}

func (t *ACPTerminalCreate) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Command         string   `json:"command"`
		Args            []string `json:"args"`
		Cwd             *string  `json:"cwd"`
		OutputByteLimit *int     `json:"outputByteLimit"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("terminal_create: %w", err)
	}
	if params.Command == "" {
		return "", fmt.Errorf("terminal_create: command is required")
	}

	resp, err := t.client.CreateTerminal(ctx, openacp.CreateTerminalRequest{
		SessionID:       t.sessionID,
		Command:         params.Command,
		Args:            params.Args,
		Cwd:             params.Cwd,
		OutputByteLimit: params.OutputByteLimit,
	})
	if err != nil {
		return "", fmt.Errorf("terminal_create: %w", err)
	}
	return "Terminal created. ID: " + resp.TerminalID, nil
}

// ── ACPTerminalOutput ──

// ACPTerminalOutput polls the current output of a terminal.
type ACPTerminalOutput struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalOutput(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalOutput {
	return &ACPTerminalOutput{client: client, sessionID: sid}
}

func (t *ACPTerminalOutput) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terminal_output",
		Description: "Get the current output of a running terminal.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "terminalId": { "type": "string", "description": "The terminal ID returned by terminal_create." }
  },
  "required": ["terminalId"]
}`),
	}
}

func (t *ACPTerminalOutput) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("terminal_output: %w", err)
	}

	resp, err := t.client.TerminalOutput(ctx, openacp.TerminalOutputRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return "", fmt.Errorf("terminal_output: %w", err)
	}
	if resp.Truncated {
		return resp.Output + "\n[output truncated]", nil
	}
	return resp.Output, nil
}

// ── ACPTerminalWait ──

// ACPTerminalWait blocks until a terminal command finishes.
type ACPTerminalWait struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalWait(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalWait {
	return &ACPTerminalWait{client: client, sessionID: sid}
}

func (t *ACPTerminalWait) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terminal_wait",
		Description: "Wait for a terminal command to finish and return its exit status.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "terminalId": { "type": "string", "description": "The terminal ID returned by terminal_create." }
  },
  "required": ["terminalId"]
}`),
	}
}

func (t *ACPTerminalWait) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("terminal_wait: %w", err)
	}

	resp, err := t.client.WaitForTerminalExit(ctx, openacp.WaitForTerminalExitRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return "", fmt.Errorf("terminal_wait: %w", err)
	}
	if resp.ExitCode != nil {
		return fmt.Sprintf("Command exited with code %d.", *resp.ExitCode), nil
	}
	if resp.Signal != nil {
		return fmt.Sprintf("Command terminated by signal: %s.", *resp.Signal), nil
	}
	return "Command finished.", nil
}

// ── ACPTerminalKill ──

// ACPTerminalKill terminates a running terminal command without releasing it.
type ACPTerminalKill struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalKill(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalKill {
	return &ACPTerminalKill{client: client, sessionID: sid}
}

func (t *ACPTerminalKill) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terminal_kill",
		Description: "Kill a running terminal command without releasing the terminal. Use terminal_output to get final output, then terminal_release to free resources.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "terminalId": { "type": "string", "description": "The terminal ID returned by terminal_create." }
  },
  "required": ["terminalId"]
}`),
	}
}

func (t *ACPTerminalKill) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("terminal_kill: %w", err)
	}

	_, err := t.client.KillTerminal(ctx, openacp.KillTerminalRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return "", fmt.Errorf("terminal_kill: %w", err)
	}
	return "Command killed.", nil
}

// ── ACPTerminalRelease ──

// ACPTerminalRelease kills the command (if running) and releases all resources.
type ACPTerminalRelease struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalRelease(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalRelease {
	return &ACPTerminalRelease{client: client, sessionID: sid}
}

func (t *ACPTerminalRelease) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "terminal_release",
		Description: "Kill the terminal command (if still running) and release all resources. The terminal ID becomes invalid after this call.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "terminalId": { "type": "string", "description": "The terminal ID returned by terminal_create." }
  },
  "required": ["terminalId"]
}`),
	}
}

func (t *ACPTerminalRelease) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("terminal_release: %w", err)
	}

	_, err := t.client.ReleaseTerminal(ctx, openacp.ReleaseTerminalRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return "", fmt.Errorf("terminal_release: %w", err)
	}
	return "Terminal released.", nil
}
