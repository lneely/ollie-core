package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"ollie/agent"
	"ollie/backend"
	"ollie/config"
	execpkg "ollie/exec"
	"ollie/mcp"
	"ollie/tools"
)

const systemPrompt = `You are an autonomous agent. You have one tool: execute_code.

## execute_code — when to use it

Use execute_code whenever the task requires:
- running shell commands or scripts
- reading or writing files
- making network requests
- any information you cannot reliably state from memory

Do not answer from memory when you can verify with execute_code.

Do not describe what you would run or show code blocks. Call execute_code
directly. If you find yourself writing a markdown code block, stop and make
the tool call instead.

## execute_code — how to call it

Three modes (pick one per call):

  Inline code:
    {"code": "echo hello", "language": "bash", "timeout": 30, "sandbox": "default"}

  Named tool script:
    {"tool": "some_tool.sh", "args": ["--flag", "value"]}

  Pipeline:
    {"pipe": [{"tool": "tool1.sh", "args": []}, {"tool": "tool2.sh", "args": ["x"]}]}

language, timeout, and sandbox are optional (defaults: bash, 30s, default).

A permission error from the sandbox does not always mean a file is missing or
unexecutable — it may be a sandbox policy boundary. If you hit one, try a
different tool or approach rather than retrying the same call.

## Step 1 — discover and load skills (always do this first)

Before anything else, call execute_code using the "tool" field (not "code")
with keywords derived from the goal and the current working directory:

  {"tool": "discover_skill.sh", "args": ["keyword"]}

If a relevant skill is found, load it the same way:

  {"tool": "load_skill.sh", "args": ["skill-name"]}

Read and follow the loaded skill's instructions before proceeding.
Re-run discovery if the task domain shifts during execution.

Never use the "code" field to run discover_skill.sh or load_skill.sh.

## Step 2 — plan, then execute immediately

After skill discovery, write a short numbered plan, then execute it without
waiting for user input:
1. Restate the goal.
2. List the steps you will take.
3. Begin executing the first step immediately — use execute_code if the step
   requires it, or respond directly if no tool call is needed.

Do not stop after planning. Do not ask for confirmation. Work through each
step in sequence. Revise the plan if a step fails or reveals new information.`

// executeCodeTool is the single built-in tool exposed to the model.
var executeCodeTool = backend.Tool{
	Name: "execute_code",
	Description: "Execute shell code or a named tool script in a sandboxed environment. " +
		"Use 'code' for inline bash, 'tool'+'args' for a named script, " +
		"or 'pipe' for a sequence of {tool, args} steps.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"properties": {
			"code":     {"type": "string",  "description": "Inline shell code to run (bash)."},
			"language": {"type": "string",  "description": "Language interpreter (default: bash)."},
			"timeout":  {"type": "integer", "description": "Timeout in seconds (default: 30)."},
			"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."},
			"tool":     {"type": "string",  "description": "Named tool script to run instead of inline code."},
			"args":     {"type": "array",   "items": {"type": "string"}, "description": "Arguments for the tool script."},
			"pipe":     {"type": "array",   "description": "Pipeline: array of {tool, args} objects run in sequence."}
		}
	}`),
}

type model struct {
	textarea textarea.Model
	viewport viewport.Model
	session  *agent.Session
	loopcfg  agent.Config
	display  []string
	ready    bool
	hooks    map[string]string
}

func main() {
	modelName := os.Getenv("OLLIE_MODEL")
	if modelName == "" && len(os.Args) > 1 {
		modelName = os.Args[1]
	}
	if modelName == "" {
		modelName = "qwen3:8b"
	}

	hooks := make(map[string]string)
	var cfg *config.Config
	cfgPath := ""
	if len(os.Args) > 2 {
		cfgPath = os.Args[2]
	} else {
		home, _ := os.UserHomeDir()
		cfgPath = home + "/.config/ollie/config.json"
	}
	if c, err := config.Load(cfgPath); err == nil {
		cfg = c
		if cfg.Hooks != nil {
			hooks = cfg.Hooks
		}
	} else if len(os.Args) > 2 {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Connect MCP servers.
	var startup []string
	mcpExec := tools.NewExecutor()
	if cfg != nil {
		for name, serverCfg := range cfg.MCPServers {
			if serverCfg.Disabled || serverCfg.Command == "" {
				continue
			}
			transport := mcp.NewSTDIOTransport(serverCfg.Command, serverCfg.Args, serverCfg.Env)
			client, err := transport.Connect()
			if err != nil {
				startup = append(startup, fmt.Sprintf("MCP %s: failed to connect: %v", name, err))
				continue
			}
			mcpExec.AddServer(name, client)
			startup = append(startup, fmt.Sprintf("MCP %s: connected", name))
		}
	}

	mcpTools, err := mcpExec.ListTools()
	if err != nil {
		log.Fatalf("Failed to list MCP tools: %v", err)
	}
	if len(mcpTools) > 0 {
		startup = append(startup, fmt.Sprintf("MCP tools loaded: %d", len(mcpTools)))
	}

	be, err := backend.New()
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}

	home, _ := os.UserHomeDir()
	builtinExec := execpkg.New(
		home+"/.local/state/ollie",
		home+"/.cache/ollie/exec",
	)

	allTools := append(mcpToolsToBackend(mcpTools), executeCodeTool)
	serverOf := make(map[string]string, len(mcpTools))
	for _, t := range mcpTools {
		serverOf[t.Name] = t.Server
	}

	loopcfg := agent.Config{
		Backend:      be,
		Model:        modelName,
		SystemPrompt: systemPrompt,
		Tools:        allTools,
		Exec: func(name string, args json.RawMessage) (string, error) {
			if server, ok := serverOf[name]; ok {
				raw, err := mcpExec.Execute(server, name, args)
				if err != nil {
					return "", err
				}
				return extractMCPText(raw), nil
			}
			if name == "execute_code" {
				return dispatchBuiltinExec(builtinExec, args)
			}
			return "", fmt.Errorf("unknown tool: %s", name)
		},
		MaxSteps: 20,
	}

	ta := textarea.New()
	ta.Placeholder = "Type your message..."
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(5)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"))
	ta.Focus()

	vp := viewport.New(0, 0)
	vp.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		Down:         key.NewBinding(key.WithKeys()),
		Up:           key.NewBinding(key.WithKeys()),
	}

	p := tea.NewProgram(model{
		textarea: ta,
		viewport: vp,
		loopcfg:  loopcfg,
		hooks:    hooks,
		display:  startup,
	})

	if hook := hooks["agentSpawn"]; hook != "" {
		exec.Command("sh", "-c", hook).Run()
	}

	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}

// -- tea model --

func (m model) Init() tea.Cmd {
	return textarea.Blink
}

type responseMsg struct {
	display []string
	session *agent.Session
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 5
		m.textarea.SetWidth(msg.Width)
		m.viewport.SetContent(m.renderDisplay())
		m.ready = true

	case responseMsg:
		m.display = msg.display
		m.session = msg.session
		m.viewport.SetContent(m.renderDisplay())
		m.viewport.GotoBottom()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}
			m.display = append(m.display, "You: "+input)
			m.viewport.SetContent(m.renderDisplay())
			m.viewport.GotoBottom()
			m.textarea.Reset()

			if hook := m.hooks["userPromptSubmit"]; hook != "" {
				exec.Command("sh", "-c", hook).Run()
			}

			return m, m.runLoop(input)
		}
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	m.textarea, cmd = m.textarea.Update(msg)
	return m, tea.Batch(cmd, vpCmd)
}

func (m model) runLoop(input string) tea.Cmd {
	loopcfg := m.loopcfg
	session := m.session
	hooks := m.hooks
	display := append([]string{}, m.display...)

	return func() tea.Msg {
		if session == nil {
			session = agent.NewSession(input)
		} else {
			session.AppendUserMessage(input)
		}

		var lines []string
		loopcfg.Output = func(msg agent.OutputMsg) {
			switch msg.Role {
			case "assistant":
				lines = append(lines, "Bot: "+msg.Content)
			case "call":
				lines = append(lines, fmt.Sprintf("→ %s(%s)", msg.Name, msg.Content))
			case "tool":
				lines = append(lines, fmt.Sprintf("  = %s", msg.Content))
			case "usage":
				lines = append(lines, "["+msg.Content+"]")
			case "error":
				lines = append(lines, "Error: "+msg.Content)
			}
		}

		if err := agent.New(loopcfg).Run(context.Background(), session); err != nil {
			display = append(display, "Error: "+err.Error())
		} else {
			display = append(display, lines...)
		}

		if hook := hooks["stop"]; hook != "" {
			exec.Command("sh", "-c", hook).Run()
		}

		return responseMsg{display: display, session: session}
	}
}

func (m model) View() string {
	if !m.ready {
		return "Loading..."
	}
	return m.viewport.View() + "\n" + m.textarea.View()
}

// -- display helpers --

// renderDisplay word-wraps each display line to the viewport width and joins them.
func (m model) renderDisplay() string {
	w := m.viewport.Width
	var buf strings.Builder
	for i, line := range m.display {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(wordWrap(line, w))
	}
	return buf.String()
}

// wordWrap wraps s at word boundaries so no line exceeds width columns.
func wordWrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, raw := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		col := 0
		first := true
		for _, word := range strings.Fields(raw) {
			wl := len(word)
			switch {
			case first:
				out.WriteString(word)
				col = wl
				first = false
			case col+1+wl > width:
				out.WriteByte('\n')
				out.WriteString(word)
				col = wl
			default:
				out.WriteByte(' ')
				out.WriteString(word)
				col += 1 + wl
			}
		}
	}
	return out.String()
}

// -- tool helpers --

// mcpToolsToBackend converts MCP tool descriptors to backend.Tool entries.
func mcpToolsToBackend(mcpTools []tools.ToolInfo) []backend.Tool {
	out := make([]backend.Tool, len(mcpTools))
	for i, t := range mcpTools {
		out[i] = backend.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return out
}

// extractMCPText unwraps {"content":[{"type":"text","text":"..."}]}.
func extractMCPText(raw json.RawMessage) string {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw)
	}
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// dispatchBuiltinExec handles execute_code natively.
func dispatchBuiltinExec(e *execpkg.Executor, args json.RawMessage) (string, error) {
	var a struct {
		Code     string             `json:"code"`
		Language string             `json:"language"`
		Timeout  int                `json:"timeout"`
		Sandbox  string             `json:"sandbox"`
		Tool     string             `json:"tool"`
		Args     []string           `json:"args"`
		Pipe     []execpkg.PipeStep `json:"pipe"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}

	code := a.Code
	trusted := false

	switch {
	case len(a.Pipe) > 0:
		var err error
		code, trusted, err = execpkg.BuildPipeline(a.Pipe)
		if err != nil {
			return "", err
		}
	case a.Tool != "":
		toolCode, err := execpkg.ReadTool(a.Tool)
		if err != nil {
			return "", err
		}
		code = toolCode
		trusted = true
		if len(a.Args) > 0 {
			var escaped []string
			for _, arg := range a.Args {
				escaped = append(escaped, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
			}
			code = fmt.Sprintf("set -- %s\n%s", strings.Join(escaped, " "), code)
		}
	}

	if code == "" {
		return "", fmt.Errorf("execute_code: one of 'code', 'tool', or 'pipe' is required")
	}
	if a.Language == "" {
		a.Language = "bash"
	}
	if a.Timeout <= 0 {
		a.Timeout = 30
	}
	if a.Sandbox == "" {
		a.Sandbox = "default"
	}

	return e.Execute(code, a.Language, a.Timeout, a.Sandbox, trusted)
}
