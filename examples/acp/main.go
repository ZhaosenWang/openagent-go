// ACP example — spawns the calculator server and demonstrates the full
// ACP session lifecycle: Initialize → NewSession → Prompt (with tool calls)
// → ListSessions → LoadSession → Prompt → CloseSession.
//
//	go build -o build/acp-server ./examples/acp/server/
//	go run ./examples/acp/
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
	flag.Parse()

	// 1. Connect to the server (spawns it as a subprocess over stdio).
	client := openacp.NewClient("acp-example", "1.0.0")
	session, err := client.ConnectStdio(context.Background(), *serverBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: connect: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	fmt.Print("=== ACP Client-Server Example ===\n\n")

	// 2. Initialize (handshake).
	initResp, err := session.Initialize(context.Background(), openacp.InitializeRequest{
		ProtocolVersion: 1,
		ClientName:      "acp-example",
		ClientVersion:   "1.0.0",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: initialize: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("🤝 handshake: agent=%s/%s proto=%d\n\n",
		initResp.AgentName, initResp.AgentVersion, initResp.ProtocolVersion)

	// 3. Create a new session.
	newResp, err := session.NewSession(context.Background(), openacp.NewSessionRequest{
		Cwd: "/tmp",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: new session: %v\n", err)
		os.Exit(1)
	}
	sid := newResp.SessionID
	fmt.Printf("📂 new session: %s\n\n", sid)

	// 4. Register event handler.
	handler := &eventPrinter{}
	session.SetEventHandler(handler)

	// 5. Calculator prompt (demonstrates tool calls).
	fmt.Println("── Prompt #1: calculate ──")
	_, err = session.Prompt(context.Background(), openacp.PromptRequest{
		SessionID: sid,
		Blocks:    []openacp.ContentBlock{{Text: "calculate 123 * 456"}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: prompt: %v\n", err)
	}
	fmt.Println()

	// 6. Plain text prompt.
	fmt.Println("── Prompt #2: plain text ──")
	handler.reset()
	_, err = session.Prompt(context.Background(), openacp.PromptRequest{
		SessionID: sid,
		Blocks:    []openacp.ContentBlock{{Text: "hello from ACP"}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: prompt: %v\n", err)
	}
	fmt.Println()

	// 7. List sessions.
	listResp, err := session.ListSessions(context.Background(), openacp.ListSessionsRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: list sessions: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("📋 list sessions: %d found\n", len(listResp.Sessions))
	for _, s := range listResp.Sessions {
		fmt.Printf("   - %s  cwd=%s  title=%q\n", s.SessionID, s.Cwd, s.Title)
	}
	fmt.Println()

	// 8. Load the session (simulates reconnect).
	_, err = session.LoadSession(context.Background(), openacp.LoadSessionRequest{
		SessionID: sid,
		Cwd:       "/tmp",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load session: %v\n", err)
	} else {
		fmt.Printf("🔄 loaded session: %s\n\n", sid)
	}

	// 9. Prompt on the loaded session.
	fmt.Println("── Prompt #3: on loaded session ──")
	handler.reset()
	_, err = session.Prompt(context.Background(), openacp.PromptRequest{
		SessionID: sid,
		Blocks:    []openacp.ContentBlock{{Text: "what is 100 / 7"}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: prompt: %v\n", err)
	}
	fmt.Println()

	// 10. Close session.
	if err := session.CloseSession(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: close session: %v\n", err)
	}
	fmt.Println("🔒 session closed")

	if s := session.Stderr(); s != "" {
		fmt.Printf("\n── server stderr ──\n%s", s)
	}
}

// ── eventPrinter ──

type eventPrinter struct{}

func (p *eventPrinter) reset() {}

func (p *eventPrinter) OnAgentMessage(text string) {
	fmt.Printf("  📝 message: %s", text)
}

func (p *eventPrinter) OnAgentThought(text string) {
	fmt.Printf("  💭 thought: %s\n", text)
}

func (p *eventPrinter) OnToolCall(tc openacp.ToolCallEvent) {
	switch tc.Status {
	case "in_progress":
		fmt.Printf("  🔧 tool_call: %s(%v)\n", tc.Title, tc.RawInput)
	case "completed":
		fmt.Printf("  ✅ tool_result [%s]: %v\n", tc.ID, tc.RawOutput)
	case "failed":
		fmt.Printf("  ❌ tool_failed [%s]: %v\n", tc.ID, tc.RawOutput)
	}
}
