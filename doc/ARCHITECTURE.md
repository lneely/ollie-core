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
- **task backend (MCP)** — any MCP server that exposes `task_create` is automatically wired as the persistence backend for `reasoning_plan` by `BuildAgentEnv`; no consumer code required. See [9beads-mcp](https://github.com/lneely/9beads-mcp) for the reference implementation and the interface contract.
- **fallback plan backend** — consumers can supply a `tools.PlanBackend` via `agent.WithFallbackPlanBackend` that is used when no `task_create` MCP tool is found. ollie-9p uses this to enqueue plan steps into the session queue.
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
                    ├── "reasoning" → tools.Server
                    │       reasoning_think
                    │       reasoning_plan
                    │         └── tools.PlanBackend (optional, priority order)
                    │               1. dispatchPlanBackend → task_create (MCP)
                    │               2. WithFallbackPlanBackend (e.g. queuePlanBackend)
                    │               3. nil → in-context plan only
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
| `reasoning` | `reasoning_think` | Externalizes intermediate reasoning (no-op, recorded in history) |
| `reasoning` | `reasoning_plan` | Decomposes a goal into ordered steps; persists to task backend if available |

File operations go through `execute_code` using standard shell tools (`cat`, `grep`, `sed`, `ed`, `ssam` if plan9port is available, etc.).

Tool definitions live in `pkg/tools/builtin.go` to avoid import cycles — the subpackages import `pkg/tools`, so `pkg/tools` cannot import them back.

`OLLIE_TOOLS_PATH` defaults to `~/.local/share/ollie/tools`. The directory can be a symlink or a mountpoint — `execute_tool` treats it as an ordinary filesystem path.

## Planning and Task Persistence

`reasoning_plan` is a meta-cognitive tool for executive planning. It decomposes a goal into a dependency graph of steps before execution begins. If a task backend is available, the plan is committed to persistent storage.

The task backend is any MCP server that exposes a `task_create` tool. `BuildAgentEnv` scans available tools after connecting MCP servers: if `task_create` is found, it wires a `dispatchPlanBackend` to the reasoning server's `Plan` field via the `tools.PlanBackendSetter` interface. If not found, it falls back to any `PlanBackend` supplied via `WithFallbackPlanBackend`. If neither is present, `reasoning_plan` produces an in-context plan only.

ollie-9p supplies a `queuePlanBackend` fallback for every session: when `task_create` is absent, plan steps are enqueued to the session's `enqueue` file in topological order and returned as placeholder IDs (`q1`, `q2`, …). This implementation lives in `9p` because it has a hard dependency on the 9P filesystem layout; the extension point itself (`tools.PlanBackend` + `WithFallbackPlanBackend`) remains in core.

The reference task backend is [9beads-mcp](https://github.com/lneely/9beads-mcp), which wraps the [9beads](https://github.com/lneely/9beads) 9P task server.

### Task interface contract

Auto-wiring requires only `task_create`. The minimum viable contract is:

| Tool | Required | Purpose |
|---|---|---|
| `task_create` | yes | Create a task; must return a plain-text ID |
| `task_delete` | recommended | Remove tasks when a plan is aborted or superseded |
| `task_list` | optional | Orient the agent across sessions |
| `task_read` | optional | Inspect a specific task |
| `task_update` | optional | Claim, complete, fail, defer, label tasks |
| `task_edit` | optional | Revise title, body, or parent of an existing task |
| `task_dep` | optional | Add or remove blocking dependencies between tasks |

Ollie degrades gracefully: if a tool is absent, the agent falls back to `execute_code` (shell-out) or skips that lifecycle step. The only hard requirement for persistent planning is `task_create` returning an ID in the `{"content": [{"type": "text", "text": "<id>"}]}` format.

See [doc/PLANNING.md](doc/PLANNING.md) for the full design rationale.

## Typical Consumer Setup

```go
newDispatcher := tools.NewDispatcherFunc(map[string]func() tools.Server{
    "execute":   execute.Decl(workdir),  // "" falls back to os.Getwd()
    "reasoning": reasoning.Decl(),
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

After connecting MCP servers, `BuildAgentEnv` scans the tool list for `task_create`. If found, it wires a `dispatchPlanBackend` to the reasoning server's `Plan` field via `tools.PlanBackendSetter`. If not found, it wires any fallback passed via `WithFallbackPlanBackend`. This auto-wiring runs on every agent start and `/agent` switch.

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
