# Ollie

An agentic CLI built on a sandboxed `execute_code` tool and a common LLM backend interface. Supports local (Ollama) and OpenAI-compatible backends, optional MCP server connections, and a skill system for domain-specific capabilities.

Intended to be used with [anvillm](https://github.com/lneely/anvillm), which provides the skill system, tool scripts, and multi-agent infrastructure that ollie builds on.

## Build

```bash
go build
```

Requires Go 1.21+.

## Run

```bash
./ollie [model]
```

Model defaults to `qwen3:8b`. Override with `OLLIE_MODEL` or pass as the first argument:

```bash
OLLIE_BACKEND=openai ./ollie qwen/qwen3-235b-a22b
```

## Configuration

### Environment: `~/.config/ollie/env`

```
OLLIE_BACKEND=openai           # ollama | openai (default: ollama)
OLLIE_OLLAMA_URL=              # base URL for Ollama (default: http://localhost:11434)
OLLIE_OPENAI_URL=https://openrouter.ai/api  # any OpenAI-compatible endpoint
OLLIE_OPENAI_KEY=sk-or-...
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

MCP server `env` values support `${VAR}` expansion from the parent environment. Only whitelisted vars are passed to the subprocess.

An alternate config path can be provided as a second argument:

```bash
./ollie qwen3:8b /path/to/config.json
```

## UI

- **Enter** — submit message
- **Ctrl+J** — insert newline
- **PageUp / PageDown** — scroll chat history
- **Ctrl+U / Ctrl+D** — half-page scroll
- **Ctrl+C / Esc** — quit

Token usage is shown after each response: `[↑N ↓N tokens]`.

## Tools

The agent has one built-in tool: `execute_code`. It runs shell code in a sandboxed environment and supports three invocation modes:

- `code` — inline bash
- `tool` + `args` — named tool script
- `pipe` — sequence of `{tool, args}` steps

MCP server tools are discovered at startup and available alongside `execute_code`.

## Skills

Skills are domain-specific tool scripts and knowledge discoverable at runtime:

```bash
{"tool": "discover_skill.sh", "args": ["keyword"]}
{"tool": "load_skill.sh",     "args": ["skill-name"]}
```

The agent discovers and loads relevant skills automatically at the start of each task.

## License

GPLv3
