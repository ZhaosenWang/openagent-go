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
- **Structured tool results** — `ToolResult` carries content/JSON/error/truncation; oversized output spills to disk automatically (line-wrapped, read/grep-friendly) instead of flooding the model context
- **Approval policy engine** — layered chain (rules → safety → approval memory → human) with argument editing and persistent "always allow" decisions
- **Self-evolution** — LLM extractor turns finished runs into durable knowledge, recalled into later sessions
- **Three-layer memory** — Working (token-driven), Compressed (LLM incremental summary via `summarizer/`), Archive (FTS5/vector searchable, never deleted); all three are provider-pluggable, including a remote OpenViking context database
- **Sandbox** — native OS-level confinement (Linux bwrap, macOS Seatbelt) for shell, file, and network operations
- **WASM plugins** — agent-level: `agent:tools` and `agent:observers` plug into the tool/observer pipeline. CLI-level: `cli:settings`, `cli:commands`, `cli:observers` for settings injection, command extension, and lifecycle monitoring
- **Static context profiles** — `AGENTS.md` (working rules) and `SOUL.md` (persona & limits) with user-level and project-level resolution
- **Slash commands** — built-in `/help`, `/mode`, `/model`, `/context`, `/cwd`, `/clear`, `/rename`, `/sessions`, extensible via `slash/` registry
- **Full CLI** — `openagent-cli` with cobra commands, config-driven models, keyring secrets, WASM plugin runtime
- **IM channels** — Feishu/Lark (WebSocket, card-based streaming output with markdown and tool call cards, one-click QR setup), personal WeChat (Tencent ilinkai channel, QR login with pairing code), and WeCom 企业微信 (official long connection, native streaming replies, QR robot auto-creation)
- **RunHooks with state** — start/end callbacks share opaque state; OTEL spans nest, slog logs duration
- **Dynamic context** — session-level plan status and mode injected into every prompt turn

## Quick Start

```bash
# Build CLI
go build -o openagent-cli ./cmd/cli/

# Show version
./openagent-cli -v

# ACP mode (stdio — for VSCode/Zed ACP plugins)
./openagent-cli serve --acp

# REST mode (HTTP + SSE)
./openagent-cli serve --port 8080

# One-shot chat with streaming output
./openagent-cli run "Hello, introduce yourself briefly"
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

Connect an ACP client (VSCode/Zed plugin).

#### Web Search backend

The `websearch` tool supports two backends, selected by `OPENAGENT_WEB_SEARCH_ENGINE`:

| Engine | Default | Reachable in mainland China | API key | Env vars |
|--------|---------|----------------------------|---------|----------|
| `tavily` | yes | sometimes (AWS us-east) | optional (keyless works) | `TAVILY_API_KEY` (for higher rate limits) |
| `bocha` | no | yes | required | `BOCHA_API_KEY` |

Default is `tavily` (keyless, no account needed). If Tavily is unreachable from your network, the error message includes a hint to switch. To use Bocha (recommended for mainland-China users):

```bash
export OPENAGENT_WEB_SEARCH_ENGINE=bocha
export BOCHA_API_KEY=<your-key>   # get one at https://open.bochaai.com
```

### Feishu / Lark Integration

Connect your agent to Feishu (Lark) so users can chat with it in IM — group chats, private chats, cards with markdown rendering, and real-time streaming output.

<img src=".github/images/feishu-bot-effect.jpg" alt="Feishu bot in action" width="750" />

**First-time setup (no credentials needed):**

```bash
./openagent-cli serve --channel feishu
```

A QR code will appear in your terminal. Open Feishu on your phone, scan it, and confirm the app creation. The SDK automatically provisions a bot app with the correct permissions (`im:message`, `im:message:send_as_bot`, `im.message.receive_v1` event) and saves the credentials locally.

![First login - scan QR code](.github/images/feishu-first-login.jpg)

**If you already have an app, configure it in `settings.json`:**

```json
{
  "provider": {
    "openai": { "api_key": "sk-...", "models": ["gpt-4o"] }
  },
  "channels": {
    "feishu": {
      "app_id": "cli_xxxxxxxxxxxxxxxx",
      "app_secret": "xxxxxxxxxxxxxxxxxxxxxxxxxx"
    }
  }
}
```

Then run with the flag to enable the channel:

```bash
./openagent-cli serve --channel feishu
```

The `--channel` flag is always required to start the bot — settings.json alone won't auto-start it. If your credentials are in settings.json, the setup step is skipped automatically.

![Subsequent login - start with credentials](.github/images/feishu-subsequent-login.jpg)

**Where credentials are stored:**

| Priority | Source | When to use |
|----------|--------|-------------|
| 1 | `settings.json` → `channels.feishu` | You have the app ID and secret |
| 2 | settings.json `channels.feishu` | Auto-saved after QR registration (settings is the single credential source) |
| 3 | QR code registration | First time, no credentials at all |

**Combine with other modes:**

```bash
# REST API + Feishu bot
./openagent-cli serve --channel feishu

# ACP mode (stdio for VSCode/Zed) + Feishu bot
./openagent-cli serve --acp --channel feishu
```

**Frontend control panel:**

The Feishu connection is a **process-level daemon** — the frontend only triggers and observes; closing or refreshing the page never affects it. Serve exposes two endpoints:

| Endpoint | Purpose |
|----------|---------|
| `GET /api/channels/feishu/status` | Connection state (`connected`, `app_id`, `connected_at`) — call on page load, then poll every few seconds |
| `POST /api/channels/feishu/connect` | Start the connection; with no persisted credentials it starts QR registration and returns `202 {status:"registration", qr_url}` for the frontend to render |
| `POST /api/channels/feishu/disconnect` | Tear down the connection (releases the machine lock); a later `POST /connect` re-establishes it |
| `GET /api/settings/channels/feishu` | The feishu configuration (`app_id`, masked `app_secret`) — the secret never leaves the server unmasked |
| `PUT /api/settings/channels/feishu` | Store the feishu configuration (`{app_id, app_secret}`, empty secret keeps the current one) — written to settings.json, applied on the next connect |
| `DELETE /api/settings/channels/feishu` | Clear the feishu credentials (a running connection keeps working until the next connect) — the "re-register" flow is DELETE + `POST /connect`, which then has no credentials and runs QR registration |

Configuration is separate from the connection: `PUT /api/settings/channels/feishu` only saves (persisted to settings.json `channels.feishu`, applied to the running server's in-memory config); applying the new values is the frontend's explicit two-step — `POST /disconnect` + `POST /connect` (a connect after disconnect cannot short-circuit on a stale "connected"). settings.json is the single credential source: manual config, frontend submissions, and QR-registration artifacts all land there. Frontend flow: load → `GET status` → show state → "Connect" button (QR registration), the config form (`GET/PUT/DELETE /api/settings/channels/feishu`), or "re-register" (DELETE + connect) when credentials failed → poll `status` until `connected`.

**Single instance per profile:** one Feishu app = one active WebSocket. The server holds a machine-level lock (`$profile/channel/feishu/feishu.lock`) for the whole connection lifetime — a second `--channel feishu` instance fails fast instead of silently stealing events. The lock is released automatically by the kernel if the process dies. For production, run under systemd/Docker so the process (and its connection) is supervised.

**Adding MCP tools (optional):**

```json
{
  "mcp_servers": {
    "browser": {
      "command": "npx",
      "args": ["-y", "@anthropic/mcp-server-browser-tools"]
    }
  }
}
```

MCP tools are available to the Feishu bot at startup. Each tool call renders as a card in the chat.

**Logging:**

```json
{
  "log": {
    "file": "/var/log/openagent/openagent.log",
    "max_size": 10,
    "max_backups": 5,
    "level": "debug"
  }
}
```

All fields are optional. Defaults: `~/.openagent/logs/openagent.log`, 10 MB rotation, 5 backups, info level.
Each `max_size` unit is megabytes. Logs go to both stderr *and* the file. Set `level` to `"debug"` to see every API request.

### WeChat (personal) Integration

Connect your agent to your **personal WeChat** via Tencent's official ilinkai channel (`ilinkai.weixin.qq.com`) — no SDK, plain HTTP long-poll. The agent replies once per message (WeChat has no streaming/message-edit API; a "对方正在输入" typing indicator shows while the agent works). Media markers (`[file: /path]` in reply text) are uploaded and sent as file/image messages.

**First-time setup (scan to create the bot):**

```bash
./openagent-cli serve --channel wechat
```

A QR code appears in the terminal — scan it with WeChat and confirm. The bot is created automatically; if the server asks for a **pairing code** (digits shown on your phone), type it in the terminal. Credentials are saved to settings.json.

**Frontend flow:** the channel has a pairing-code step and a "scanned" state — the frontend polls the QR endpoint for the interactive flags:

| Endpoint | Purpose |
|----------|---------|
| `GET /api/channels/wechat/status` | Connection state (`connected`, `account_id`, `last_error`) |
| `POST /api/channels/wechat/connect` | Start the connection; with no credentials it starts QR login and returns `202 {status:"registration", qr_url, qr_img_base64, expires_in}` |
| `POST /api/channels/wechat/disconnect` | Tear down the connection |
| `GET /api/channels/wechat/qr` | The registration QR + `scanned` / `verify_code_required` / `verify_code_retry` flags (the frontend shows the pairing-code input box when required) |
| `POST /api/channels/wechat/verifycode` | Submit the pairing code (`{code}`) — the only channel for the `need_verifycode` step |
| `GET/PUT/DELETE /api/settings/channels/wechat` | Credentials (`token`, `base_url`, `account_id`, `user_id`; masked token on GET) |

A session that expires server-side (`errcode -14`) clears the credentials automatically — the next connect re-runs the QR login.

### WeCom (企业微信) Integration

Connect your agent to a **WeCom smart robot** via the official long-connection API (`wss://openws.work.weixin.qq.com`) — the richest of the three channels: **native streaming replies** (one message that grows in place), group chats with @-mentions, and voice already transcribed to text.

**First-time setup (scan to auto-create the robot):**

```bash
./openagent-cli serve --channel wecom
```

A QR code appears — scan it with the WeCom app; the robot is created automatically and the BotID/Secret are saved to settings.json. Alternatively, create the robot manually in the WeCom admin console (安全与管理 → 管理工具 → 智能机器人 → API 模式 → 长连接) and configure it via the settings endpoint:

```json
{
  "channels": { "wecom": { "bot_id": "aibs...", "secret": "..." } }
}
```

**Frontend control panel** — same shape as Feishu (only the credential fields differ):

| Endpoint | Purpose |
|----------|---------|
| `GET /api/channels/wecom/status` | Connection state (`connected`, `bot_id`, `connected_at`, `last_error`) |
| `POST /api/channels/wecom/connect` | Start the connection; with no credentials it starts QR authorization and returns `202 {status:"registration", qr_url, qr_img_base64, expires_in}` |
| `POST /api/channels/wecom/disconnect` | Tear down the connection |
| `GET /api/channels/wecom/qr` | The authorization QR (re-fetch after a refresh) |
| `GET/PUT/DELETE /api/settings/channels/wecom` | Credentials (`bot_id`, masked `secret`) |

**Streaming replies:** the agent's answer is sent as a stream message — `finish=false` refreshes grow the same message until `finish=true` ends it. The session rate limit is 30 messages/minute.

**Connection semantics (all three channels):** settings credentials never auto-connect — the only auto-connect entry points are `--channel <name>` (fail-fast) and the frontend's `POST /connect`. Scanned/configured credentials are reused across restarts; the machine lock (`$profile/channel/<name>/<name>.lock`) keeps one live connection per profile.

### OpenViking Integration

OpenViking is a context database that provides server-side memory, skill, and resource management. Configuring an endpoint switches all three domains from local storage to the OpenViking server — one address is enough.

```json
{
  "openviking": {
    "endpoint": "http://127.0.0.1:1933",
    "api_key": "ov-xxxxxxxxxxxx"
  }
}
```

- `endpoint` — OpenViking server address. Required to enable.
- `api_key` — Bearer token sent as `Authorization: Bearer <key>`. Optional; empty = no auth (dev mode only).

To keep a specific domain on local storage while using OpenViking for the rest:

```json
{
  "openviking": { "endpoint": "http://127.0.0.1:1933", "api_key": "ov-xxx" },
  "context_providers": { "memory": "builtin" }
}
```

`context_providers` accepts `"builtin"` or `"openviking"` for each of `memory`, `skill`, `resource`. Empty = follow the endpoint default.

## Architecture

```
┌──────────────────────────────────────────────┐
│  Application (rest / acp / cmd/cli)          │
│    assembles agent config + runtime deps     │
└──────────────────┬───────────────────────────┘
                   ▼
┌──────────────────────────────────────────────┐
│  kernel.Runtime  (8-node execution engine)   │
│  ├─ context     (AgentContext assembly +     │
│  │               knowledge recall)           │
│  ├─ execution   (tool jobs, retry, stream)   │
│  ├─ governance  (approval policy chain:      │
│  │               rules→safety→memory→human)  │
│  ├─ session     (store + token-budget        │
│  │               compression)                │
│  ├─ provider/   (memory | skill | resource)  │
│  └─ eventbus    (audit events)               │
└──────────────────────────────────────────────┘
```

The `agent.Agent` is pure configuration (model, prompts, guards, sub-agents);
everything executable lives in the runtime and its dependencies — tools,
stores, policies, hooks, and observers are interfaces injected at assembly.

## Plugins

Plugins are **WASM modules** (.wasm files). Any language that compiles to WASM works — Rust, Go, TypeScript, Zig, etc. The host runtime (wazero) loads and executes them in a sandboxed environment.

A plugin declares its type via metadata. Currently we provide a Rust SDK (`plugin/pdk/rust/`) that wraps the FFI contract, but the ABI is simple enough to implement from any language.

| Plugin type | What it does |
|-------------|--------------|
| `agent:tools` | Adds custom tools to the agent — the agent can call them like shell/read/write |
| `agent:observers` | Hooks into the agent's run pipeline (enter/leave for each stage) |
| `cli:settings` | Transforms `settings.json` at startup (merge env vars, add providers, etc.) |
| `cli:commands` | Registers extra cobra subcommands into the CLI |
| `cli:observers` | Monitors CLI command lifecycle (startup/shutdown/command enter/exit) |

### How it works

Each plugin type exposes one or two exported functions:

| Type | Exports | Signature |
|------|---------|-----------|
| `agent:tools` | `openagent_agent_tools()` → JSON | Returns `[{name, description, parameters}]` |
| | `openagent_execute(name, args)` → string | Called when the agent invokes the tool |
| `agent:observers` | `openagent_on_stage(event_json)` | Called on each stage enter/leave |
| `cli:settings` | `openagent_cli_init(settings_json)` → JSON | Returns merged settings |
| `cli:commands` | `openagent_cli_commands()` → JSON | Returns `[{use, short, long}]` |
| | `openagent_cli_run(name, args_json)` → string | Called when the command runs |
| `cli:observers` | `openagent_cli_on_startup()` / `...on_shutdown()` / etc. | Lifecycle callbacks |

The host runtime (wazero + `plugin/wasmhost/`) provides a set of importable host functions (`log_info`, `keyring_get`, `http_request`, `utc_now`, etc.) that plugins can call.

### Enabling plugins

Place `.wasm` files in a directory and configure it in `settings.json`:

```json
{
  "plugins": ["~/.openagent/plugins"]
}
```

At startup the CLI scans all configured directories for `.wasm` files, reads their metadata, instantiates them, and wires them into the agent or CLI command tree.

### Compiling a plugin (Rust example)

```bash
# Prerequisites: Rust + wasm32-unknown-unknown target
rustup target add wasm32-unknown-unknown

# Build
cd examples/plugin/tool
cargo build --release --target wasm32-unknown-unknown

# Copy to plugins directory
cp target/wasm32-unknown-unknown/release/example_agent_tool.wasm ~/.openagent/plugins/echo.wasm
```

Or use the Makefile:

```bash
make -C examples/plugin
```

### Writing a tool plugin (agent:tools)

```rust
use openagent_sdk::tool::{register_tools, ToolDef};

#[no_mangle]
pub extern "C" fn openagent_agent_tools() -> *const u8 {
    register_tools(&[ToolDef {
        name: "echo",
        description: "Echo back the input message.",
        parameters: r#"{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}"#,
    }])
}

#[no_mangle]
pub extern "C" fn openagent_execute(name: &str, args: &str) -> String {
    format!("echo: {}", extract_field(args, "message"))
}
```

### Writing an observer plugin (agent:observers)

```rust
use openagent_sdk::observer::StageEvent;

#[no_mangle]
pub extern "C" fn openagent_on_stage(event_json: &str) {
    let e: StageEvent = serde_json::from_str(event_json).unwrap();
    if e.phase == "enter" {
        // log_info is an imported host function
    }
}
```

### Host API (importable from any language)

| Function | Purpose |
|----------|---------|
| `log_info(msg)` / `log_warn(msg)` / `log_error(msg)` | Logging through the host |
| `utc_now() -> i64` | Current time in nanoseconds |
| `keyring_get(service, key) -> string` | Read from system keyring |
| `keyring_set(service, key, value)` | Write to system keyring |
| `keyring_delete(service, key)` | Delete from system keyring |
| `http_request(method, url, headers_json, body) -> {status, body}` | Outbound HTTP |

Full example: `examples/plugin/`. Rust SDK: `plugin/pdk/rust/`.

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
| `examples/artifact/` | Result policy — large tool results spill to disk |
| `examples/browser-agent/` | Browser agent via Playwright MCP |
| `examples/mcp-client/` | MCP client demo (IaC pipeline) |
| `cmd/cli/` | Full-featured CLI with WASM plugin runtime |

## Packages

| Package | Purpose |
|---------|---------|
| `openagent` | Core types — Agent (pure config), Team, ToolResult, token helpers |
| `kernel/` | Runtime — the 8-node execution engine (memory → prompt → guard → model → guard → policy → tools → store) |
| `execution/` | Tool execution — parallel jobs, retry, streaming, result policy |
| `governance/` | Approval policy engine — rules → safety → memory → human, persistent decisions |
| `context/` | AgentContext assembly — knowledge recall, skill match, LLM extractor (self-evolution) |
| `acp/sdk/` | ACP v1 protocol SDK — types, JSON-RPC 2.0 mux, client |
| `acp/` | AgentServer — wraps an Agent as an ACP handler |
| `rest/` | REST + SSE handlers (single, team, orchestrate) |
| `orchestrate/` | Multi-agent DAG decomposition + streaming execution |
| `plan/` | `plan_create`/`plan_update` tools (ACP plan mode) |
| `slash/` | Slash command registry and dispatch |
| `summarizer/` | LLM-based incremental conversation compression |
| `session/` | Session store interface + token-budget compression |
| `session/sqlite/` | SQLite session store |
| `session/file/` | File-backed session store |
| `provider/memory/` | Durable knowledge backends (sqlite, file) |
| `provider/skill/` | On-demand skill matching/loading |
| `provider/resource/` | External reference resources |
| `provider/openviking/` | OpenViking context database client (memory/skill/resource over HTTP) |
| `model/openai/` | OpenAI ChatCompletion + streaming |
| `tokenizer/` | tiktoken model-aware token counting (sampled estimate for huge texts) |
| `sandbox/native/` | OS-level process confinement (bwrap/Seatbelt) |
| `eventbus/` | Session-scoped pub/sub for SSE |
| `plugin/wasmhost/` | Shared WASM host module (keyring, HTTP, logging, utc_now) |
| `plugin/agent/wasm/` | Agent-scoped WASM plugin host |
| `plugin/cli/` | CLI plugin manager and types |
| `plugin/cli/wasm/` | CLI-scoped WASM runtime, loader, observer hub |
| `plugin/pdk/rust/` | Rust SDK crate for building WASM plugins |
| `skill/fs/` | Filesystem skill loader |
| `mcp/` | Model Context Protocol client |
| `guard/llm/` | LLM-based input/output guard |
| `hooks/otel/` | OpenTelemetry hooks |
| `hooks/slog/` | Structured logging hooks |
| `tool/` | Built-in tools (shell, read, write, ls, grep, edit, websearch, webfetch, ACP fs, ACP terminal) |
| `channel/` | IM platform adapters — Feishu WebSocket, card rendering |
| `cmd/cli/` | CLI runtime, WASM host, Rust SDK examples |
