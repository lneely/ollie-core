package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

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

const systemPromptBase = `Use the fewest words possible. No preamble, filler, or narration ("Let me...", "I'll now...", "Great!"). No explanations of actions taken. No summaries of completed work. No reasoning unless asked. If the answer is one word, write one word.
Call tools immediately and directly. Never describe what you are about to do — act.
Complete tasks fully before stopping. Do not pause mid-task to narrate progress or request confirmation.
Do not ask clarifying questions unless the task is genuinely ambiguous. Attempt the task; correct based on feedback.
Do not restate tasks, hedge, or self-congratulate.
Do not attempt tasks outside your tools.
Do not use hedging language ("it looks like", "it appears", "it seems", "likely", "probably"). If you are uncertain, use tools to find out. Give definite answers based on evidence.
Do not re-read or re-fetch any file or resource that already has a result in the conversation history. Use the existing result.
Use tools to gather information before asking the user for clarification. Explore files, run commands, and investigate the environment first. Only ask when you have exhausted what you can discover on your own.
Use execute_code for all shell commands and scripts. Use execute_tool only for named scripts in ~/mnt/anvillm/tools. Use execute_pipe to chain steps: use {code: "cmd --flags"} for shell commands, {tool, args} only for named scripts in ~/mnt/anvillm/tools.
Use file_read and file_write for all file read and write operations. Never use shell commands to read or write files.

Tool call examples:
  Read a file:      {"path": "/home/user/foo.go"}
  Read lines 10-20: {"path": "/home/user/foo.go", "start_line": 10, "end_line": 20}
  Run shell code:   {"code": "ls -la", "language": "bash"}
  List directory:   {"code": "find . -maxdepth 2 -type f", "language": "bash"}
  Run named tool:   {"tool": "discover_skill.sh", "args": ["keyword"]}
  Pipeline:         {"pipe": [{"code": "cat file.txt"}, {"code": "grep foo"}]}`

func systemPrompt(allTools []backend.Tool) string {
	cwd, _ := os.Getwd()
	now := time.Now().Format("2006-01-02 15:04:05 MST")
	names := make([]string, len(allTools))
	for i, t := range allTools {
		names[i] = t.Name
	}
	s := systemPromptBase + "\n\nWorking directory: " + cwd +
		"\nCurrent time: " + now +
		"\nAvailable tools: " + strings.Join(names, ", ")
	return s
}

var builtinTools = []backend.Tool{
	{
		Name:        "execute_code",
		Description: "Run inline code in a sandboxed environment.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["code"],
			"properties": {
				"code":     {"type": "string",  "description": "Code to execute."},
				"language": {"type": "string",  "description": "Language interpreter (default: bash)."},
				"timeout":  {"type": "integer", "description": "Timeout in seconds (default: 30)."},
				"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."}
			}
		}`),
	},
	{
		Name:        "execute_tool",
		Description: "Run a named tool script from ~/mnt/anvillm/tools in a sandboxed environment. Use this only for named scripts, not for inline shell commands.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["tool"],
			"properties": {
				"tool":     {"type": "string", "description": "Name of the tool script (e.g. discover_skill.sh)."},
				"args":     {"type": "array",  "items": {"type": "string"}, "description": "Arguments for the tool script."},
				"timeout":  {"type": "integer", "description": "Timeout in seconds (default: 30)."},
				"sandbox":  {"type": "string",  "description": "Sandbox name (default: default)."}
			}
		}`),
	},
	{
		Name:        "execute_pipe",
		Description: "Run a pipeline of steps, piping stdout of each into stdin of the next. Use {code: \"cmd --flags\"} for shell commands; use {tool, args} only for named scripts in ~/mnt/anvillm/tools.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["pipe"],
			"properties": {
				"pipe": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"tool": {"type": "string"},
							"args": {"type": "array", "items": {"type": "string"}},
							"code": {"type": "string"}
						}
					}
				},
				"timeout": {"type": "integer", "description": "Timeout in seconds (default: 30)."},
				"sandbox": {"type": "string",  "description": "Sandbox name (default: default)."}
			}
		}`),
	},
	{
		Name:        "file_read",
		Description: "Read a file or a range of lines. Output includes line numbers. Always use this instead of shell commands for reading files.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["path"],
			"properties": {
				"path":       {"type": "string",  "description": "Path to the file."},
				"start_line": {"type": "integer", "description": "First line to read, 1-based (default: 1)."},
				"end_line":   {"type": "integer", "description": "Last line to read, inclusive (default: EOF)."}
			}
		}`),
	},
	{
		Name:        "file_write",
		Description: "Write content to a file. Omit start_line/end_line to overwrite the whole file. Provide both to replace only that line range. Always use file_read or grep -n to identify the exact line range before writing. Never guess line numbers. Preserve original formatting and indentation. Always use this instead of shell commands for writing files.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["path", "content"],
			"properties": {
				"path":       {"type": "string",  "description": "Path to the file."},
				"content":    {"type": "string",  "description": "Content to write."},
				"start_line": {"type": "integer", "description": "First line of range to replace, 1-based."},
				"end_line":   {"type": "integer", "description": "Last line of range to replace, inclusive."}
			}
		}`),
	},
}

// agentState represents what the agent is currently doing.
type agentState int

const (
	agentIdle agentState = iota
	agentThinking
	agentRunningTool
	agentRetrying
	agentConfirming
	agentStalled
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
	agentName   string // active agent config name, e.g. "default"
	agentsDir   string // path to ~/.config/ollie/agents/
	mcpExec          *tools.Executor   // current agent's MCP executor; closed on agent switch
	builtinExec      *execpkg.Executor
	confirmPtr       *agent.ConfirmFn // indirection so startAgent can set it per-run
	ctxOverhead      int              // fixed per-request char overhead (system prompt + tool schemas)
	invalidateCaches func()           // clears tool-call dedup caches; set from agentEnv

	quitPending bool     // whether a second Ctrl+C should quit
	lastCtrlC   time.Time // timestamp of last Ctrl+C press
	// status bar state
	state         agentState
	currentTool   string
	retrySecsLeft int
	lastUsage     backend.Usage
	ctxStats      agent.ContextStats
	confirmCh     chan bool // non-nil when waiting for user confirmation
}

type agentMsg struct {
	role      string
	content   string
	name      string
	done      bool
	usage     backend.Usage
	ctxStats  agent.ContextStats
	confirmCh chan bool // set when role=="confirm"
}

type confirmMsg struct {
	approved bool
}

// statusBarStyle is a full-width reversed bar.
var statusBarStyle = lipgloss.NewStyle().
	Reverse(true).
	Padding(0, 1)

// timeoutMsg is sent when the Ctrl+C double-press window expires
type timeoutMsg struct{}

// agentEnv holds the runtime state derived from an agent config file.
type agentEnv struct {
	mcpExec          *tools.Executor  // kept so it can be closed on agent switch
	tools            []backend.Tool
	exec             agent.ToolExecutor
	confirm          *agent.ConfirmFn // pointer filled in by startAgent per-run
	hooks            map[string]string
	systemPrompt     string
	genParams        backend.GenerationParams
	ctxOverhead      int
	messages         []string // startup / status messages to display
	invalidateCaches func()   // clears per-session tool-call dedup caches on compact/clear
}

// buildAgentEnv constructs the runtime environment for a given agent config.
// cfg may be nil (no agent file found), in which case only the builtin tool
// is available and no hooks or agent prompt are set.
func buildAgentEnv(cfg *config.Config, builtinExec *execpkg.Executor) agentEnv {
	var messages []string
	mcpExec := tools.NewExecutor()

	if cfg != nil {
		for name, serverCfg := range cfg.MCPServers {
			if serverCfg.Disabled || serverCfg.Command == "" {
				continue
			}
			transport := mcp.NewSTDIOTransport(serverCfg.Command, serverCfg.Args, serverCfg.Env)
			client, err := transport.Connect()
			if err != nil {
				messages = append(messages, fmt.Sprintf("MCP %s: failed to connect: %v", name, err))
				continue
			}
			mcpExec.AddServer(name, client)
			messages = append(messages, fmt.Sprintf("MCP %s: connected", name))
		}
	}

	mcpTools, err := mcpExec.ListTools()
	if err != nil {
		messages = append(messages, fmt.Sprintf("MCP list tools: %v", err))
	} else if len(mcpTools) > 0 {
		messages = append(messages, fmt.Sprintf("MCP tools loaded: %d", len(mcpTools)))
	}

	serverOf := make(map[string]string, len(mcpTools))
	for _, t := range mcpTools {
		serverOf[t.Name] = t.Server
	}

	allTools := append(mcpToolsToBackend(mcpTools), builtinTools...)

	hooks := map[string]string{}
	agentPrompt := ""
	trustedTools := map[string]struct{}{}
	var genParams backend.GenerationParams
	if cfg != nil {
		if cfg.Hooks != nil {
			hooks = cfg.Hooks
		}
		agentPrompt = cfg.Prompt
		for _, t := range cfg.TrustedTools {
			trustedTools[t] = struct{}{}
		}
		genParams = backend.GenerationParams{
			MaxTokens:        cfg.MaxTokens,
			Temperature:      cfg.Temperature,
			FrequencyPenalty: cfg.FrequencyPenalty,
			PresencePenalty:  cfg.PresencePenalty,
		}
	}

	sp := systemPrompt(allTools)
	if agentPrompt != "" {
		sp = systemPrompt(allTools) + "\n\n" + agentPrompt
	}

	overhead := len(sp)
	for _, t := range allTools {
		overhead += len(t.Name) + len(t.Description) + len(t.Parameters)
	}

	builtinNames := make(map[string]struct{}, len(builtinTools))
	for _, t := range builtinTools {
		builtinNames[t.Name] = struct{}{}
	}

	var confirmFn agent.ConfirmFn // filled in by startAgent
	confirmPtr := &confirmFn

	// fileRanges tracks which line ranges of each file have been read this session.
	// Used to warn on overlapping re-reads and to guard file_write against blind overwrites.
	// Cleared on /compact and /clear; individual entries deleted when a file is written.
	type fileReadState struct {
		ranges     []lineRange
		totalLines int // total lines when last read; used for whole-file write coverage check
	}
	fileRanges := make(map[string]*fileReadState)

	// toolCallSeen tracks all non-file tool calls by exact (name, args) to warn on repeats.
	toolCallSeen := make(map[string]bool)

	rawExec := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		if server, ok := serverOf[name]; ok {
			raw, err := mcpExec.Execute(server, name, args)
			if err != nil {
				return "", err
			}
			return extractMCPText(raw), nil
		}
		if _, ok := builtinNames[name]; ok {
			var cfn agent.ConfirmFn
			if _, trusted := trustedTools[name]; !trusted {
				cfn = *confirmPtr
			}

			if name == "file_read" {
				var a struct {
					Path string `json:"path"`
				}
				json.Unmarshal(args, &a) //nolint:errcheck
				meta, err := dispatchFileRead(cfn, args)
				if err != nil {
					return "", err
				}
				content := meta.content
				if a.Path != "" {
					st := fileRanges[a.Path]
					if st != nil && rangesOverlap(st.ranges, meta.start, meta.end) {
						content = fmt.Sprintf("[WARNING: Lines %d-%d of this file were already read this session. Do not re-read ranges already in your context.]\n", meta.start, meta.end) + content
					}
					if st == nil {
						st = &fileReadState{}
						fileRanges[a.Path] = st
					}
					st.ranges = append(st.ranges, lineRange{meta.start, meta.end})
					st.totalLines = meta.totalLines
				}
				return content, nil
			}

			if name == "file_write" {
				var a struct {
					Path      string `json:"path"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
				}
				json.Unmarshal(args, &a) //nolint:errcheck
				if a.Path != "" {
					st := fileRanges[a.Path]
					if st == nil {
						return "", fmt.Errorf("file_write: %s has not been read this session; read it first to avoid overwriting unknown changes", a.Path)
					}
					ws, we := a.StartLine, a.EndLine
					if ws == 0 && we == 0 {
						// Whole-file write: verify full coverage.
						totalLines := st.totalLines
						if totalLines == 0 {
							data, err := os.ReadFile(a.Path)
							if err != nil {
								return "", fmt.Errorf("file_write: cannot verify read coverage: %w", err)
							}
							totalLines = len(strings.Split(string(data), "\n"))
						}
						ws, we = 1, totalLines
					}
					if !rangesCover(st.ranges, ws, we) {
						return "", fmt.Errorf("file_write: lines %d-%d of %s have not been read this session; read them first to avoid overwriting unknown changes", ws, we, a.Path)
					}
					// Invalidate: file content has changed, any cached ranges are stale.
					delete(fileRanges, a.Path)
				}
			}

			return dispatchBuiltinExec(ctx, name, builtinExec, cfn, args)
		}
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	execFn := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		result, err := rawExec(ctx, name, args)
		// Warn on repeated identical tool calls (excluding file_read/file_write which
		// are handled separately above).
		if err == nil && name != "file_read" && name != "file_write" {
			key := name + "\x00" + string(args)
			if toolCallSeen[key] {
				result = "[WARNING: This exact tool call was already made this session. Result may be unchanged. Do not repeat unless something has changed.]\n" + result
			}
			toolCallSeen[key] = true
		}
		return result, err
	}

	return agentEnv{
		mcpExec:      mcpExec,
		tools:        allTools,
		exec:         execFn,
		confirm:      confirmPtr,
		hooks:        hooks,
		systemPrompt: sp,
		genParams:    genParams,
		ctxOverhead:  overhead,
		messages:     messages,
		invalidateCaches: func() {
			clear(fileRanges)
			clear(toolCallSeen)
		},
	}
}

// agentConfigPath returns the path for a named agent config, falling back to
// the legacy config.json if the agents/ directory file does not exist.
func agentConfigPath(agentsDir, name string) string {
	p := agentsDir + "/" + name + ".json"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	// Legacy fallback: only for "default"
	if name == "default" {
		home, _ := os.UserHomeDir()
		return home + "/.config/ollie/config.json"
	}
	return p // will produce a load error, which is the right behaviour
}

func main() {
	home, _ := os.UserHomeDir()
	agentsDir := home + "/.config/ollie/agents"

	be, err := backend.New()
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}

	modelName := os.Getenv("OLLIE_MODEL")
	if modelName == "" {
		modelName = "qwen3:8b"
	}

	// Resolve after backend.New() so that loadEnvFile has already run and
	// populated OLLIE_BACKEND / OLLIE_OPENAI_URL from ~/.config/ollie/env.
	backendName := resolveBackendName()
	builtinExec := execpkg.New(
		home+"/.local/state/ollie",
		home+"/.cache/ollie/exec",
	)

	// Load the default agent config.
	agentName := os.Getenv("OLLIE_AGENT")
	if agentName == "" {
		agentName = "default"
	}
	if len(os.Args) > 1 {
		agentName = os.Args[1]
	}
	cfgPath := agentConfigPath(agentsDir, agentName)
	cfg, cfgErr := config.Load(cfgPath)

	var startup []string
	if cfgErr != nil && len(os.Args) > 2 {
		log.Fatalf("Failed to load agent config: %v", cfgErr)
	} else if cfgErr != nil {
		startup = append(startup, fmt.Sprintf("agent config: %v", cfgErr))
	}

	env := buildAgentEnv(cfg, builtinExec)
	startup = append(startup, env.messages...)

	loopcfg := agent.Config{
		Backend:          be,
		Model:            modelName,
		SystemPrompt:     env.systemPrompt,
		Tools:            env.tools,
		Exec:             env.exec,
		MaxSteps:         20,
		GenerationParams: env.genParams,
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
		textarea:         ta,
		viewport:         vp,
		loopcfg:          loopcfg,
		hooks:            env.hooks,
		display:          startup,
		modelName:        modelName,
		backendName:      backendName,
		agentName:        agentName,
		agentsDir:        agentsDir,
		mcpExec:          env.mcpExec,
		builtinExec:      builtinExec,
		confirmPtr:       env.confirm,
		ctxOverhead:      env.ctxOverhead,
		invalidateCaches: env.invalidateCaches,
		quitPending:      false,
		state:            agentIdle,
	})

	if hook := env.hooks["agentSpawn"]; hook != "" {
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
			b.WriteString("\n\n")
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
	case agentConfirming:
		stateStr = "confirm [y/n]"
	case agentStalled:
		stateStr = "stalled"
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

	bar := fmt.Sprintf("[%s :: %s :: %s] %s | %s | %s",
		m.backendName, m.modelName, m.agentName, stateStr, usageStr, ctxStr)
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
	return buf + chunk
}

func (m *model) appendDisplay(line string) {
	if len(m.display) > 0 {
		m.display = append(m.display, "")
	}
	m.display = append(m.display, line)
}

func (m *model) finalizeBuf() {
	if m.buf != "" {
		m.appendDisplay("Bot: " + m.buf)
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
		m.appendDisplay(fmt.Sprintf("-> %s(%s)", am.name, args))

	case "tool":
		m.state = agentThinking
		s := strings.TrimRight(am.content, "\n")
		if len(s) > 500 {
			s = s[:500] + "..."
		}
		m.appendDisplay("= " + s)

	case "retry":
		m.state = agentRetrying
		if secs, err := strconv.Atoi(am.content); err == nil {
			m.retrySecsLeft = secs
		}

	case "error":
		m.finalizeBuf()
		m.appendDisplay("Error: " + am.content)
		if m.session != nil {
			m.session.Rollback()
		}

	case "confirm":
		m.state = agentConfirming
		m.confirmCh = am.confirmCh
		m.appendDisplay("Confirm: " + am.content + " [y/n]")

	case "stalled":
		m.state = agentStalled

	case "usage":
		// Update status bar only; no display line appended.
		m.lastUsage = am.usage
		m.ctxStats = am.ctxStats
	}
}

// drainAgent cancels the in-flight goroutine and drains remaining events.
// If interrupted is true, rolls back any incomplete assistant turn.
func (m *model) drainAgent(interrupted bool) {
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
	if interrupted && m.session != nil {
		m.session.Rollback()
	}
	m.state = agentIdle
	m.currentTool = ""
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = msg.Width
		m.textarea.SetWidth(msg.Width)
		barHeight := lipgloss.Height(m.renderStatusBar())
		m.viewport.Height = msg.Height - m.textarea.Height() - barHeight
		m.refreshView()
		m.ready = true

	case timeoutMsg:
		// Clear quitPending flag when timeout expires
		m.quitPending = false
		return m, nil


	case confirmMsg:
		if m.confirmCh != nil {
			m.confirmCh <- msg.approved
			m.confirmCh = nil
		}
		m.state = agentRunningTool
		m.refreshView()
		if m.agentCh == nil {
			return m, nil
		}
		return m, func() tea.Msg { return <-m.agentCh }

	case agentMsg:
		m.apply(msg)
		m.refreshView()
		if msg.done {
			if m.state != agentStalled {
				m.state = agentIdle
			}
			m.currentTool = ""
			m.agentCh = nil
			m.cancel = nil
			m.doneCh = nil
			return m, nil
		}
		// Stop pumping while waiting for user confirmation.
		if m.state == agentConfirming {
			return m, nil
		}
		return m, func() tea.Msg { return <-m.agentCh }

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			// ESC always interrupts if agent is running
			if m.cancel != nil {
				m.drainAgent(true)
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
				m.drainAgent(true)
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

			// Handle confirmation prompt
			if m.state == agentConfirming && m.confirmCh != nil {
				m.textarea.Reset()
				lower := strings.ToLower(input)
				if lower == "y" || lower == "yes" {
					m.appendDisplay("You: " + input)
					m.refreshView()
					return m, func() tea.Msg { return confirmMsg{approved: true} }
				} else if lower == "n" || lower == "no" {
					m.appendDisplay("You: " + input)
					m.refreshView()
					return m, func() tea.Msg { return confirmMsg{approved: false} }
				}
				// Anything else: deny and fall through to normal prompt handling
				ch := m.confirmCh
				m.confirmCh = nil
				m.state = agentIdle
				ch <- false
			}

			m.drainAgent(false)
			m.finalizeBuf()
			m.appendDisplay("You: " + input)
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
	if strings.HasPrefix(input, "!") {
		cmdStr := strings.TrimSpace(input[1:])
		if cmdStr == "" {
			return true
		}
		out, err := exec.Command("sh", "-c", cmdStr).CombinedOutput()
		if err != nil {
			m.appendDisplay("Error: " + err.Error())
		}
		if len(out) > 0 {
			m.appendDisplay(strings.TrimRight(string(out), "\n"))
		}
		return true
	}
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

	case "/agent":
		if len(args) == 0 {
			m.display = append(m.display, fmt.Sprintf("active agent: %s", m.agentName))
			return true
		}
		name := args[0]
		cfgPath := agentConfigPath(m.agentsDir, name)
		cfg, err := config.Load(cfgPath)
		if err != nil {
			m.display = append(m.display, fmt.Sprintf("Error: agent %q: %v", name, err))
			return true
		}
		if m.mcpExec != nil {
			m.mcpExec.Close()
		}
		env := buildAgentEnv(cfg, m.builtinExec)
		m.mcpExec = env.mcpExec
		m.hooks = env.hooks
		m.loopcfg.SystemPrompt = env.systemPrompt
		m.loopcfg.Tools = env.tools
		m.loopcfg.Exec = env.exec
		m.loopcfg.GenerationParams = env.genParams
		m.ctxOverhead = env.ctxOverhead
		m.confirmPtr = env.confirm
		m.invalidateCaches = env.invalidateCaches
		m.agentName = name
		m.session = nil // new agent, new session
		for _, msg := range env.messages {
			m.display = append(m.display, msg)
		}
		m.display = append(m.display, fmt.Sprintf("agent: %s", name))
		return true

	case "/compact":
		if m.session == nil {
			m.display = append(m.display, "nothing to compact")
			return true
		}
		n, err := m.session.Compact(context.Background(), m.loopcfg.Backend, m.loopcfg.Model)
		if err != nil {
			m.display = append(m.display, "compact error: "+err.Error())
		} else if n == 0 {
			m.display = append(m.display, "nothing to compact")
		} else {
			m.display = append(m.display, fmt.Sprintf("compacted %d messages", n))
			if m.invalidateCaches != nil {
				m.invalidateCaches()
			}
		}
		return true

	case "/context":
		if m.session == nil {
			m.display = append(m.display, "no active session")
			return true
		}
		for _, line := range strings.Split(m.session.ContextDebug(), "\n") {
			m.display = append(m.display, line)
		}
		return true

	case "/history":
		if m.session == nil {
			m.display = append(m.display, "no active session")
			return true
		}
		for _, msg := range m.session.History() {
			preview := msg.Content
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			m.display = append(m.display, fmt.Sprintf("[%s] %s", msg.Role, preview))
		}
		return true

	case "/clear":
		m.session = nil
		m.display = nil
		m.buf = ""
		m.ctxStats = agent.ContextStats{}
		m.lastUsage = backend.Usage{}
		if m.invalidateCaches != nil {
			m.invalidateCaches()
		}
		return true

	case "/help":
		m.display = append(m.display, "Available commands:")
		m.display = append(m.display, "  /agent [name]    - Show or switch active agent")
		m.display = append(m.display, "  /backend <type>  - Switch backend (ollama, openai)")
		m.display = append(m.display, "  /model <name>    - Switch model")
		m.display = append(m.display, "  /compact         - Summarize evicted context messages")
		m.display = append(m.display, "  /context         - Show context window debug info")
		m.display = append(m.display, "  /history         - Dump bounded message history")
		m.display = append(m.display, "  /clear           - Clear session and display")
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

		loopcfg.Confirm = func(prompt string) bool {
			replyCh := make(chan bool, 1)
			select {
			case ch <- agentMsg{role: "confirm", content: prompt, confirmCh: replyCh}:
			case <-ctx.Done():
				return false
			}
			select {
			case approved := <-replyCh:
				return approved
			case <-ctx.Done():
				return false
			}
		}
		if m.confirmPtr != nil {
			*m.confirmPtr = loopcfg.Confirm
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

func dispatchBuiltinExec(ctx context.Context, name string, e *execpkg.Executor, confirm agent.ConfirmFn, args json.RawMessage) (string, error) {
	switch name {
	case "file_read":
		r, err := dispatchFileRead(confirm, args)
		return r.content, err
	case "file_write":
		return dispatchFileWrite(confirm, args)
	case "execute_pipe":
		return dispatchExecutePipe(ctx, e, confirm, args)
	case "execute_tool":
		return dispatchExecuteTool(ctx, e, confirm, args)
	default: // execute_code
		return dispatchExecuteCode(ctx, e, confirm, args)
	}
}

func execArgs(args json.RawMessage) (code, language, sandbox string, timeout int, err error) {
	var a struct {
		Code     string `json:"code"`
		Language string `json:"language"`
		Timeout  int    `json:"timeout"`
		Sandbox  string `json:"sandbox"`
	}
	if err = json.Unmarshal(args, &a); err != nil {
		return
	}
	code = a.Code
	language = a.Language
	if language == "" {
		language = "bash"
	}
	timeout = a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox = a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return
}

func dispatchExecuteCode(ctx context.Context, e *execpkg.Executor, confirm agent.ConfirmFn, args json.RawMessage) (string, error) {
	code, language, sandbox, timeout, err := execArgs(args)
	if err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}
	if code == "" {
		return "", fmt.Errorf("execute_code: 'code' is required")
	}
	if confirm != nil && !confirm(fmt.Sprintf("execute_code: %s", squashWhitespace(code))) {
		return "", fmt.Errorf("execute_code: denied by user")
	}
	return e.Execute(ctx, code, language, timeout, sandbox, false)
}

func dispatchExecuteTool(ctx context.Context, e *execpkg.Executor, confirm agent.ConfirmFn, args json.RawMessage) (string, error) {
	var a struct {
		Tool     string   `json:"tool"`
		Args     []string `json:"args"`
		Language string   `json:"language"`
		Timeout  int      `json:"timeout"`
		Sandbox  string   `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_tool: bad args: %w", err)
	}
	if a.Tool == "" {
		return "", fmt.Errorf("execute_tool: 'tool' is required")
	}
	if confirm != nil && !confirm(fmt.Sprintf("execute_tool: %s %s", a.Tool, strings.Join(a.Args, " "))) {
		return "", fmt.Errorf("execute_tool: denied by user")
	}
	toolCode, err := execpkg.ReadTool(a.Tool)
	if err != nil {
		return "", err
	}
	code := toolCode
	if len(a.Args) > 0 {
		var escaped []string
		for _, arg := range a.Args {
			escaped = append(escaped, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
		}
		code = fmt.Sprintf("set -- %s\n%s", strings.Join(escaped, " "), code)
	}
	language := a.Language
	if language == "" {
		language = "bash"
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox := a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return e.Execute(ctx, code, language, timeout, sandbox, true)
}

func dispatchExecutePipe(ctx context.Context, e *execpkg.Executor, confirm agent.ConfirmFn, args json.RawMessage) (string, error) {
	var a struct {
		Pipe    []execpkg.PipeStep `json:"pipe"`
		Timeout int                `json:"timeout"`
		Sandbox string             `json:"sandbox"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("execute_pipe: bad args: %w", err)
	}
	if len(a.Pipe) == 0 {
		return "", fmt.Errorf("execute_pipe: 'pipe' is required")
	}
	code, _, err := execpkg.BuildPipeline(a.Pipe)
	if err != nil {
		return "", err
	}
	if confirm != nil && !confirm(fmt.Sprintf("execute_pipe: %s", squashWhitespace(code))) {
		return "", fmt.Errorf("execute_pipe: denied by user")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandbox := a.Sandbox
	if sandbox == "" {
		sandbox = "default"
	}
	return e.Execute(ctx, code, "bash", timeout, sandbox, true)
}

const fileReadMaxLines = 500

// lineRange is an inclusive [start, end] line range (1-based).
type lineRange struct{ start, end int }

// fileReadResult carries the output of dispatchFileRead plus the range metadata
// needed for per-session coverage tracking.
type fileReadResult struct {
	content    string
	start      int // actual first line returned (1-based)
	end        int // actual last line returned (1-based)
	totalLines int // total lines in the file before any truncation
}

// rangesOverlap reports whether [ws, we] overlaps any interval in ranges.
func rangesOverlap(ranges []lineRange, ws, we int) bool {
	for _, r := range ranges {
		if r.start <= we && r.end >= ws {
			return true
		}
	}
	return false
}

// rangesCover reports whether the union of ranges fully contains [ws, we].
func rangesCover(ranges []lineRange, ws, we int) bool {
	sorted := make([]lineRange, len(ranges))
	copy(sorted, ranges)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	cur := 0
	for _, r := range sorted {
		if r.start > cur+1 {
			break
		}
		if r.end > cur {
			cur = r.end
		}
	}
	return cur >= we
}

func dispatchFileRead(confirm agent.ConfirmFn, args json.RawMessage) (fileReadResult, error) {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return fileReadResult{}, fmt.Errorf("file_read: bad args: %w", err)
	}
	if a.Path == "" {
		return fileReadResult{}, fmt.Errorf("file_read: 'path' is required")
	}
	if confirm != nil && !confirm(fmt.Sprintf("read %s", a.Path)) {
		return fileReadResult{}, fmt.Errorf("file_read: denied by user")
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return fileReadResult{}, fmt.Errorf("file_read: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	totalLines := len(lines)
	start := 1
	end := totalLines
	if a.StartLine > 0 {
		start = a.StartLine
	}
	if a.EndLine > 0 {
		end = a.EndLine
	}
	if start < 1 {
		start = 1
	}
	if end > totalLines {
		end = totalLines
	}
	if start > end {
		return fileReadResult{}, fmt.Errorf("file_read: start_line %d > end_line %d", start, end)
	}
	truncated := false
	if end-start+1 > fileReadMaxLines {
		end = start + fileReadMaxLines - 1
		truncated = true
	}
	var out strings.Builder
	for i, line := range lines[start-1 : end] {
		fmt.Fprintf(&out, "%d\t%s\n", start+i, line)
	}
	content := strings.TrimRight(out.String(), "\n")
	if truncated {
		content += fmt.Sprintf("\n[truncated: showing lines %d-%d of %d; use start_line/end_line or grep -n to narrow range]", start, end, totalLines)
	}
	return fileReadResult{content: content, start: start, end: end, totalLines: totalLines}, nil
}

func dispatchFileWrite(confirm agent.ConfirmFn, args json.RawMessage) (string, error) {
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("file_write: bad args: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("file_write: 'path' is required")
	}
	prompt := fmt.Sprintf("write %s", a.Path)
	if a.StartLine > 0 {
		prompt = fmt.Sprintf("write %s lines %d-%d", a.Path, a.StartLine, a.EndLine)
	}
	if confirm != nil && !confirm(prompt) {
		return "", fmt.Errorf("file_write: denied by user")
	}

	// Full overwrite
	if a.StartLine == 0 && a.EndLine == 0 {
		if err := os.WriteFile(a.Path, []byte(a.Content), 0644); err != nil {
			return "", fmt.Errorf("file_write: %w", err)
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
	}

	// Range replacement
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("file_write: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	start, end := a.StartLine, a.EndLine
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return "", fmt.Errorf("file_write: start_line %d > end_line %d", start, end)
	}
	newLines := strings.Split(a.Content, "\n")
	result := append(lines[:start-1], append(newLines, lines[end:]...)...)
	if err := os.WriteFile(a.Path, []byte(strings.Join(result, "\n")), 0644); err != nil {
		return "", fmt.Errorf("file_write: %w", err)
	}
	return fmt.Sprintf("replaced lines %d-%d in %s", start, end, a.Path), nil
}
