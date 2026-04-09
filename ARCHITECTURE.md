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
  tools/         — Server and Dispatcher interfaces, tool definitions (builtin.go)
  tools/execute/ — execute.Server: execute_code, execute_tool, execute_pipe
  tools/file/    — file.Server: file_read, file_write

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
- **`tools.Server`** — add a new tool server (built-in or MCP-backed); all servers are equal
- **`tools.Dispatcher`** — replace the tool router entirely (e.g. remote dispatcher, mock)
- **`agent.Core`** — the agent's public API; frontends drive it without knowing internals

All tool servers implement the same `tools.Server` interface regardless of whether they are built-in or backed by MCP. There is no special "builtin" concept — `execute.Server` and `file.Server` are just servers registered by name, the same way MCP servers are.

`tools.NewDispatcher()` returns a `tools.Dispatcher`. Callers register servers with `d.AddServer(name, server)` before passing `d` to `agent.BuildAgentEnv`. `tools.NewServer(client)` wraps an `mcp.Client` as a `tools.Server`.

## Data Flow

```
Frontend
  └── agent.Core.Submit(prompt)
        └── loop.run()
              ├── backend.ChatStream()              — LLM call
              └── tools.Dispatcher.Dispatch(...)   — tool dispatch
                    ├── "execute" → tools.Server
                    │       execute_code / execute_tool / execute_pipe
                    ├── "file"    → tools.Server
                    │       file_read / file_write
                    └── "<servername>"  → tools.Server
```

All tool calls go through one `tools.Dispatcher.Dispatch` path. Every registered server is a `tools.Server`; the dispatcher routes by name and is agnostic to how any server is implemented.

## Built-in Tools

Five tools are registered across two servers:

| Server | Tool | What it does |
|---|---|---|
| `execute` | `execute_code` | Runs inline bash in a landrun sandbox |
| `execute` | `execute_tool` | Reads a named script from `OLLIE_TOOLS_PATH` and runs it sandboxed |
| `execute` | `execute_pipe` | Chains steps, piping stdout of each into stdin of the next |
| `file` | `file_read` | Reads a file with line numbers (required before `file_write`) |
| `file` | `file_write` | Writes or patches a file by line range |

Tool definitions (`ExecuteDefs`, `FileDefs`) live in `pkg/tools/builtin.go` to avoid import cycles — the subpackages import `pkg/tools`, so `pkg/tools` cannot import them back.

`OLLIE_TOOLS_PATH` defaults to `~/.local/share/ollie/tools`. The directory can be a symlink or a mountpoint — `execute_tool` treats it as an ordinary filesystem path.

## Typical Consumer Setup

```go
execServer := execute.New(logDir, workspaceBase)
fileServer := file.New()

d := tools.NewDispatcher()
d.AddServer("execute", execServer)
d.AddServer("file", fileServer)

env := agent.BuildAgentEnv(cfg, d)  // also connects MCP servers from cfg

core := agent.NewAgentCore(agent.AgentCoreConfig{
    Backend:    be,
    ExecServer: execServer,
    FileServer: fileServer,
    Env:        env,
    // ...
})
```

`BuildAgentEnv` adds MCP servers from the config on top of the pre-registered servers. `ExecServer` and `FileServer` are stored on the core so `/agent` switching can rebuild the dispatcher with the same sandboxed servers plus new MCP connections.

## Session and Context

`agent.Session` owns the message history. A `contextBuilder` enforces a rolling window to prevent unbounded prompt growth. When the window fills, older messages are either evicted (with a compaction notice injected) or proactively summarized via `/compact`.

## Sandboxing

`internal/sandbox` wraps commands with [landrun](https://github.com/landlock-lsm/landrun) (Landlock LSM). Configuration is layered:

1. Global defaults (`~/.config/ollie/sandbox.yaml`)
2. Named sandbox overlay (`~/.config/ollie/sandbox/<name>.yaml`)

If `superpowerd` is running, commands are additionally wrapped with `superpowers run-session` for privilege scoping (optional, detected at runtime).

## Frontends

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui): a readline-based terminal UI that imports ollie as a library. It is the only consumer of `agent.Core` today, but the interface is intentionally frontend-agnostic.
