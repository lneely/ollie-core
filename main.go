package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ollie/agent"
	"ollie/backend"
	"ollie/config"
	execpkg "ollie/exec"
	"ollie/mcp"
	"ollie/tools"
)

const systemPrompt = `You are an autonomous agent. You have one tool: execute_code.

## Output protocol

Be terse. No preamble. No narration. No post-action summaries.
Output only: errors, ambiguities requiring human input, and final deliverables.
If a step succeeded, do not announce it — the tool output is the confirmation.
Do not output filler phrases ("Let me...", "I'll now...", "Great question!").
Do not restate the task. Do not hedge. Do not self-congratulate.

## execute_code — when to use it

Use execute_code whenever the task requires:
- running shell commands or scripts
- reading or writing files
- making network requests
- any information you cannot reliably state from memory

Do not answer from memory when you can verify with execute_code.

Do not describe what you would run or show code blocks.
Call execute_code directly.
If you find yourself writing a markdown code block, stop and make the tool call instead.

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

var executeCodeTool = backend.Tool{
	Name:        "execute_code",
	Description: "Execute shell code or a named tool script in a sandboxed environment. Use 'code' for inline bash, 'tool'+'args' for a named script, or 'pipe' for a sequence of {tool, args} steps.",
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

// agentState represents what the agent is currently doing.
type agentState int

const (
	agentIdle agentState = iota
	agentThinking
	agentRunningTool
	agentRetrying
)

// resolveBackendName returns a short human-readable backend label derived
// from OLLIE_BACKEND and (for openai-compatible backends) OLLIE_OPENAI_URL.
// Known provider hostnames are mapped to friendly names so that e.g.
// openrouter.ai shows up as "openrouter" rather than "openai".
func resolveBackendName() string {
	which := os.Getenv("OLLIE_BACKEND")
	if which == "" {
		which = "ollama"
	}
	if which != "openai" {
		return which
	}
	// For openai-compatible backends, try to identify the provider from the URL.
	url := strings.ToLower(os.Getenv("OLLIE_OPENAI_URL"))
	switch {
	case strings.Contains(url, "openrouter"):
		return "openrouter"
	case strings.Contains(url, "together"):
		return "together"
	case strings.Contains(url, "groq"):
		return "groq"
	case strings.Contains(url, "mistral"):
		return "mistral"
	case strings.Contains(url, "anthropic"):
		return "anthropic"
	case strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1"):
		return "local"
	case url == "":
		return "openai"
	default:
		return "openai"
	}
}

// NOTE: buf and display lines use plain strings; bubbletea copies the model
// by value on every Update(), so strings.Builder and other copy-sensitive
// types will panic.
type model struct {
	textarea    textarea.Model
	viewport    viewport.Model
	session     *agent.Session
	loopcfg     agent.Config
	display     []string
	buf         string // live streaming assistant text
	hooks       map[string]string
	ready       bool
	agentCh     chan tea.Msg
	cancel      context.CancelFunc
	doneCh      chan struct{}
	modelName   string
	backendName string // e.g. "ollama", "openrouter", "openai"
	ctxOverhead int    // fixed per-request char overhead (system prompt + tool schemas)

	quitPending bool     // whether a second Ctrl+C should quit
	lastCtrlC   time.Time // timestamp of last Ctrl+C press
	// status bar state
	state         agentState
	currentTool   string
	retrySecsLeft int
	lastUsage     backend.Usage
	ctxStats      agent.ContextStats
}

type agentMsg struct {
	role     string
	content  string
	name     string
	done     bool
	usage    backend.Usage
	ctxStats agent.ContextStats
}

// statusBarStyle is a full-width reversed bar.
var statusBarStyle = lipgloss.NewStyle().
	Reverse(true).
	Padding(0, 1)

// timeoutMsg is sent when the Ctrl+C double-press window expires
type timeoutMsg struct{}

func main() {
	var startup []string
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
	} else {
		startup = append(startup, fmt.Sprintf("config: %v", err))
	}

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

	modelName := os.Getenv("OLLIE_MODEL")
	if modelName == "" && len(os.Args) > 1 {
		modelName = os.Args[1]
	}
	if modelName == "" {
		modelName = "qwen3:8b"
	}

	// Resolve after backend.New() so that loadEnvFile has already run and
	// populated OLLIE_BACKEND / OLLIE_OPENAI_URL from ~/.config/ollie/env.
	backendName := resolveBackendName()

	home, _ := os.UserHomeDir()
	builtinExec := execpkg.New(
		home+"/.local/state/ollie",
		home+"/.cache/ollie/exec",
	)

	allTools := append(mcpToolsToBackend(mcpTools), executeCodeTool)

	// Compute fixed per-request overhead: system prompt + all tool schemas.
	// This is subtracted from the context budget so limits mean what they say.
	ctxOverhead := len(systemPrompt)
	for _, t := range allTools {
		ctxOverhead += len(t.Name) + len(t.Description) + len(t.Parameters)
	}

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
		HalfPageDown: key.NewBinding(key.WithKeys("alt+d")),
		HalfPageUp:   key.NewBinding(key.WithKeys("alt+u")),
		Down:         key.NewBinding(key.WithKeys("down", "alt+n")),
		Up:           key.NewBinding(key.WithKeys("up", "alt+p")),
	}

	p := tea.NewProgram(model{
		textarea:    ta,
		viewport:    vp,
		loopcfg:     loopcfg,
		hooks:       hooks,
		display:     startup,
		modelName:   modelName,
		backendName: backendName,
		ctxOverhead: ctxOverhead,
		quitPending: false,
		state:       agentIdle,
	})

	if hook := hooks["agentSpawn"]; hook != "" {
		exec.Command("sh", "-c", hook).Run()
	}

	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}

func (m model) Init() tea.Cmd {
	return textarea.Blink
}

// renderDisplay produces the full viewport text.
func (m model) renderDisplay() string {
	var b strings.Builder
	for i, line := range m.display {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	if m.buf != "" {
		if len(m.display) > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("Bot: " + m.buf)
	}
	return b.String()
}

func (m *model) refreshView() {
	m.viewport.SetContent(wordWrap(m.renderDisplay(), m.viewport.Width))
	m.viewport.GotoBottom()
}

// renderStatusBar returns the one-line status bar string.
func (m model) renderStatusBar() string {
	// Agent state segment.
	var stateStr string
	switch m.state {
	case agentIdle:
		stateStr = "idle"
	case agentThinking:
		stateStr = "thinking\u2026"
	case agentRunningTool:
		stateStr = "tool: " + m.currentTool
	case agentRetrying:
		stateStr = fmt.Sprintf("retry %ds", m.retrySecsLeft)
	}

	// Token usage segment.
	usageStr := "last: -"
	if m.lastUsage.InputTokens > 0 || m.lastUsage.OutputTokens > 0 {
		usageStr = fmt.Sprintf("last: \u2191%d \u2193%d", m.lastUsage.InputTokens, m.lastUsage.OutputTokens)
	}

	// Context stats segment.
	ctxStr := "ctx: -"
	if m.ctxStats.StoredMessages > 0 {
		ktok := float64(m.ctxStats.ApproxTokens) / 1000.0
		ctxStr = fmt.Sprintf("ctx: ~%.1fk tokens (%d msgs)", ktok, m.ctxStats.BoundedMessages)
		if m.ctxStats.Evicted > 0 {
			ctxStr += fmt.Sprintf(", %d evicted", m.ctxStats.Evicted)
		}
	}

	bar := fmt.Sprintf("[%s :: %s] %s | %s | %s",
		m.backendName, m.modelName, stateStr, usageStr, ctxStr)
	return statusBarStyle.Width(m.viewport.Width).Render(bar)
}

// finalizeBuf commits the in-progress streaming text as a new display line.

// addStreamingContent concatenates a streaming chunk onto the buffer,
// inserting a single space when neither side has whitespace at the boundary.
//
// Some backends (e.g. DeepInfra) occasionally drop whitespace-only tokens
// (emitting null or "" for a space token), causing words to fuse.  Since
// LLM APIs always deliver complete BPE tokens — which never split mid-word —
// inserting a space whenever two non-space runes meet is safe in practice.
func addStreamingContent(buf, chunk string) string {
	if buf == "" || chunk == "" {
		return buf + chunk
	}
	last, _ := utf8.DecodeLastRuneInString(buf)
	first, _ := utf8.DecodeRuneInString(chunk)
	if !unicode.IsSpace(last) && !unicode.IsSpace(first) {
		return buf + " " + chunk
	}
	return buf + chunk
}

func (m *model) finalizeBuf() {
	if m.buf != "" {
		m.display = append(m.display, "Bot: "+m.buf)
		m.buf = ""
	}
}

// apply writes one agent output event into the model.
func (m *model) apply(am agentMsg) {
	switch am.role {
	case "assistant":
		m.state = agentThinking
		m.buf = addStreamingContent(m.buf, am.content)

	case "call":
		m.state = agentRunningTool
		m.currentTool = am.name
		m.finalizeBuf()
		args := squashWhitespace(am.content)
		if len(args) > 500 {
			args = args[:500] + "..."
		}
		m.display = append(m.display, fmt.Sprintf("-> %s(%s)", am.name, args))

	case "tool":
		m.state = agentThinking
		s := squashWhitespace(am.content)
		if len(s) > 500 {
			s = s[:500] + "..."
		}
		m.display = append(m.display, "= "+s)

	case "nudge":
		// Model narrated without acting; loop injected a continuation prompt.
		m.display = append(m.display, "[nudge: continuing…]")

	case "retry":
		m.state = agentRetrying
		if secs, err := strconv.Atoi(am.content); err == nil {
			m.retrySecsLeft = secs
		}

	case "error":
		m.finalizeBuf()
		m.display = append(m.display, "Error: "+am.content)

	case "usage":
		// Update status bar only; no display line appended.
		m.lastUsage = am.usage
		m.ctxStats = am.ctxStats
	}
}

// drainAgent cancels the in-flight goroutine and drains remaining events.
func (m *model) drainAgent() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	m.cancel = nil
	if m.doneCh != nil {
		<-m.doneCh
		m.doneCh = nil
	}
	if m.agentCh != nil {
		for msg := range m.agentCh {
			if am, ok := msg.(agentMsg); ok {
				m.apply(am)
			}
		}
		m.agentCh = nil
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Reserve 1 row for the status bar, 5 for the textarea.
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 5 - 1
		m.textarea.SetWidth(msg.Width)
		m.refreshView()
		m.ready = true

	case timeoutMsg:
		// Clear quitPending flag when timeout expires
		m.quitPending = false
		return m, nil


	case agentMsg:
		m.apply(msg)
		m.refreshView()
		if msg.done {
			m.state = agentIdle
			m.currentTool = ""
			m.agentCh = nil
			return m, nil
		}
		return m, func() tea.Msg { return <-m.agentCh }

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			// ESC always interrupts if agent is running
			if m.cancel != nil {
				m.drainAgent()
			}
			m.quitPending = false
			return m, nil

		case "ctrl+c":
			now := time.Now()
			
			// Handle double-press to quit
			if m.quitPending && now.Sub(m.lastCtrlC) <= 1*time.Second {
				// Double Ctrl+C within 1 second: quit
				return m, tea.Quit
			}

			// First Ctrl+C: interrupt if agent running
			if m.cancel != nil {
				m.drainAgent()
				m.quitPending = false
				return m, nil
			}

			// No agent running, track for potential double-press
			if m.lastCtrlC.IsZero() {
				// First Ctrl+C with no agent
				m.lastCtrlC = now
				m.quitPending = true
				return m, tea.Cmd(func() tea.Msg {
					time.Sleep(1 * time.Second)
					return timeoutMsg{}
				})
			} else if now.Sub(m.lastCtrlC) <= 1*time.Second {
				// Double Ctrl+C within 1 second with no agent: quit
				return m, tea.Quit
			} else {
				// Ctrl+C after timeout, reset and start new timer
				m.lastCtrlC = now
				m.quitPending = true
				return m, tea.Cmd(func() tea.Msg {
					time.Sleep(1 * time.Second)
					return timeoutMsg{}
				})
			}

		case "enter":
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}
			m.drainAgent()
			m.finalizeBuf()
			m.display = append(m.display, "You: "+input)
			m.refreshView()
			m.textarea.Reset()

			if hook := m.hooks["userPromptSubmit"]; hook != "" {
				exec.Command("sh", "-c", hook).Run()
			}

			if m.handleCommand(input) {
				m.refreshView()
				return m, nil
			}

			if m.session == nil {
				m.session = agent.NewSessionWithConfig(input, agent.ContextConfig{
					FixedOverheadChars: m.ctxOverhead,
				})
			} else {
				m.session.AppendUserMessage(input)
			}
			m.state = agentThinking
			ch, cancel, doneCh := m.startAgent(m.session)
			m.agentCh = ch
			m.cancel = cancel
			m.doneCh = doneCh
			return m, func() tea.Msg { return <-ch }
		}
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	m.textarea, cmd = m.textarea.Update(msg)
	return m, tea.Batch(cmd, vpCmd)
}

// handleCommand processes special slash commands and returns true if handled.
func (m *model) handleCommand(input string) bool {
	if !strings.HasPrefix(input, "/") {
		return false
	}

	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}

	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/backend":
		if len(args) == 0 {
			m.display = append(m.display, "Error: /backend requires an argument (e.g., /backend ollama)")
			return true
		}
		backendType := args[0]
		os.Setenv("OLLIE_BACKEND", backendType)
		be, err := backend.New()
		if err != nil {
			m.display = append(m.display, fmt.Sprintf("Error: Failed to switch backend: %v", err))
			return true
		}
		m.loopcfg.Backend = be
		m.backendName = resolveBackendName()
		m.display = append(m.display, fmt.Sprintf("Switched backend to: %s", m.backendName))
		return true

	case "/model":
		if len(args) == 0 {
			m.display = append(m.display, "Error: /model requires an argument (e.g., /model qwen3:8b)")
			return true
		}
		m.loopcfg.Model = args[0]
		m.modelName = args[0]
		m.display = append(m.display, fmt.Sprintf("Switched model to: %s", args[0]))
		return true

	case "/help":
		m.display = append(m.display, "Available commands:")
		m.display = append(m.display, "  /backend <type>  - Switch backend (ollama, openai)")
		m.display = append(m.display, "  /model <name>    - Switch model")
		m.display = append(m.display, "  /help            - Show this help")
		return true
	}

	return false
}

// startAgent launches the loop in a goroutine, wiring its output to ch.
func (m model) startAgent(session *agent.Session) (chan tea.Msg, context.CancelFunc, chan struct{}) {
	loopcfg := m.loopcfg
	hooks := m.hooks
	ch := make(chan tea.Msg, 64)
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan struct{})

	go func() {
		defer close(ch)
		defer close(doneCh)

		loopcfg.Output = func(em agent.OutputMsg) {
			var msg agentMsg
			msg.role = em.Role
			msg.content = em.Content
			msg.name = em.Name

			if em.Role == "usage" {
				msg.usage = em.Usage
				msg.ctxStats = session.ContextStats()
			}

			select {
			case ch <- msg:
			case <-ctx.Done():
				return
			}
		}

		if err := agent.Run(ctx, loopcfg, session); err != nil {
			select {
			case ch <- agentMsg{role: "error", content: err.Error()}:
			case <-ctx.Done():
				return
			}
		}

		if hook := hooks["stop"]; hook != "" {
			exec.Command("sh", "-c", hook).Run()
		}

		select {
		case ch <- agentMsg{done: true}:
		case <-ctx.Done():
		}
	}()

	return ch, cancel, doneCh
}

func (m model) View() string {
	if !m.ready {
		return "Loading..."
	}
	return m.viewport.View() + "\n" + m.renderStatusBar() + "\n" + m.textarea.View()
}

// compactJSON removes whitespace between JSON tokens.
func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return squashWhitespace(s)
	}
	return squashWhitespace(buf.String())
}

// squashWhitespace collapses all runs of whitespace into single spaces.
func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// wordWrap wraps text at word boundaries so no line exceeds width.
func wordWrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		col, first := 0, true
		for _, word := range strings.Fields(line) {
			wl := len(word)
			switch {
			case first:
				out.WriteString(word)
				col = wl
				first = false
			case col+wl+1 > width:
				out.WriteByte('\n')
				out.WriteString(word)
				col = wl
				first = true
			default:
				out.WriteByte(' ')
				out.WriteString(word)
				col += wl + 1
			}
		}
	}
	return out.String()
}

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
