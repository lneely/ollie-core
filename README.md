# ollie

A Go library for building agentic systems. Provides a sandboxed `execute_code` tool, a common LLM backend interface, and a skill system for domain-specific capabilities.

Works well with [anvillm](https://github.com/lneely/anvillm), which provides a skill system, tool scripts, and multi-agent infrastructure.

## Primitives

**`agent.Core`** — the central interface for a running agent session. Exposes `Submit` (send a prompt, stream events back), `Interrupt`, `Inject`, `Queue`/`PopQueue` (buffered prompt delivery), `State`, `Reply`, `SystemPrompt`, `Usage`, `CtxSz`, `ListModels`, `CWD`/`SetCWD`, `SetSessionID`, `IsRunning`, and `Close`.

**`agent.Session`** — the conversation turn accumulator. Tracks message history, token usage, context compaction, and session persistence. Supports `compact` (summarize-and-truncate) and `PreCompactionSnapshot`.

**`agent.AgentEnv`** — wires together a backend, tool dispatcher, config, and hooks into the environment passed to `NewAgentCore`. Built via `BuildAgentEnv`.

**`agent.Hooks`** — lifecycle callbacks (`agentSpawn`, `preTurn`, `postTurn`, `preCompact`, `postCompact`, `turnError`) executed as shell commands with a JSON payload. Run via `Hooks.Run`.

**`backend.Backend`** — the LLM interface: `ChatStream`, `Models`, `ContextLength`, `Name`, `Model`/`SetModel`, `DefaultModel`. Implementations: Ollama, OpenAI-compatible, Anthropic, Copilot, Kiro.

**`tools.Server`** — interface for a tool provider: `ListTools`, `CallTool`, `Close`. The only built-in implementation is `execute.Server`. Custom servers implement this interface directly.

**`tools.Dispatcher`** — routes tool calls to the correct server by name. Built via `NewDispatcher` or `NewDispatcherFunc` (from a map of `Decl` factories). Supports `AddServer`, `GetServer`, `ListTools`, `Dispatch`.

## Packages

```
pkg/agent/           — Core interface, agent loop, session management
pkg/backend/         — Backend interface + implementations (Ollama, OpenAI, Anthropic, Copilot, Kiro)
pkg/config/          — Config struct and loader
pkg/env/             — Environment variable loading (env file + shell)
pkg/log/             — Structured logging
pkg/paths/           — Config and data directory resolution
pkg/skills/          — Skill file discovery and loading
pkg/store/           — Store interfaces and implementations (FlatDir, Session, Batch, Skill)
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
  "hooks": {
    "agentSpawn": [
      "bd prime 2>/dev/null || true"
    ],
    "preTurn": [
      "$OLLIE/x/prime tools-file",
      "$OLLIE/x/prime tools-reasoning",
      "$OLLIE/x/prime tools-memory",
      "$OLLIE/x/prime tools-subagent"
    ],
    "postTurn": ["true"]
  }
}
```

Hook values accept a string or an array of strings. Commands run in order; each command's stdout is appended to the system prompt context. Exit code 2 blocks the triggering action; any other non-zero exit is a non-blocking warning.

### System prompt

The base system prompt is loaded from `SYSTEM_PROMPT.md` (in `~/.config/ollie/prompts/`) by Go at session creation via `BuildAgentEnv`. Tool-specific prompts are injected via `preTurn` hooks using the `prime` script (`$OLLIE/x/prime <name>`), which reads a file from `p/` and writes it to stdout with environment variable substitution. The fully assembled result is readable at `s/<id>/systemprompt`.

### Sandbox config: `~/.config/ollie/sandbox/<name>.yaml`

Controls landrun sandboxing for `execute_code`. Created automatically with defaults on first run. See the file header for documentation.

## Tools

One built-in tool via `execute.Server`:

**`execute_code`** — run a pipeline of one or more stages in a sandbox. Stages run sequentially, each stage's stdout fed to the next stage's stdin. A single stage is the degenerate case and returns raw output. Each stage is `{code, language}` for inline code, `{tool, args}` for a named script (language from shebang), or `{parallel: [...]}` for concurrent fan-out. Supported inline languages: bash (default), python3, perl, lua, awk, sed, jq, ed, expect, bc. Accepts `timeout` (per stage, default 30s) and `sandbox` (default: `default`).

## Session lifecycle

`NewAgentCore` creates `/tmp/ollie/{sessionID}` when a session starts. `Core.Close()` removes it. Callers must call `Close()` when tearing down a session — olliesrv does this in `killSession` and on server shutdown.

Additional capabilities (file I/O, memory, reasoning, task management, sub-agents, browser automation) are implemented as tool scripts in `OLLIE_TOOLS_PATH`, invoked via `execute_code` `{tool}` steps. Default tools: `file_read`, `file_write`, `file_edit`, `file_glob`, `file_grep`, `memory_remember`, `memory_recall`, `reasoning_think`, `task_add`, `task_check`, `task_clear`, `denote_view`, `browser_screencap`, `subagent_spawn`.

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