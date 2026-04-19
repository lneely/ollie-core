# ollie

A Go library for building agentic systems. Provides a sandboxed `execute_code` tool, a common LLM backend interface, MCP server support, and a skill system for domain-specific capabilities.

Works well with [anvillm](https://github.com/lneely/anvillm), which provides a skill system, tool scripts, and multi-agent infrastructure.

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui), a terminal UI built on top of this library.

## Primitives

**`agent.Core`** — the central interface for a running agent session. Exposes `Submit` (send a prompt, stream events back), `Interrupt`, `Inject`, `Queue`/`PopQueue` (buffered prompt delivery), `State`, `Reply`, `SystemPrompt`, `Usage`, `CtxSz`, `ListModels`, `ListServers`, `CWD`/`SetCWD`, `SetSessionID`, `IsRunning`, and `Close`.

**`agent.Session`** — the conversation turn accumulator. Tracks message history, token usage, context compaction, and session persistence. Supports `compact` (summarize-and-truncate) and `PreCompactionSnapshot`.

**`agent.AgentEnv`** — wires together a backend, tool dispatcher, config, and hooks into the environment passed to `NewAgentCore`. Built via `BuildAgentEnv`.

**`agent.Hooks`** — lifecycle callbacks (`agentSpawn`, `preTurn`, `postTurn`, `preCompact`, `postCompact`) executed as shell commands with a JSON payload. Run via `Hooks.Run`.

**`backend.Backend`** — the LLM interface: `ChatStream`, `Models`, `ContextLength`, `Name`, `Model`/`SetModel`, `DefaultModel`. Implementations: Ollama, OpenAI-compatible, Anthropic, Copilot, Kiro.

**`tools.Server`** — interface for a tool provider: `ListTools`, `CallTool`, `Close`. The only built-in implementation is `execute.Server`. MCP servers are wrapped via `tools.NewServer(client)`. Custom servers implement this interface directly.

**`tools.Dispatcher`** — routes tool calls to the correct server by name. Built via `NewDispatcher` or `NewDispatcherFunc` (from a map of `Decl` factories). Supports `AddServer`, `GetServer`, `ListTools`, `Dispatch`.

## Packages

```
pkg/agent/           — Core interface, agent loop, session management
pkg/backend/         — Backend interface + implementations (Ollama, OpenAI, Anthropic, Copilot, Kiro)
pkg/config/          — Config struct and loader
pkg/mcp/             — MCP client
pkg/tools/           — Server and Dispatcher interfaces; tool definitions
pkg/tools/execute/   — execute.Server: execute_code
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
    "agentSpawn": [
      "$OLLIE/x/prime sys-base",
      "$OLLIE/x/prime sys-ollie",
      "notify-send ollie started"
    ],
    "preTurn": [],
    "postTurn": []
  }
}
```

MCP server `env` values support `${VAR}` expansion from the parent environment.

Hook values accept a string or an array of strings. Commands run in order; each command's stdout is appended to the system prompt context. Exit code 2 blocks the triggering action; any other non-zero exit is a non-blocking warning.

### System prompt

There is no built-in system prompt. The system prompt is assembled entirely from `agentSpawn` hook output. The `prime` script (`$OLLIE/x/prime <name>`) reads a file from `p/` and writes it to stdout with environment variable substitution — use it in `agentSpawn` to compose the system prompt from prompt template files. The fully assembled result is readable at `s/<id>/systemprompt`.

### Sandbox config: `~/.config/ollie/sandbox.yaml`

Controls landrun sandboxing for `execute_code`. Created automatically with defaults on first run. See the file header for documentation.

## Tools

One built-in tool via `execute.Server`:

**`execute_code`** — run a pipeline of one or more stages in a sandbox. Stages run sequentially, each stage's stdout fed to the next stage's stdin. A single stage is the degenerate case and returns raw output. Each stage is `{code, language}` for inline code, `{tool, args}` for a named script (language from shebang), or `{parallel: [...]}` for concurrent fan-out. Supported inline languages: bash (default), python3, perl, lua, awk, sed, jq, ed, expect, bc. Accepts `timeout` (per stage, default 30s) and `sandbox` (default: `default`).

## Session lifecycle

`NewAgentCore` creates `/tmp/ollie/{sessionID}` when a session starts. `Core.Close()` removes it. Callers must call `Close()` when tearing down a session — olliesrv does this in `killSession` and on server shutdown.

Additional capabilities (file I/O, memory, reasoning, task planning, sub-agents) are implemented as tool scripts in `OLLIE_TOOLS_PATH`, invoked via `execute_code` `{tool}` steps.

MCP server tools are discovered at startup and available alongside the built-ins.

Each tool server exports a `Decl` function that returns a `func() tools.Server` factory. `execute.Decl(cwd)` accepts a working directory used as `cmd.Dir` for sandboxed commands and for `{CWD}` expansion in the sandbox config; pass `""` to fall back to `os.Getwd()`. Frontends register servers by passing Decl results to `tools.NewDispatcherFunc`. Adding a new tool means implementing `tools.Server`, exporting a `Decl` function, and registering it — no frontend changes required.

## Skills

Skills are domain-specific knowledge files served from the ollie 9P mount (`sk/` directory).

```sh
# Discover
ls ${OLLIE:-$HOME/mnt/ollie}/sk/
grep -li <keyword> ${OLLIE:-$HOME/mnt/ollie}/sk/*.md

# Load
cat ${OLLIE:-$HOME/mnt/ollie}/sk/<name>.md
```

Skills are sourced from `OLLIE_SKILLS_PATH` (default: `~/.config/ollie/skills/`). The `sk/` directory in the mount exposes them as flat `<name>.md` files.

## License

GPLv3