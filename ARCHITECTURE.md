# ollie Architecture

## Philosophy

ollie is a small, extensible core library for building agentic systems. It has no binary of its own — frontends and consumers live in separate repos and import ollie as a dependency. The internal surface area is kept minimal; everything a consumer needs is exported under `pkg/`.

## Package Layout

```
pkg/
  agent/         — Core interface, agent loop, session and context management
  backend/       — Backend interface + implementations
  config/        — Agent config struct and loader
  mcp/           — MCP client (concrete)
  tools/         — Executor interface + MCPExecutor
  tools/execute/ — Built-in sandbox tools (execute_code, execute_tool, execute_pipe)

internal/
  sandbox/       — landrun sandbox config and command wrapper
```

### Why internal/sandbox?

The sandbox package is an implementation detail of `pkg/tools/execute`. It has no stable public API and no reason to be imported by consumers directly. Keeping it internal prevents accidental coupling.

### Why pkg/mcp is concrete (not interfaced)

`pkg/mcp` exposes a concrete `Client` rather than an interface. MCP is a well-defined protocol; the client is a thin transport layer and there is currently no need for consumers to swap implementations. This may be revisited.

## Extension Points

Consumers extend ollie by implementing or composing its interfaces:

- **`backend.Backend`** — swap or add LLM backends
- **`tools.Executor`** — replace the tool router (e.g. remote dispatcher, mock)
- **`agent.Core`** — the agent's public API; frontends drive it without knowing internals

`tools.NewExecutor()` returns a `*MCPExecutor` (concrete, for `AddServer` during setup), then is used as `tools.Executor` (interface) at runtime.

## Data Flow

```
Frontend
  └── agent.Core.Submit(prompt)
        └── loop.run()
              ├── backend.ChatStream()                   — LLM call
              └── tools.Executor.Execute()               — tool dispatch (interface)
                    ├── tools.MCPExecutor                — default impl: MCP server tools
                    └── tools/execute.Executor.Execute() — built-in sandbox tools
```

## Built-in Tools

Three tools are registered by default:

| Tool | What it does |
|---|---|
| `execute_code` | Runs inline bash in a landrun sandbox |
| `execute_tool` | Reads a named script from `OLLIE_TOOLS_PATH` and runs it sandboxed |
| `execute_pipe` | Chains steps, piping stdout of each into stdin of the next |

`OLLIE_TOOLS_PATH` defaults to `~/.local/share/ollie/tools`. The directory can be a symlink or a 9pfuse mountpoint — `execute_tool` treats it as an ordinary filesystem path.

## Session and Context

`agent.Session` owns the message history. A `contextBuilder` enforces a rolling window to prevent unbounded prompt growth. When the window fills, older messages are either evicted (with a compaction notice injected) or proactively summarized via `/compact`.

## Sandboxing

`internal/sandbox` wraps commands with [landrun](https://github.com/landlock-lsm/landrun) (Landlock LSM). Configuration is layered:

1. Global defaults (`~/.config/ollie/sandbox.yaml`)
2. Named sandbox overlay (`~/.config/ollie/sandbox/<name>.yaml`)

If `superpowerd` is running, commands are additionally wrapped with `superpowers run-session` for privilege scoping (optional, detected at runtime).

## Frontends

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui): a readline-based terminal UI that imports ollie as a library. It is the only consumer of `agent.Core` today, but the interface is intentionally frontend-agnostic.
