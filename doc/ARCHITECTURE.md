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
  tools/execute/   — execute.Server: execute_code, execute_tool, execute_pipe
  tools/file/      — file.Server: file_read, file_write
  tools/reasoning/ — reasoning.Server: reasoning_think, reasoning_plan

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
- **`tools.PlanBackend`** — optional persistence backend for `reasoning_plan`; implement and wire via `tools.PlanBackendSetter` to persist plans to any task store
- **`agent.Core`** — the agent's public API; frontends drive it without knowing internals

All tool servers implement the same `tools.Server` interface regardless of whether they are built-in or backed by MCP. There is no special "builtin" concept — `execute.Server` and `file.Server` are registered by name the same way MCP servers are, and are torn down and recreated on `/agent` switches just like MCP connections.

Each built-in server package exports a `Decl` function (`func Decl(...) func() tools.Server`) — a parameterized factory that produces a fresh server instance. `tools.NewDispatcherFunc` takes a map of name→Decl result and returns a `func() tools.Dispatcher` suitable for `agent.AgentCoreConfig.NewDispatcher`. `tools.NewServer(client)` wraps an `mcp.Client` as a `tools.Server`.

`execute.Decl(workdir string)` accepts a working directory that is set as `cmd.Dir` for sandboxed commands and used to expand `{CWD}` in the sandbox config. Pass `""` to fall back to `os.Getwd()`.

**Adding a new tool server:**
1. Implement `tools.Server` in a new package under `pkg/tools/`
2. Export `func Decl(...) func() tools.Server`
3. Add tool definitions to `pkg/tools/builtin.go` (avoids import cycles)
4. Register the Decl result by name in `tools.NewDispatcherFunc` — no frontend changes needed

## Data Flow

```
Frontend
  └── agent.Core.Submit(prompt)
        └── loop.run()
              ├── backend.ChatStream()              — LLM call
              └── tools.Dispatcher.Dispatch(...)   — tool dispatch
                    ├── "execute"   → tools.Server
                    │       execute_code / execute_tool / execute_pipe
                    ├── "file"      → tools.Server
                    │       file_read / file_write
                    ├── "reasoning" → tools.Server
                    │       reasoning_think
                    │       reasoning_plan
                    │         └── tools.PlanBackend (optional)
                    │               task_create / ... (any MCP task server)
                    └── "<servername>"  → tools.Server
```

All tool calls go through one `tools.Dispatcher.Dispatch` path. Every registered server is a `tools.Server`; the dispatcher routes by name and is agnostic to how any server is implemented.

## Built-in Tools

Seven tools are registered across three servers:

| Server | Tool | What it does |
|---|---|---|
| `execute` | `execute_code` | Runs inline bash in a landrun sandbox |
| `execute` | `execute_tool` | Reads a named script from `OLLIE_TOOLS_PATH` and runs it sandboxed |
| `execute` | `execute_pipe` | Chains steps, piping stdout of each into stdin of the next |
| `file` | `file_read` | Reads a file with line numbers (required before `file_write`) |
| `file` | `file_write` | Writes or patches a file by line range |
| `reasoning` | `reasoning_think` | Externalizes intermediate reasoning (no-op, recorded in history) |
| `reasoning` | `reasoning_plan` | Decomposes a goal into ordered steps; persists to task backend if available |

Tool definitions live in `pkg/tools/builtin.go` to avoid import cycles — the subpackages import `pkg/tools`, so `pkg/tools` cannot import them back.

`OLLIE_TOOLS_PATH` defaults to `~/.local/share/ollie/tools`. The directory can be a symlink or a mountpoint — `execute_tool` treats it as an ordinary filesystem path.

## Planning and Task Persistence

`reasoning_plan` is a meta-cognitive tool for executive planning. It decomposes a goal into a dependency graph of steps before execution begins. If a task backend is available, the plan is committed to persistent storage.

The task backend is any MCP server that exposes a `task_create` tool. `BuildAgentEnv` scans available tools after connecting MCP servers: if `task_create` is found, it wires a `dispatchPlanBackend` to the reasoning server's `Plan` field via the `tools.PlanBackendSetter` interface. If not found, `reasoning_plan` produces an in-context plan only — degraded but functional.

The reference task backend is [9beads-mcp](https://github.com/lneely/9beads-mcp), which wraps the [9beads](https://github.com/lneely/9beads) 9P task server. Any MCP server that exposes `task_create` (and optionally `task_list`, `task_read`, `task_update`) satisfies the interface.

See [doc/PLANNING.md](doc/PLANNING.md) for the full design rationale.

## Typical Consumer Setup

```go
newDispatcher := tools.NewDispatcherFunc(map[string]func() tools.Server{
    "execute": execute.Decl(workdir),  // "" falls back to os.Getwd()
    "file":    file.Decl,
})

env := agent.BuildAgentEnv(cfg, newDispatcher(), workdir)  // also connects MCP servers from cfg

core := agent.NewAgentCore(agent.AgentCoreConfig{
    Backend:       be,
    WorkDir:       workdir,
    Env:           env,
    NewDispatcher: newDispatcher,
    // ...
})
```

`BuildAgentEnv` adds MCP servers from the config on top of the pre-registered servers. On `/agent` switches, `NewDispatcher` is called to produce a fresh dispatcher — all servers (built-in and MCP) are torn down and recreated for the new agent config. `WorkDir` is preserved across switches.

After connecting MCP servers, `BuildAgentEnv` scans the tool list for `task_create`. If found, it wires a `dispatchPlanBackend` to the reasoning server's `Plan` field via `tools.PlanBackendSetter`. This auto-wiring runs on every agent start and `/agent` switch.

`Core.ListServers()` returns all registered tool servers and their tools, grouped by server name. Accessible via the `/mcp` command or `ollie/s/{sid}/mcp` in ollie-9p.

## Session and Context

`agent.Session` owns the message history. A `contextBuilder` enforces a rolling window to prevent unbounded prompt growth. When the window fills, older messages are either evicted (with a compaction notice injected) or proactively summarized via `/compact`.

## Sandboxing

`internal/sandbox` wraps commands with [landrun](https://github.com/landlock-lsm/landrun) (Landlock LSM). Configuration is layered:

1. Global defaults (`~/.config/ollie/sandbox.yaml`)
2. Named sandbox overlay (`~/.config/ollie/sandbox/<name>.yaml`)

If `superpowerd` is running, commands are additionally wrapped with `superpowers run-session` for privilege scoping (optional, detected at runtime).

## Frontends

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui): a readline-based terminal UI that imports ollie as a library. It is the only consumer of `agent.Core` today, but the interface is intentionally frontend-agnostic.
