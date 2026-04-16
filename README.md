# ollie

A Go library for building agentic systems. Provides a sandboxed `execute_code` tool, a common LLM backend interface, MCP server support, and a skill system for domain-specific capabilities.

Works well with [anvillm](https://github.com/lneely/anvillm), which provides a skill system, tool scripts, and multi-agent infrastructure.

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui), a terminal UI built on top of this library.

## Primitives

**`agent.Core`** — the central interface for a running agent session. Exposes `Submit` (send a prompt, stream events back), `Interrupt`, `Inject`, `Queue`/`PopQueue` (buffered prompt delivery), `State`, `Reply`, `SystemPrompt`, `Usage`, `CtxSz`, `ListModels`, `ListServers`, `CWD`/`SetCWD`, `SetSessionID`, `IsRunning`, and `Close`.

**`agent.Session`** — the conversation turn accumulator. Tracks message history, token usage, context compaction, and session persistence. Supports `compact` (summarize-and-truncate) and `PreCompactionSnapshot`.

**`agent.AgentEnv`** — wires together a backend, tool dispatcher, config, and hooks into the environment passed to `NewAgentCore`. Built via `BuildAgentEnv`.

**`agent.Hooks`** — lifecycle callbacks (`agentSpawn`, `userPromptSubmit`, `stop`) executed as shell commands with a JSON payload. Run via `Hooks.Run`.

**`backend.Backend`** — the LLM interface: `ChatStream`, `Models`, `ContextLength`, `Name`, `Model`/`SetModel`, `DefaultModel`. Implementations: Ollama, OpenAI-compatible, Anthropic, Copilot, Kiro.

**`tools.Server`** — interface for a tool provider: `ListTools`, `CallTool`, `Close`. Implementations: `execute.Server` (built-in execution tools), MCP client wrapper, any custom server.

**`tools.Dispatcher`** — routes tool calls to the correct server by name. Built via `NewDispatcher` or `NewDispatcherFunc` (from a map of `Decl` factories). Supports `AddServer`, `GetServer`, `ListTools`, `Dispatch`.

## Packages

```
pkg/agent/           — Core interface, agent loop, session management
pkg/backend/         — Backend interface + implementations (Ollama, OpenAI, Anthropic, Copilot, Kiro)
pkg/config/          — Config struct and loader
pkg/mcp/             — MCP client
pkg/tools/           — Server and Dispatcher interfaces; tool definitions
pkg/tools/execute/   — execute.Server: execute_code, execute_tool, execute_pipe
```

## Install

```
mk
```

No build step — ollie-core is a library.

## Configuration

### Config file: `~/.config/ollie/config.json`

```json
{
  "mcpServers": {
    "my-server": {
      "command": "my-mcp-server",
      "args": [],
      "env": {
        "API_TOKEN": "${API_TOKEN}"
      }
    }
  },
  "hooks": {
    "agentSpawn": "notify-send ollie started",
    "userPromptSubmit": "",
    "stop": ""
  }
}
```

MCP server `env` values support `${VAR}` expansion from the parent environment.

### Sandbox config: `~/.config/ollie/sandbox.yaml`

Controls landrun sandboxing for `execute_code`. Created automatically with defaults on first run. See the file header for documentation.

## Tools

Three built-in tools via `execute.Server`:

**`execute_code`** — run one or more code snippets in a sandbox. Accepts a `steps` array; multiple steps run concurrently and results are returned in submission order. Each step is either inline `code` (with optional `language`) or a named `tool` script. Supported languages: bash (default), python3, perl, lua, awk, sed, jq, ed, expect, bc. Accepts `timeout` (per-step, default 30s) and `sandbox` (default: `default`).

**`execute_tool`** — run a named script from `OLLIE_TOOLS_PATH`. Language detected from shebang. Accepts `tool`, `args`, `timeout`, `sandbox`.

**`execute_pipe`** — run a sequential pipeline, chaining each stage's stdout to the next stage's stdin. Each stage is `{code}`, `{tool, args}`, or `{parallel: [...]}` for concurrent fan-out within a stage. Accepts `timeout` (per-stage) and `sandbox`.

## Session lifecycle

`NewAgentCore` creates `/tmp/ollie/{sessionID}` when a session starts. `Core.Close()` removes it. Callers must call `Close()` when tearing down a session — olliesrv does this in `killSession` and on server shutdown.

File operations go through `execute_code` using standard shell tools (`cat`, `grep`, `sed`, `ed`, `ssam` if plan9port is available, etc.).

MCP server tools are discovered at startup and available alongside the built-ins.

Each tool server exports a `Decl` function that returns a `func() tools.Server` factory. `execute.Decl(cwd)` accepts a working directory used as `cmd.Dir` for sandboxed commands and for `{CWD}` expansion in the sandbox config; pass `""` to fall back to `os.Getwd()`. Frontends register servers by passing Decl results to `tools.NewDispatcherFunc`. Adding a new tool means implementing `tools.Server`, exporting a `Decl` function, and registering it — no frontend changes required.

## Skills

Skills are domain-specific knowledge files served from the ollie 9P mount (`sk/` directory).

```sh
# Discover
ls ${OLLIE_9MOUNT:-$HOME/mnt/ollie}/sk/
grep -li <keyword> ${OLLIE_9MOUNT:-$HOME/mnt/ollie}/sk/*.md

# Load
cat ${OLLIE_9MOUNT:-$HOME/mnt/ollie}/sk/<name>.md
```

Skills are sourced from `OLLIE_SKILLS_PATH` (default: `~/.config/ollie/skills/`). The `sk/` directory in the mount exposes them as flat `<name>.md` files.

## License

GPLv3