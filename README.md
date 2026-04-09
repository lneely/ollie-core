# ollie

A Go library for building agentic systems. Provides a sandboxed `execute_code` tool, a common LLM backend interface, MCP server support, and a skill system for domain-specific capabilities.

Intended to be used with [anvillm](https://github.com/lneely/anvillm), which provides the skill system, tool scripts, and multi-agent infrastructure that ollie builds on.

The reference frontend is [ollie-tui](https://github.com/lneely/ollie-tui), a terminal UI built on top of this library.

## Packages

```
pkg/agent/         — Core interface, agent loop, session management
pkg/backend/       — Backend interface + implementations (Ollama, OpenAI, Anthropic, Copilot, Kiro)
pkg/config/        — Config struct and loader
pkg/mcp/           — MCP client
pkg/tools/         — Executor interface + MCPExecutor
pkg/tools/execute/ — Built-in sandbox executor (execute_code, execute_tool, execute_pipe)
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

## Tools

Three built-in tool modes:

- `execute_code` — run inline shell code in a sandbox
- `execute_tool` — run a named tool script from the tools directory
- `execute_pipe` — chain steps as a pipeline

MCP server tools are discovered at startup and available alongside the built-ins.

## Skills

Skills are domain-specific tool scripts and knowledge discoverable at runtime:

```bash
{"tool": "discover_skill.sh", "args": ["keyword"]}
{"tool": "load_skill.sh",     "args": ["skill-name"]}
```

## License

GPLv3
