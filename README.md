# CodingMan

CodingMan 是一个用 Go 实现的 coding agent。它以 TUI 作为默认入口，围绕 Agent Core 提供模型调用、工具执行、权限控制、上下文管理、会话恢复、SKILL、MCP、Hooks 和多 Agent 协作能力。

启动 CodingMan 的目录会作为默认工作目录。程序启动时优先从项目根目录的 `.env` 加载配置；如果没有 `.env`，则读取当前环境变量。配置了 `BASE_URL` 时，表示使用第三方 OpenAI-compatible 或 Anthropic-compatible endpoint。

![CodingMan Architecture](assets/architecture.png)

## 快速开始

准备配置：

```bash
cp .env.example .env
```

编辑 `.env`，至少填写：

```env
PROVIDER=OpenAI
MODEL_NAME=gpt-5-mini
API_KEY=
BASE_URL=
```

启动 TUI：

```bash
go run .
```

常用验证：

```bash
go test ./...
go build ./...
go vet ./...
```

## 核心能力

- **TUI 入口**：`main.go` 是启动入口，支持普通对话、plan mode、ESC 中断、图片输入和 slash commands。
- **Provider**：支持 OpenAI-compatible 和 Anthropic-compatible 模型接口；第三方 API 通过 `BASE_URL` 接入。
- **工具系统**：内置 `read`、`write`、`edit`、`grep`、`glob`、`bash`，并支持并行安全分批执行。
- **权限系统**：支持 `ask`、`allow-deny`、`full-auto` 三种模式；只读工具和安全 bash 命令可默认并行。
- **上下文系统**：加载系统提示词、项目记忆、SKILL、会话记忆、跨会话记忆，并支持自动压缩。
- **SKILL 系统**：支持用户级和项目级 SKILL，项目级同名覆盖用户级；可通过 `/skill use <name>` 激活运行时工具白名单。
- **记忆自我进化**：复杂任务后自动审查对话，将可复用经验沉淀为用户级 SKILL。
- **MCP Client**：支持 stdio、SSE、HTTP、WebSocket 传输，提供 MCP tools 和 resources 访问。
- **Hooks**：支持 `http`、`shell`、`function`、`log` 类型 Hook，覆盖工具调用和 Agent 生命周期事件。
- **子 Agent / A2A**：主 Agent ID 默认为 `main`，可启动直接子 Agent，支持 fork/worker 模式、异步任务和中止。
- **日志系统**：每轮对话生成 trace id，日志格式为 `[时间][trace id] 内容`。

## 配置

主要配置来自 `.env`：

```env
PROVIDER=OpenAI
MODEL_NAME=gpt-5-mini
API_KEY=
BASE_URL=

CWD=.
BASE_SYSTEM=
INCLUDE_DATE=true
LOAD_AGENTS_MD=true
LOAD_SKILLS=true
AUTO_COMPACT=true
COMPACT_THRESHOLD=60000
KEEP_RECENT_ROUNDS=6
MAX_AGENTS_MD_BYTES=65536
PROGRESSIVE_MEMORY_MAX_CHARS=12000
PROGRESSIVE_SKILL_MAX_CHARS=12000

MAX_LLM_TURNS=20
MAX_TOOL_CALLS=50
MAX_PARALLEL_TOOL_CALLS=4
MAX_CONSECUTIVE_TOOL_ERRORS=3
MAX_CONSECUTIVE_API_ERRORS=3
MAX_SUB_AGENT_DEPTH=1
MAX_CONCURRENT_SUB_AGENTS=4

SESSION_MEMORY_TOOL_THRESHOLD=10
SKILL_EVOLUTION_TOOL_THRESHOLD=10
SESSION_MEMORY_MAX_ENTRIES=8
SESSION_MEMORY_MAX_CHARS=8000
CROSS_SESSION_MEMORY_MAX_CHARS=12000
```

提示缓存、工具预算、重试、日志等配置见 [.env.example](.env.example)。

## Slash Commands

TUI 内输入 `/help` 可以查看所有命令。

常用命令：

```text
/help                         show help
/clear                        clear conversation history
/cache                        show prompt cache status
/cache on|off                 enable or disable prompt cache
/plan                         show plan mode status
/plan on|off                  toggle plan mode
/skill                        show loaded and active skills
/skill use <name>             activate a skill and its allow_tools
/skill clear                  clear active skill
/sessions                     list saved sessions
/resume [session_id|latest]   restore a saved session
/system <path>                load system prompt from file
/permission                   show permission mode and policy
/permission ask               ask before tool calls
/permission allow-deny        use allow/deny policy
/permission full-auto         allow all tool calls
/allow <tool>                 allow a tool in this session
/allow *                      allow all tools in this session
/deny <tool>                  deny a tool in this session
/exit                         quit
```

## 上下文与记忆

项目记忆按用户级、项目级、本地级渐进加载：

- 用户级：`~/.codingman/AGENTS.md`
- 项目级：`<project>/.codingman/AGENTS.md`
- 项目规则：`<project>/.codingman/rules/**/*.md`
- 本地级：从项目根目录到当前工作目录逐级加载 `.codingman/AGENTS.md`

会话以启动目录为粒度持久化：

```text
~/.codingman/projects/<path-hash>/
```

每个会话独立 JSONL 文件，恢复范围包括 messages、file history、attribution、todos 和 session memory。

跨会话记忆存储在：

```text
~/.codingman/projects/<path-hash>/memory/
```

包含：

```text
MEMORY.md
user_prefs.md
project_stack.md
feedback_testing.md
references.md
```

## SKILL

用户级 SKILL：

```text
~/.codingman/skills/<skill-name>/SKILL.md
```

项目级 SKILL：

```text
<project-root>/.codingman/skills/<skill-name>/SKILL.md
```

SKILL frontmatter 示例：

```md
---
name: go-testing
description: Go test debugging workflow
allow_tools: [read, grep, bash, edit]
context: fork
---

# Go Testing

Describe the reusable workflow here.
```

字段说明：

- `name`：SKILL 名称。
- `description`：一句话描述。
- `allow_tools`：激活该 SKILL 后 Agent 可用工具白名单；不写表示所有工具。
- `context`：`fork` 或 `inline`，默认 `fork`。

当工具调用次数达到 `SKILL_EVOLUTION_TOOL_THRESHOLD` 后，Agent 会在后台审查对话，将值得长期复用的经验写成用户级 SKILL。

## MCP 与 Hooks

MCP 配置从以下文件合并加载：

```text
~/.codingman/settings.json
<project-root>/settings.json
```

支持字段 `mcp_servers` 或 `mcpServers`。MCP 工具注册名格式为：

```text
mcp_<server>_<tool>
```

Hooks 也从 `settings.json` 加载，支持事件：

```text
PreToolUse
PostToolUse
Notification
Stop
SubagentStop
```

Hook 类型：

```text
http
shell
function
log
```

## 开发

常用命令：

```bash
go test ./...
go build ./...
go vet ./...
```

模块结构：

```text
agent/   Agent core, providers, context, memory, MCP, hooks, sub-agents
tool/    Built-in tools and tool registry
main.go  TUI entrypoint
```
