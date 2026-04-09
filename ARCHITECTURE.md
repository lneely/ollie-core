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
  tools/         — Server/Executor interfaces, MCPExecutor, BuiltinServer, tool definitions
  tools/execute/ — Sandbox runner (execute_code, execute_tool, execute_pipe)
  tools/file/    — File operations (file_read, file_write)

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
- **`tools.Server`** — add a new tool server (built-in or MCP-backed); register with `MCPExecutor.AddServer`
- **`tools.Executor`** — replace the tool router entirely (e.g. remote dispatcher, mock)
- **`agent.Core`** — the agent's public API; frontends drive it without knowing internals

`tools.NewExecutor()` returns a `*MCPExecutor` (concrete, for `AddServer` during setup), then is used as `tools.Executor` (interface) at runtime. `tools.NewMCPServer(client)` wraps an `mcp.Client` as a `tools.Server`.

## Data Flow

```
Frontend
  └── agent.Core.Submit(prompt)
        └── loop.run()
              ├── backend.ChatStream()                        — LLM call
              └── tools.Executor.Execute(server, tool, args)  — tool dispatch (interface)
                    └── tools.MCPExecutor                     — routes by server name
                          ├── "builtin" → tools.BuiltinServer    — all built-in tools
                          │       ├── execute_* → execute.Executor.Dispatch()
                          │       └── file_*    → file.Read / file.Write
                          └── "<name>"  → tools.mcpServer        — MCP protocol tools
```

All tool calls go through one `tools.Executor.Execute` path. `BuiltinServer` in `pkg/tools/` is the single built-in server; `execute/` and `file/` are its implementation subpackages and do not implement `tools.Server` themselves. MCP tools are wrapped in `mcpServer`.

## Built-in Tools

Five tools are registered under the `"builtin"` server by default:

| Tool | What it does |
|---|---|
| `execute_code` | Runs inline bash in a landrun sandbox |
| `execute_tool` | Reads a named script from `OLLIE_TOOLS_PATH` and runs it sandboxed |
| `execute_pipe` | Chains steps, piping stdout of each into stdin of the next |
| `file_read` | Reads a file with line numbers (required before `file_write`) |
| `file_write` | Writes or patches a file by line range |

`tools.BuiltinServer` in `pkg/tools/` is the single `tools.Server` for all built-in tools. Tool definitions (`ExecuteDefs`, `FileDefs`) and file dispatch live in `pkg/tools/builtin.go`; execute dispatch lives in `pkg/tools/execute/dispatch.go`.

`OLLIE_TOOLS_PATH` defaults to `~/.local/share/ollie/tools`. The directory can be a symlink or a mountpoint — `execute_tool` treats it as an ordinary filesystem path.

## Session and Context

`agent.Session` owns the message history. A `contextBuilder` enforces a rolling window to prevent unbounded prompt growth. When the window fills, older messages are either evicted (with a compaction notice injected) or proactively summarized via `/compact`.

## Sandboxing

`internal/sandbox` wraps commands with [landrun](https://github.com/landlock-lsm/landrun) (Landlock LSM). Configuration is layered:

1. Global defaults (`~/.config/ollie/sandbox.yaml`)
2. Named sandbox overlay (`~/.config/ollie/sandbox/<name>.yaml`)

If `superpowerd` is running, commands are additionally wrapped with `superpowers run-session` for privilege scoping (optional, detected at runtime).

## Frontends

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui): a readline-based terminal UI that imports ollie as a library. It is the only consumer of `agent.Core` today, but the interface is intentionally frontend-agnostic.
