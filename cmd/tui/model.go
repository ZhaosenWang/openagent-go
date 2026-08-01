package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/kernel"
)

// ── Styles ──

var (
	styleUser  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleAgent = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	styleTool  = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Italic(true)
	styleError = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleHelp  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleModal = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("1")).
			Padding(1, 2)
)

// ── State ──

type tuiState int

const (
	stateIdle tuiState = iota
	stateStreaming
	stateApproving
)

// ── Messages ──

type chatMsg struct {
	role    string // "user", "agent", "tool", "error"
	content string
	tool    string
}

// ── Stream messages (bubbletea commands) ──

type streamStartMsg struct {
	evt openagent.StreamEvent
	ch  <-chan openagent.StreamEvent
}
type streamEventMsg struct{ evt openagent.StreamEvent }
type streamEndMsg struct{}

type approveMsg struct{ req approveRequest }

// ── Model ──

type model struct {
	viewport viewport.Model
	textarea textarea.Model

	rt      *kernel.Runtime
	session openagent.Session

	messages []chatMsg

	// Streaming
	streamCh  <-chan openagent.StreamEvent
	streamBuf string

	// Approval
	approveCh      <-chan approveRequest
	pendingApprove *approveRequest

	state  tuiState
	width  int
	height int
	err    error
}

func newModel(rt *kernel.Runtime, session openagent.Session, approveCh <-chan approveRequest) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter to send)"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.SetContent("Welcome! Try asking the assistant to do a calculation.\n")

	return model{
		viewport:  vp,
		textarea:  ta,
		rt:        rt,
		session:   session,
		messages:  make([]chatMsg, 0),
		approveCh: approveCh,
		state:     stateIdle,
	}
}

// ── Init ──

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.textarea.Focus(),
		listenForApproval(m.approveCh),
	)
}

// ── Update ──

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.SetWidth(msg.Width)
		m.textarea.SetWidth(msg.Width - 2)
		m.viewport.SetHeight(msg.Height - 4) // 3 for textarea + 1 for help
		m.updateViewport()
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		}

		// State-dependent routing
		switch m.state {
		case stateApproving:
			return m.handleApprovalKey(msg)
		case stateStreaming:
			return m, nil // ignore keys while streaming (except ctrl+c above)
		}

		// Idle state: handle send, forward rest to textarea
		if msg.String() == "enter" {
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}
			m.textarea.Reset()
			m.addMsg(chatMsg{role: "user", content: input})
			m.state = stateStreaming
			m.streamBuf = ""
			m.err = nil
			m.updateViewport()
			return m, tea.Batch(
				runAgentCmd(m.rt, m.session, openagent.UserMessage(input)),
				listenForApproval(m.approveCh),
			)
		}

		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd

	case streamStartMsg:
		m.streamCh = msg.ch
		return m, m.handleStreamEvent(msg.evt)

	case streamEventMsg:
		return m, m.handleStreamEvent(msg.evt)

	case streamEndMsg:
		m.state = stateIdle
		m.updateViewport()
		return m, m.textarea.Focus()

	case approveMsg:
		m.state = stateApproving
		m.pendingApprove = &msg.req
		m.updateViewport()
		return m, nil

	case error:
		m.err = msg
		m.state = stateIdle
		m.addMsg(chatMsg{role: "error", content: msg.Error()})
		m.updateViewport()
		return m, m.textarea.Focus()
	}

	return m, nil
}

// ── Stream handling ──

func (m *model) handleStreamEvent(evt openagent.StreamEvent) tea.Cmd {
	switch evt.Type {
	case openagent.StreamTextDelta:
		m.streamBuf += evt.Text
		m.updateViewport()

	case openagent.StreamToolCall:
		if m.streamBuf != "" {
			m.addMsg(chatMsg{role: "agent", content: m.streamBuf})
			m.streamBuf = ""
		}
		if len(evt.Message.ToolCalls) > 0 {
			tc := evt.Message.ToolCalls[0]
			b, _ := json.Marshal(tc.Function)
			m.addMsg(chatMsg{role: "tool", content: string(b), tool: tc.Function.Name})
		}

	case openagent.StreamToolResult:
		m.addMsg(chatMsg{role: "agent", content: "  ← " + evt.Message.Content})

	case openagent.StreamDone:
		if m.streamBuf != "" {
			m.addMsg(chatMsg{role: "agent", content: m.streamBuf})
			m.streamBuf = ""
		}
		m.updateViewport()

	case openagent.StreamError:
		m.addMsg(chatMsg{role: "error", content: evt.Error.Error()})
		m.streamBuf = ""

	case openagent.StreamRetrying:
		m.streamBuf += fmt.Sprintf("\n(retrying: %v)\n", evt.Error)
		m.updateViewport()
	}

	// Channel close after done/error triggers streamEndMsg.
	if m.state == stateStreaming {
		return awaitStream(m.streamCh)
	}
	return nil
}

// ── Approval handling ──

func (m *model) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.pendingApprove.respond <- approveResponse{allowed: true, reason: "user approved"}
		m.pendingApprove = nil
		m.state = stateStreaming
		m.updateViewport()
		return m, awaitStream(m.streamCh)
	case "n", "N":
		m.pendingApprove.respond <- approveResponse{allowed: false, reason: "user denied"}
		m.pendingApprove = nil
		m.state = stateStreaming
		m.updateViewport()
		return m, awaitStream(m.streamCh)
	}
	return m, nil
}

// ── Messages ──

func (m *model) addMsg(msg chatMsg) {
	m.messages = append(m.messages, msg)
}

func (m *model) updateViewport() {
	m.viewport.SetContent(m.renderMessages())
	m.viewport.GotoBottom()
}

func (m *model) renderMessages() string {
	var sb strings.Builder

	for _, msg := range m.messages {
		switch msg.role {
		case "user":
			sb.WriteString(styleUser.Render("▸ You:"))
			sb.WriteString(" ")
			sb.WriteString(msg.content)
			sb.WriteString("\n")
		case "agent":
			sb.WriteString(styleAgent.Render("▸ Assistant:"))
			sb.WriteString(" ")
			sb.WriteString(msg.content)
			sb.WriteString("\n")
		case "tool":
			sb.WriteString(styleTool.Render(fmt.Sprintf("  🔧 %s(%s)", msg.tool, msg.content)))
			sb.WriteString("\n")
		case "error":
			sb.WriteString(styleError.Render("✖ " + msg.content))
			sb.WriteString("\n")
		}
	}

	// Streaming buffer
	if m.streamBuf != "" {
		sb.WriteString(styleAgent.Render("▸ Assistant:"))
		sb.WriteString(" ")
		sb.WriteString(m.streamBuf)
	}

	// Approval modal
	if m.pendingApprove != nil {
		tc := m.pendingApprove.call
		modal := fmt.Sprintf("⚡ Approve: %s(%s)\n[Y]es  [N]o", tc.Function.Name, strings.TrimSpace(tc.Function.Arguments))
		sb.WriteString("\n")
		sb.WriteString(styleModal.Render(modal))
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		sb.WriteString(styleHelp.Render("Type a message and press Enter..."))
	}

	return sb.String()
}

// ── Help text ──

func (m model) helpText() string {
	switch m.state {
	case stateApproving:
		return "Y/N to approve • ctrl+c quit"
	case stateStreaming:
		return "waiting for response... • ctrl+c quit"
	default:
		if m.err != nil {
			return styleError.Render(fmt.Sprintf("Error: %v", m.err)) + " • ctrl+c quit"
		}
		return "enter to send • ctrl+c quit"
	}
}

// ── View ──

func (m *model) View() tea.View {
	content := lipgloss.JoinVertical(
		lipgloss.Top,
		m.viewport.View(),
		m.textarea.View(),
		styleHelp.Render(m.helpText()),
	)
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// ── Commands ──

func runAgentCmd(rt *kernel.Runtime, session openagent.Session, input openagent.Message) tea.Cmd {
	return func() tea.Msg {
		ch := rt.RunStreamWithPrefix(context.Background(), session, nil, input)
		evt, ok := <-ch
		if !ok {
			return streamEndMsg{}
		}
		return streamStartMsg{evt: evt, ch: ch}
	}
}

func awaitStream(ch <-chan openagent.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return streamEndMsg{}
		}
		return streamEventMsg{evt: evt}
	}
}

func listenForApproval(ch <-chan approveRequest) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return approveMsg{req: req}
	}
}

// ── Tools ──

type calculatorTool struct{}

func (t *calculatorTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "calculator",
		Description: "Evaluate a math expression like '15+27' or '100/3'.",
		Parameters:  openagent.SchemaOf[Tui1Params](),
	}
}

func (t *calculatorTool) Execute(_ context.Context, args json.RawMessage) *openagent.ToolResult {
	var p struct{ Expression string }
	json.Unmarshal(args, &p)
	expr := strings.ReplaceAll(p.Expression, " ", "")
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
	}
	return openagent.ErrorResult(fmt.Errorf("unsupported operator: %c", op), false, "")
}

type echoTool struct{}

func (t *echoTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "echo",
		Description: "Echoes the input message back.",
		Parameters:  openagent.SchemaOf[Tui2Params](),
	}
}

func (t *echoTool) Execute(_ context.Context, args json.RawMessage) *openagent.ToolResult {
	var p struct{ Message string }
	json.Unmarshal(args, &p)
	return &openagent.ToolResult{Content: fmt.Sprintf("you said: %s", p.Message)}
}

type Tui1Params struct {
	Expression string `json:"expression"`
}

type Tui2Params struct {
	Message string `json:"message"`
}
