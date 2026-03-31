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
	if len(os.Args) < 2 {
		log.Fatal("Usage: ollie <config.json> [model]")
	}

	modelName := os.Getenv("OLLIE_MODEL")
	if modelName == "" && len(os.Args) > 2 {
		modelName = os.Args[2]
	}
	if modelName == "" {
		modelName = "qwen3:8b"
	}

	cfg, err := config.Load(os.Args[1])
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Connect MCP servers.
	mcpExecutor := tools.NewExecutor()
	for name, serverCfg := range cfg.MCPServers {
		if serverCfg.Disabled || serverCfg.Command == "" {
			continue
		}
		transport := mcp.NewSTDIOTransport(serverCfg.Command, serverCfg.Args, serverCfg.Env)
		client, err := transport.Connect()
		if err != nil {
			log.Printf("Failed to connect to %s: %v", name, err)
			continue
		}
		mcpExecutor.AddServer(name, client)
		log.Printf("Connected to MCP server: %s", name)
	}

	mcpTools, err := mcpExecutor.ListTools()
	if err != nil {
		log.Fatalf("Failed to list tools: %v", err)
	}
	log.Printf("Loaded %d tools", len(mcpTools))

	be, err := backend.New()
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}

	home, _ := os.UserHomeDir()
	builtinExec := execpkg.New(
		home+"/.local/state/ollie",
		home+"/.cache/ollie/exec",
	)

	loopcfg := agent.Config{
		Backend:  be,
		Model:    modelName,
		Tools:    mcpToolsToBackend(mcpTools),
		Exec:     buildDispatch(builtinExec, mcpExecutor, mcpTools),
		MaxSteps: 20,
	}

	hooks := cfg.Hooks
	if hooks == nil {
		hooks = make(map[string]string)
	}

	ta := textarea.New()
	ta.Placeholder = "Type your message..."
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(5)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"))
	ta.Focus()

	p := tea.NewProgram(model{
		textarea: ta,
		loopcfg:  loopcfg,
		hooks:    hooks,
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
		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-5)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - 5
		}
		m.textarea.SetWidth(msg.Width)

	case responseMsg:
		m.display = msg.display
		m.session = msg.session
		m.viewport.SetContent(strings.Join(m.display, "\n"))
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
			m.viewport.SetContent(strings.Join(m.display, "\n"))
			m.viewport.GotoBottom()
			m.textarea.Reset()

			if hook := m.hooks["userPromptSubmit"]; hook != "" {
				exec.Command("sh", "-c", hook).Run()
			}

			return m, m.runLoop(input)
		}
	}

	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m model) runLoop(input string) tea.Cmd {
	loopcfg := m.loopcfg
	session := m.session
	hooks := m.hooks
	display := append([]string{}, m.display...)

	return func() tea.Msg {
		// First message: create a new session with the input as goal.
		// Subsequent messages: append to existing session.
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
			case "tool":
				lines = append(lines, fmt.Sprintf("→ %s: %s", msg.Name, msg.Content))
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

// -- helpers --

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

// buildDispatch routes tool calls to the MCP server that owns them, falling
// back to the built-in execute_code sandbox if not found via MCP.
func buildDispatch(builtin *execpkg.Executor, mcpExec *tools.Executor, mcpTools []tools.ToolInfo) agent.ToolExecutor {
	serverOf := make(map[string]string, len(mcpTools))
	for _, t := range mcpTools {
		serverOf[t.Name] = t.Server
	}

	return func(name string, args json.RawMessage) (string, error) {
		if server, ok := serverOf[name]; ok {
			raw, err := mcpExec.Execute(server, name, args)
			if err != nil {
				return "", err
			}
			return extractMCPText(raw), nil
		}
		if name == "execute_code" {
			return dispatchBuiltinExec(builtin, args)
		}
		return "", fmt.Errorf("unknown tool: %s", name)
	}
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

// dispatchBuiltinExec handles execute_code natively without MCP.
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
