# openagent-go

> [English](README.md) | [Architecture](DESIGN.md) | [架构 (中文)](DESIGN.zh.md)

一个完全可插拔的多智能体 AI Agent 框架，Go 语言实现。

## 特性

- **全插件架构** — 每个组件都是接口：Model、Memory、Tools、Guards、Approver、Hooks、Observer
- **ACP v1 协议** — 完整的 Agent Client Protocol 实现，基于 stdio（JSON-RPC 2.0）。可用任何 ACP 客户端（VSCode 插件、Zed 等）
- **Plan 模式** — `plan_create`/`plan_update` 工具让 agent 将复杂任务分解为结构化步骤，实时追踪进度
- **多智能体团队** — agent 之间通过 `transfer_to_*` 工具交接任务；每个 agent 有独立的记忆、工具和守卫
- **多智能体编排** — LLM 驱动的 DAG 分解、并行执行和自动重规划（`orchestrate/`）
- **SSE 流式输出** — 实时逐 token 渲染，支持 reasoning 展示、工具调用卡片
- **三层记忆系统** — Working（token 驱动）、Compressed（LLM 增量摘要，`summarizer/`）、Archive（FTS5/向量检索，永不删除）
- **沙箱环境** — 原生 OS 级别隔离（Linux bwrap、macOS Seatbelt），安全执行 shell、文件、网络操作
- **WASM 插件** — Agent 级：`agent:tools` 和 `agent:observers` 接入工具/观测器管线。CLI 级：`cli:settings`、`cli:commands`、`cli:observers`，用于设置注入、命令扩展和生命周期监控
- **静态上下文配置** — `AGENTS.md`（工作规则）和 `SOUL.md`（性格与底线），支持用户级和项目级覆盖
- **Slash 命令** — 内置 `/help`、`/mode`、`/model`、`/context`、`/cwd`、`/clear`、`/rename`、`/sessions`，通过 `slash/` 注册表扩展
- **完整 CLI** — `openagent-cli`，cobra 命令、配置驱动模型、keyring 密钥管理、WASM 插件运行时
- **RunHooks 状态传递** — Start/End 回调共享不透明状态，OTEL 正确嵌套 span，slog 精确计时
- **动态上下文** — 会话级 plan 状态和 mode 指令每轮自动注入 prompt

## 快速开始

```bash
# 编译 CLI
go build -o openagent-cli ./cmd/cli/

# ACP 模式（stdio — 配合 VSCode/Zed ACP 插件使用）
./openagent-cli serve --acp

# REST 模式（HTTP + SSE）
./openagent-cli serve --port 8080

# 前端
cd examples/frontend/vue-app && npm install && npm run dev
```

### 配置

创建 `~/.openagent/settings.json`：

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

将 `AGENTS.md` 和 `SOUL.md` 放在 `~/.openagent/profile/` 或 `$(pwd)/.openagent/profile/` 来自定义 agent 的行为。

打开 `http://localhost:5173` 或连接 ACP 客户端 — 服务端同时支持两种协议。

## 架构

```
┌──────────────────────────────────────────────┐
│  Agent                                       │
│  ├── Model        (LLM 提供商)               │
│  ├── Memory       (对话持久化)               │
│  ├── Tools        (shell, 读写文件, ...)     │
│  ├── InGuard      (输入校验)                 │
│  ├── OutGuard     (输出校验)                 │
│  ├── Approver     (工具调用的用户确认)       │
│  ├── Hooks        (生命周期回调)             │
│  └── Observer     (pipeline 监控)            │
└──────────────────────────────────────────────┘
```

## 示例

| 示例 | 说明 |
|------|------|
| `examples/basic/` | 最小化 agent + model |
| `examples/stream/` | 流式文本输出 |
| `examples/memory/` | 记忆 + 摘要压缩 |
| `examples/team/` | 多 agent 交接 |
| `examples/guard/` | 输入/输出守卫 |
| `examples/hooks/` | 生命周期钩子 |
| `examples/observer/` | Pipeline 观测器 |
| `examples/delegate/` | Agent 作为工具委托 |
| `examples/sandbox/` | 原生沙箱工具 |
| `examples/plugin/` | WASM 工具 + 观测器插件 |
| `examples/skill/` | 按需加载技能 |
| `examples/acp/` | ACP agent 协议（server + client） |
| `examples/iac/` | 多 agent IaC 流水线 |
| `examples/backend/` | 完整 REST + SSE API 服务 |
| `examples/frontend/vue-app/` | Vue 3 SPA 参考前端 |
| `cmd/cli/` | 完整 CLI，含 WASM 插件运行时 |

## 包

| 包 | 用途 |
|----|------|
| `openagent` | 核心类型、Agent、Team、Runner、Memory、Sandbox |
| `acp/sdk/` | ACP v1 协议 SDK — 类型定义、JSON-RPC 2.0 mux、客户端 |
| `acp/` | AgentServer — 将 Agent 包装为 ACP handler |
| `rest/` | REST + SSE 处理器（单 agent / team / orchestrate） |
| `orchestrate/` | 多 agent DAG 分解 + 流式执行 |
| `plan/` | `plan_create`/`plan_update` 工具（ACP plan 模式） |
| `slash/` | Slash 命令注册表和分发 |
| `summarizer/` | 基于 LLM 的增量对话压缩 |
| `memory/sqlite/` | SQLite + FTS5 + 向量记忆后端 |
| `memory/file/` | JSONL 文件记忆后端 |
| `model/openai/` | OpenAI ChatCompletion + 流式 |
| `tokenizer/` | tiktoken 模型感知 token 计数 |
| `sandbox/native/` | OS 级进程隔离（bwrap/Seatbelt） |
| `session/` | 会话元数据类型和存储接口 |
| `session/sqlite/` | SQLite 会话存储 |
| `session/file/` | 文件会话存储 |
| `eventbus/` | 会话级发布订阅（SSE） |
| `plugin/wasmhost/` | 共享 WASM host 模块（keyring、HTTP、日志、utc_now） |
| `plugin/agent/wasm/` | Agent 级 WASM 插件宿主 |
| `plugin/cli/` | CLI 插件管理和类型 |
| `plugin/cli/wasm/` | CLI 级 WASM 运行时、加载器、observer hub |
| `plugin/sdk/rust/` | Rust SDK crate，用于构建 WASM 插件 |
| `skill/fs/` | 文件系统技能加载器 |
| `mcp/` | Model Context Protocol 客户端 |
| `guard/llm/` | 基于 LLM 的输入/输出守卫 |
| `hooks/otel/` | OpenTelemetry 钩子 |
| `hooks/slog/` | 结构化日志钩子 |
| `tool/` | 内置工具 (shell, read, write, ls, grep, ACP fs, ACP terminal) |
| `cmd/cli/` | CLI 运行时、WASM 宿主、Rust SDK 示例 |
