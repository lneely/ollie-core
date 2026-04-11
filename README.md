# ollie

A Go library for building agentic systems. Provides a sandboxed `execute_code` tool, a common LLM backend interface, MCP server support, and a skill system for domain-specific capabilities.

Works well with [anvillm](https://github.com/lneely/anvillm), which provides a skill system, tool scripts, and multi-agent infrastructure.

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui), a terminal UI built on top of this library.

## Packages

```
pkg/agent/         — Core interface, agent loop, session management
pkg/backend/       — Backend interface + implementations (Ollama, OpenAI, Anthropic, Copilot, Kiro)
pkg/config/        — Config struct and loader
pkg/mcp/           — MCP client
pkg/tools/         — Server and Dispatcher interfaces; tool definitions
pkg/tools/execute/ — execute.Server: execute_code, execute_tool, execute_pipe
pkg/tools/file/    — file.Server: file_read, file_write
```

## Install

```
mk
```

Copies agent configs to `~/.config/ollie/agents/`.

## Configuration

### Environment: `~/.config/ollie/env`

```
OLLIE_BACKEND=openai           # ollama | openai | openrouter | anthropic | copilot | kiro (default: ollama)
OLLIE_OLLAMA_URL=              # base URL for Ollama (default: http://localhost:11434)
OLLIE_OPENAI_URL=https://openrouter.ai/api
OLLIE_OPENAI_KEY=sk-or-...
OLLIE_ANTHROPIC_KEY=sk-ant-...
OLLIE_COPILOT_TOKEN=...
OLLIE_KIRO_TOKEN=...           # bearer token or sqlite:// path (auto-detected from Kiro CLI if unset)
OLLIE_MODEL=qwen/qwen3-235b-a22b
OLLIE_TOOLS_PATH=~/.local/share/ollie/tools  # directory for execute_tool scripts
```

Shell environment variables take precedence over the env file.

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

Five built-in tools across two servers:

- `execute_code` — run inline shell code in a sandbox
- `execute_tool` — run a named tool script from `OLLIE_TOOLS_PATH` (default: `~/.local/share/ollie/tools`)
- `execute_pipe` — chain steps as a pipeline
- `file_read` — read a file with line numbers (required before `file_write`)
- `file_write` — write or patch a file by line range

MCP server tools are discovered at startup and available alongside the built-ins.

Each tool server exports a `Decl` function that returns a `func() tools.Server` factory. `execute.Decl(workdir)` accepts a working directory used as `cmd.Dir` for sandboxed commands and for `{CWD}` expansion in the sandbox config; pass `""` to fall back to `os.Getwd()`. Frontends register servers by passing Decl results to `tools.NewDispatcherFunc`. Adding a new tool means implementing `tools.Server`, exporting a `Decl` function, and registering it — no frontend changes required.

## Skills

Skills are domain-specific tool scripts and knowledge discoverable at runtime:

```bash
{"tool": "discover_skill.sh", "args": ["keyword"]}
{"tool": "load_skill.sh",     "args": ["skill-name"]}
```

## Integrations

**[9beads-mcp](https://github.com/lneely/9beads-mcp)** provides task persistence using **[9beads](https://github.com/lneely/9beads)** — enabling the agent to track, list, and manage tasks across sessions.

## Credits

Many sources of inspiration:

- [Plan 9 from Bell Labs](https://9fans.net) — for an interesting system
- [@9fans](https://github.com/9fans) — for the Plan 9 port
- [Suckless](https://suckless.org) — for articulating good software development principles
- [@simonfxr](https://github.com/simonfxr) — for a solid agent baseline to "borrow" from, and other nifty ideas
- [@aws](https://github.com/aws/amazon-q-developer-cli) — for a solid open-source agent implementation

## License

GPLv3
