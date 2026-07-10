// Standalone ACP client — connects to any ACP agent via stdio and sends a prompt.
//
//	go build -o build/acp-client ./examples/acp/client/
//	./build/acp-client -prompt "calculate 3.14 * 2"
//
// If -server is omitted, it defaults to ./build/acp-server relative to the
// module root.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	openacp "github.com/yusheng-g/openagent-go/acp"
)

func main() {
	serverBin := flag.String("server", "./build/acp-server", "path to ACP server binary")
	prompt := flag.String("prompt", "calculate 12 + 34", "prompt to send")
	flag.Parse()

	client := openacp.NewClient("acp-client", "1.0.0")
	session, err := client.ConnectStdio(context.Background(), *serverBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: connect to %s: %v\n", *serverBin, err)
		os.Exit(1)
	}
	defer session.Close()

	// Handshake.
	initResp, err := session.Initialize(context.Background(), openacp.InitializeRequest{
		ProtocolVersion: 1,
		ClientName:      "acp-client",
		ClientVersion:   "1.0.0",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: initialize: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("connected to %s/%s proto=%d\n\n", initResp.AgentName, initResp.AgentVersion, initResp.ProtocolVersion)

	// New session.
	newResp, err := session.NewSession(context.Background(), openacp.NewSessionRequest{Cwd: "/tmp"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: new session: %v\n", err)
		os.Exit(1)
	}
	sid := newResp.SessionID

	// Register event handler and send prompt.
	session.SetEventHandler(&eventPrinter{})

	fmt.Printf("> %s\n\n", *prompt)
	_, err = session.Prompt(context.Background(), openacp.PromptRequest{
		SessionID: sid,
		Blocks:    []openacp.ContentBlock{{Text: *prompt}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: prompt: %v\n", err)
		os.Exit(1)
	}

	session.CloseSession(context.Background())
}

type eventPrinter struct{}

func (p *eventPrinter) OnAgentMessage(text string)  { fmt.Print(text) }
func (p *eventPrinter) OnAgentThought(text string)   { fmt.Printf("[thought] %s\n", text) }
func (p *eventPrinter) OnToolCall(tc openacp.ToolCallEvent) {
	switch tc.Status {
	case "in_progress":
		fmt.Printf("[tool] %s(%v)\n", tc.Title, tc.RawInput)
	case "completed":
		fmt.Printf("[tool_result] %v\n", tc.RawOutput)
	case "failed":
		fmt.Printf("[tool_failed] %v\n", tc.RawOutput)
	}
}
