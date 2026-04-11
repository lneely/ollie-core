# Planning and Task Persistence in Ollie

## Problem

An agent without planning is less effective but still useful. Planning capability
should be optional — degraded but functional when no task backend is available,
and persistent when one is.

The challenge: how to integrate a planning tool with an optional external task
backend without hard-coupling the core to any specific implementation.

## Design Decisions

### reasoning_plan as a built-in tool

Planning is executive functioning — breaking a goal into ordered steps before
acting. It belongs in the `reasoning_*` namespace alongside `reasoning_think`,
which handles moment-to-moment reflection. Both are meta-cognitive tools:
`reasoning_think` externalizes intermediate thought; `reasoning_plan`
externalizes the work breakdown.

Tool schemas are more reliable than free-text instructions over long sessions.
Agents rarely forget how to call `file_read`; they can forget shell conventions.
The planning operation is complex enough (goal + structured steps + dependency
edges) that a typed schema earns its place.

`reasoning_plan` does not rely on shell-out. It takes structured JSON, formats
the plan as readable text, and optionally persists it. This is the right
minimum for a built-in tool.

### Loose coupling via PlanBackend interface

The reasoning server holds an optional `Plan tools.PlanBackend` field (nil by
default). When nil, `reasoning_plan` produces an in-context plan only. When set,
the plan is persisted (or queued) by the backend.

`tools.PlanBackend` and `tools.PlanBackendSetter` are defined in `pkg/tools`.
The reasoning server implements `PlanBackendSetter`. The agent package implements
`dispatchPlanBackend`, which routes task creation through the dispatcher. No
import cycles: reasoning → tools, agent → tools, agent ↛ reasoning.

Consumers can supply a fallback backend via `agent.WithFallbackPlanBackend(b)`:

```go
env := agent.BuildAgentEnv(cfg, d, workdir, agent.WithFallbackPlanBackend(myFallback))
```

The fallback is only used when no `task_create` MCP tool is found. If a task
backend is available it takes priority unconditionally.

### Task backend as an MCP server

The task backend is any MCP server that exposes `task_create`. There is no
built-in task server in ollie core — this is an intentional extension point.
The convention (the interface contract) is:

| Tool | Required | Description |
|---|---|---|
| `task_create` | yes | Create a task; return its ID as plain text |
| `task_delete` | recommended | Remove tasks when a plan is aborted or superseded |
| `task_list` | no | List tasks by status |
| `task_read` | no | Read a task by ID |
| `task_update` | no | Update task status/metadata |
| `task_edit` | no | Revise title, body, or parent of an existing task |
| `task_dep` | no | Add or remove blocking dependencies between tasks |

`task_create` must return a plain-text ID in the standard MCP content format:
`{"content": [{"type": "text", "text": "<id>"}]}`.

Any MCP server implementing this convention will be auto-detected and wired.

### Auto-wiring in BuildAgentEnv

After connecting MCP servers, `BuildAgentEnv` scans the tool list for
`task_create`. If found, it constructs a `dispatchPlanBackend` and sets it on
the reasoning server via `PlanBackendSetter`. This wiring runs on every agent
start and `/agent` switch, so the task backend is always current.

If `task_create` is not found, `BuildAgentEnv` checks whether the caller
supplied a fallback via `WithFallbackPlanBackend`. If so, that backend is wired
instead. Priority: task MCP > caller fallback > nil (in-context only).

No frontend changes are needed to gain planning capability — add a task MCP
server to the agent config and it works.

### Graceful degradation

- No task MCP server configured, no fallback → `reasoning_plan` produces an
  in-context plan only.
- No task MCP server configured, fallback supplied → steps are handed to the
  fallback backend (e.g. queued for sequential execution). See
  [Queue-based fallback](#queue-based-fallback) below.
- Task MCP server configured but unreachable → `task_create` fails,
  `CreatePlan` returns an error, reasoning server degrades to in-context plan
  with a warning.
- Task MCP server running → full persistence, steps get IDs, plan is durable.

The agent's behavior is identical in all cases: call `reasoning_plan`, get a
formatted plan, proceed. The persistence difference is transparent.

### Queue-based fallback

ollie-9p registers a `queuePlanBackend` as the fallback for every session. When
`task_create` is absent, `reasoning_plan` writes each step as a queued prompt to
the session's `enqueue` file in topological order (blockers before dependents).
The implementation lives in `9p` (not core) because it has a hard dependency on
the 9P filesystem layout — the extension point is the interface, not this
specific implementation.

Steps are returned as placeholder IDs (`q1`, `q2`, …) so the agent can refer to
them in subsequent `reasoning_think` calls. The queue persists independently of
context — unlike an in-context-only plan, steps survive compaction.

The goal description is prepended to the first enqueued step for context.
Dependency annotations (`after: q1, q2`) are included in each step's prompt so
the agent retains the relationship even if it processes steps across multiple
turns.

Fan-out parallelism is achievable by spawning sub-sessions: the enqueued step
can instruct the agent to create a child session per parallel branch, nudge each
with its task via `prompt`, and have sub-agents write completion notifications
back to the parent via `enqueue`.

### Shell-out for everything else

Queries, comments, bulk operations, event watching — all of these belong in
`execute_code`, not in built-in tools. The `task_*` MCP tools cover the full
planning and execution lifecycle (create, list, read, update, edit, delete,
dependency management). For example, advanced 9beads operations are accessible
via shell: `cat $TASK_DIR/list`, `grep`, etc.

This keeps the built-in surface minimal and relies on `execute_code` for
flexibility.

## Reference Implementation: 9beads-mcp

[9beads-mcp](https://github.com/lneely/9beads-mcp) wraps the 9beads 9P task
server as an MCP server. It:

- Resolves the project mount from `$PWD` (passed explicitly in the MCP server
  env config, since MCP subprocesses run in a minimal environment)
- Auto-mounts the project directory if not already mounted
- Exposes `task_create`, `task_list`, `task_read`, `task_update`, `task_edit`, `task_delete`, `task_dep`

Agent config example:

```yaml
mcpServers:
  task:
    command: 9beads-mcp
    env:
      PWD: "$PWD"
```

`$PWD` is expanded by `os.ExpandEnv` at connect time, giving the MCP server
the agent's working directory.

## Future Direction: Event-Driven Planning

9beads exposes `~/mnt/beads/events` as a blocking JSON event stream. A
goroutine in the frontend could watch this and inject events into the agent's
interrupt queue (via `PromptFIFO`) when:

- A blocked step becomes unblocked (its dependency completed)
- A step is assigned to this agent by an external actor
- An external process marks a step complete

This would make agents reactive rather than polling — the agent yields after
completing work and wakes up when the event stream fires. Not implemented yet;
documented here as the natural next step.
