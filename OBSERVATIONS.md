# Observations

Running notes on design decisions, experiments, and lessons learned during
development. Entries are roughly chronological.

---

## Tool Schema Complexity and Model Capability

**Context.** The execute package originally exposed a single `execute_code`
tool with a flexible schema: callers could pass inline `steps`, a `pipe` array
for sequential stdin-chained execution, tool-name references, parallel fan-out
groups, elevated steps, and a sandbox selector — all through one JSON object.

**Observation.** Larger models with strong reasoning ability (e.g. frontier
chat models and extended-thinking variants) handled the unified schema well.
They correctly discriminated between the `steps` and `pipe` fields, chose
appropriate execution modes, and composed pipelines without confusion.

Smaller models struggled. Common failure modes:

- Emitting both `steps` and `pipe` in the same call, producing ambiguous or
  undefined behaviour.
- Using `pipe` where `steps` was correct (or vice versa), because the
  distinction between "parallel batch" and "stdin pipeline" was not obvious
  from a single tool description.
- Ignoring the `tool` field inside a step and writing inline shell code that
  reimplemented an existing named tool.

**Decision.** Split `execute_code` into three purpose-specific tools:

| Tool | Input key | Purpose |
|------|-----------|---------------------------|
| `execute_code` | `steps` | Arbitrary inline code. Each step is independent; read-safe steps may run in parallel automatically. |
| `call_tool` | `calls` | Named scripts from the tools directory. Inline `code` fields are rejected at dispatch. |
| `pipe` | `stages` | Sequential pipeline. stdout of each stage feeds stdin of the next. Stages may be inline code or named tools. |

The `parallel` option (a `parallel` array inside a step) is retained in
`execute_code` and `pipe` for fan-out within a stage. This gives smaller
models a clear, narrow contract per tool without removing any capability.

**Trade-off.** Sophisticated models now make three tool choices where they
previously made one. For those models this is a minor regression in ergonomics.
The bet is that the schema clarity gained at the smaller-model end outweighs
the slight overhead at the larger-model end, and that a well-prompted larger
model will use the right tool consistently regardless.
