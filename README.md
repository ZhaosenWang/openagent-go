# openagent-go

> [中文](README.zh.md) | [Architecture](DESIGN.md) | [架构 (中文)](DESIGN.zh.md)

A fully pluggable, multi-agent AI agent framework in Go.

## Features

- **Pluggable architecture** — every component is an interface: Model, Memory, Tools, Guards, Approver, Hooks, Observer
- **ACP v1 protocol** — full Agent Client Protocol implementation over stdio (JSON-RPC 2.0). Use any ACP-compatible client (VSCode extension, Zed, etc.)
- **Plan mode** — `plan_create`/`plan_update` tools let the agent decompose complex tasks into structured steps with live progress tracking
- **Multi-agent team** — agents hand off tasks via `transfer_to_*` tools; each agent has independent memory, tools, and guard
- **Multi-agent orchestration** — LLM-driven DAG decomposition, parallel execution, and auto-replan via `orchestrate/`
- **Streaming SSE** — real-time token-by-token output, reasoning display, tool call cards
- **Three-layer memory** — Working (token-driven), Compressed (LLM incremental summary via `summarizer/`), Archive (FTS5/vector searchable, never deleted)
- **Sandbox** — native OS-level confinement (Linux bwrap, macOS Seatbelt) for shell, file, and network operations
- **WASM plugins** — agent-level: `agent:tools` and `agent:observers` plug into the tool/observer pipeline. CLI-level: `cli:settings`, `cli:commands`, `cli:observers` for settings injection, command extension, and lifecycle monitoring
- **Static context profiles** — `AGENTS.md` (working rules) and `SOUL.md` (persona & limits) with user-level and project-level resolution
- **Slash commands** — built-in `/help`, `/mode`, `/model`, `/context`, `/cwd`, `/clear`, `/rename`, `/sessions`, extensible via `slash/` registry
- **Full CLI** — `openagent-cli` with cobra commands, config-driven models, keyring secrets, WASM plugin runtime
- **RunHooks with state** — start/end callbacks share opaque state; OTEL spans nest, slog logs duration
- **Dynamic context** — session-level plan status and mode injected into every prompt turn

## Quick Start

```bash
# Build CLI
go build -o openagent-cli ./cmd/cli/

# ACP mode (stdio — for VSCode/Zed ACP plugins)
./openagent-cli serve --acp

# REST mode (HTTP + SSE)
./openagent-cli serve --port 8080

# Frontend
cd examples/frontend/vue-app && npm install && npm run dev
```

### Configuration

Create `~/.openagent/settings.json`:

```json
{
  "provider": {
    "openai": {
      "api_key": "sk-...",
      "models": ["gpt-4o"]
    }
  },
  "profiles": ".openagent/profile"
}
```

Put `AGENTS.md` and `SOUL.md` in `~/.openagent/profile/` or `$(pwd)/.openagent/profile/` to customise the agent's behaviour.

Open `http://localhost:5173` or connect an ACP client — the server supports both protocols.

## Architecture

```
┌──────────────────────────────────────────────┐
│  Agent                                       │
│  ├── Model        (LLM provider)             │
│  ├── Memory       (conversation storage)     │
│  ├── Tools        (shell, file, grep, ...)   │
│  ├── InGuard      (input validation)         │
│  ├── OutGuard     (output validation)        │
│  ├── Approver     (tool call confirmation)   │
│  ├── Hooks        (lifecycle callbacks)      │
│  └── Observer     (pipeline monitoring)      │
└──────────────────────────────────────────────┘
```

## Examples

| Example | Description |
|---------|-------------|
| `examples/basic/` | Minimal agent + model |
| `examples/stream/` | Streaming text deltas |
| `examples/memory/` | Memory + summarizer |
| `examples/team/` | Multi-agent handoff |
| `examples/guard/` | Input/output guards |
| `examples/hooks/` | Lifecycle hooks |
| `examples/observer/` | Pipeline observer |
| `examples/delegate/` | Agent as tool delegation |
| `examples/sandbox/` | Native sandbox tools |
| `examples/plugin/` | WASM tool + observer plugins |
| `examples/skill/` | On-demand skill loading |
| `examples/acp/` | ACP agent protocol (server + client) |
| `examples/iac/` | Multi-agent IaC pipeline |
| `examples/backend/` | Full REST + SSE API server |
| `examples/frontend/vue-app/` | Vue 3 SPA reference UI |
| `cmd/cli/` | Full-featured CLI with WASM plugin runtime |

## Packages

| Package | Purpose |
|---------|---------|
| `openagent` | Core types, Agent, Team, Runner, Memory, Sandbox |
| `acp/sdk/` | ACP v1 protocol SDK — types, JSON-RPC 2.0 mux, client |
| `acp/` | AgentServer — wraps an Agent as an ACP handler |
| `rest/` | REST + SSE handlers (single, team, orchestrate) |
| `orchestrate/` | Multi-agent DAG decomposition + streaming execution |
| `plan/` | `plan_create`/`plan_update` tools (ACP plan mode) |
| `slash/` | Slash command registry and dispatch |
| `summarizer/` | LLM-based incremental conversation compression |
| `memory/sqlite/` | SQLite + FTS5 + vector memory backend |
| `memory/file/` | JSONL file memory backend |
| `model/openai/` | OpenAI ChatCompletion + streaming |
| `tokenizer/` | tiktoken model-aware token counting |
| `sandbox/native/` | OS-level process confinement (bwrap/Seatbelt) |
| `session/` | Session metadata types and store interface |
| `session/sqlite/` | SQLite session store |
| `session/file/` | File-backed session store |
| `eventbus/` | Session-scoped pub/sub for SSE |
| `plugin/wasmhost/` | Shared WASM host module (keyring, HTTP, logging, utc_now) |
| `plugin/agent/wasm/` | Agent-scoped WASM plugin host |
| `plugin/cli/` | CLI plugin manager and types |
| `plugin/cli/wasm/` | CLI-scoped WASM runtime, loader, observer hub |
| `plugin/sdk/rust/` | Rust SDK crate for building WASM plugins |
| `skill/fs/` | Filesystem skill loader |
| `mcp/` | Model Context Protocol client |
| `acp/` | ACP server — Agent→Client RPC tools |
| `guard/llm/` | LLM-based input/output guard |
| `hooks/otel/` | OpenTelemetry hooks |
| `hooks/slog/` | Structured logging hooks |
| `tool/` | Built-in tools (shell, read, write, ls, grep, ACP fs, ACP terminal) |
| `cmd/cli/` | CLI runtime, WASM host, Rust SDK examples |
