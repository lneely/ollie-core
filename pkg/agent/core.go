package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	"crypto/rand"

	"ollie/pkg/backend"
	"ollie/pkg/config"
	execute "ollie/pkg/tools/execute"
	"ollie/pkg/mcp"
	"ollie/pkg/tools"
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
Use execute_code for all shell commands and scripts. Use execute_tool only for named scripts in {{.ToolsPath}}. Use execute_pipe to chain steps: use {code: "cmd --flags"} for shell commands, {tool, args} only for named scripts in {{.ToolsPath}}.
Use grep or execute_code to search and explore files. Use file_read only when you need to write — it reads the full file and is required before file_write. Never use shell commands to read or write files.

Tool call examples:
  Read a file:      {"path": "/home/user/foo.go"}
  Run shell code:   {"code": "ls -la", "language": "bash"}
  List directory:   {"code": "find . -maxdepth 2 -type f", "language": "bash"}
  Run named tool:   {"tool": "discover_skill.sh", "args": ["keyword"]}
  Pipeline:         {"pipe": [{"code": "cat file.txt"}, {"code": "grep foo"}]}`

// buildFirstPrompt seeds the first user message with the project file listing
// and README so the agent has immediate context.
func buildFirstPrompt(input string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return input
	}

	var sb strings.Builder
	sb.WriteString(input)

	const maxListingBytes = 16 * 1024
	const maxReadmeBytes = 8 * 1024

	lsOut, err := exec.Command("git", "-C", cwd, "ls-files").Output()
	if err == nil && len(lsOut) > 0 {
		if len(lsOut) > maxListingBytes {
			lsOut = lsOut[:maxListingBytes]
			if i := bytes.LastIndexByte(lsOut, '\n'); i >= 0 {
				lsOut = lsOut[:i+1]
			}
			lsOut = append(lsOut, []byte("...(truncated)\n")...)
		}
		sb.WriteString("\n\n--- files (git ls-files) ---\n")
		sb.Write(lsOut)
	} else {
		var files []string
		filepath.WalkDir(cwd, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel := strings.TrimPrefix(path, cwd+"/")
			if rel == "" {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				files = append(files, rel)
			}
			return nil
		})
		if len(files) > 0 {
			sb.WriteString("\n\n--- files ---\n")
			written := 0
			for _, f := range files {
				line := f + "\n"
				if written+len(line) > maxListingBytes {
					sb.WriteString("...(truncated)\n")
					break
				}
				sb.WriteString(line)
				written += len(line)
			}
		}
	}

	readmeData, err := os.ReadFile(cwd + "/README.md")
	if err == nil && len(readmeData) > 0 {
		if len(readmeData) > maxReadmeBytes {
			readmeData = readmeData[:maxReadmeBytes]
			if i := bytes.LastIndexByte(readmeData, '\n'); i >= 0 {
				readmeData = readmeData[:i+1]
			}
			readmeData = append(readmeData, []byte("...(truncated)\n")...)
		}
		sb.WriteString("\n--- README.md ---\n")
		sb.Write(readmeData)
	}

	return sb.String()
}

// systemPrompt builds the full system prompt for a given tool set.
var systemPromptTmpl = template.Must(template.New("system").Parse(systemPromptBase))

type systemPromptData struct {
	ToolsPath string
}

func systemPrompt(allTools []backend.Tool) string {
	cwd, _ := os.Getwd()
	now := time.Now().Format("2006-01-02 15:04:05 MST")
	names := make([]string, len(allTools))
	for i, t := range allTools {
		names[i] = t.Name
	}
	var buf strings.Builder
	systemPromptTmpl.Execute(&buf, systemPromptData{ //nolint:errcheck
		ToolsPath: execute.ToolsPath(),
	})
	return buf.String() + "\n\nWorking directory: " + cwd +
		"\nCurrent time: " + now +
		"\nAvailable tools: " + strings.Join(names, ", ")
}

// AgentEnv holds the runtime state derived from an agent config file.
type AgentEnv struct {
	mcpExec          tools.Executor
	tools            []backend.Tool
	exec             toolExecutor
	confirm          *confirmFn
	Hooks            Hooks
	systemPrompt     string
	genParams        backend.GenerationParams
	CtxOverhead      int
	Messages         []string
}

// BuildAgentEnv constructs an AgentEnv from a config file and a builtin executor.
func BuildAgentEnv(cfg *config.Config, builtinExec *execute.Executor) AgentEnv {
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
			mcpExec.AddServer(name, tools.NewMCPServer(client))
			messages = append(messages, fmt.Sprintf("MCP %s: connected", name))
		}
	}

	// Wire confirm through pointer so it can be updated after construction.
	var cfn confirmFn
	confirmPtr := &cfn
	builtinExec.Confirm = func(prompt string) bool {
		if *confirmPtr == nil {
			return true
		}
		return (*confirmPtr)(prompt)
	}
	mcpExec.AddServer("builtin", tools.NewBuiltinServer(builtinExec))

	allToolInfos, listErr := mcpExec.ListTools()
	if listErr != nil {
		messages = append(messages, fmt.Sprintf("list tools: %v", listErr))
	}
	mcpCount := 0
	for _, t := range allToolInfos {
		if t.Server != "builtin" {
			mcpCount++
		}
	}
	if mcpCount > 0 {
		messages = append(messages, fmt.Sprintf("MCP tools loaded: %d", mcpCount))
	}

	serverOf := make(map[string]string, len(allToolInfos))
	for _, t := range allToolInfos {
		serverOf[t.Name] = t.Server
	}

	allTools := mcpToolsToBackend(allToolInfos)

	hooks := Hooks{}
	agentPrompt := ""
	var genParams backend.GenerationParams
	if cfg != nil {
		if cfg.Hooks != nil {
			hooks = cfg.Hooks
		}
		agentPrompt = cfg.Prompt
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

	exec := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		server, ok := serverOf[name]
		if !ok {
			return "", fmt.Errorf("unknown tool: %s", name)
		}
		raw, err := mcpExec.Execute(ctx, server, name, args)
		if err != nil {
			return "", err
		}
		return extractMCPText(raw), nil
	}

	return AgentEnv{
		mcpExec:      mcpExec,
		tools:        allTools,
		exec:         exec,
		confirm:      confirmPtr,
		Hooks:        hooks,
		systemPrompt: sp,
		genParams:    genParams,
		CtxOverhead:  overhead,
		Messages:     messages,
	}
}


// agentConfigPath resolves the config file path for a named agent.
func agentConfigPath(agentsDir, name string) string {
	p := agentsDir + "/" + name + ".json"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if name == "default" {
		home, _ := os.UserHomeDir()
		return home + "/.config/ollie/config.json"
	}
	return p
}

// newSessionID generates a unique session identifier.
func newSessionID() string {
	b := make([]byte, 3)
	rand.Read(b) //nolint:errcheck
	return time.Now().Format("20060102-150405") + "-" + fmt.Sprintf("%06x", b)
}


// infoEvent wraps a plain-text message as an info Event.
func infoEvent(text string) Event {
	return Event{Role: "info", Content: text + "\n"}
}

// actionHandle holds the cancel function for the current agent turn.
type actionHandle struct {
	cancel context.CancelCauseFunc
}

// AgentCoreConfig is the configuration for creating an agentCore.
type AgentCoreConfig struct {
	Backend     backend.Backend
	ModelName   string // if non-empty, overrides backend's default model
	AgentName   string
	AgentsDir   string
	SessionsDir string
	SessionID   string
	Session     *Session
	Env         AgentEnv
	BuiltinExec *execute.Executor
}

// agentCore is the Core implementation. It owns all agent and session state
// but has no knowledge of how output is rendered.
type agentCore struct {
	session          *Session
	loopcfg          loopConfig
	hooks            Hooks
	agentName        string
	agentsDir        string
	sessionsDir      string
	sessionID        string
	mcpExec          tools.Executor
	builtinExec      *execute.Executor
	confirmPtr    *confirmFn
	ctxOverhead   int
	currentAction    atomic.Pointer[actionHandle]
}

var _ Core = (*agentCore)(nil) // compile-time interface check

// NewAgentCore creates an agentCore from the given configuration.
func NewAgentCore(cfg AgentCoreConfig) Core {
	if cfg.ModelName != "" {
		cfg.Backend.SetModel(cfg.ModelName)
	}
	loopcfg := loopConfig{
		Backend:          cfg.Backend,
		systemPrompt:     cfg.Env.systemPrompt,
		Tools:            cfg.Env.tools,
		Exec:             cfg.Env.exec,
		MaxSteps:         20,
		GenerationParams: cfg.Env.genParams,
	}
	return &agentCore{
		session:          cfg.Session,
		loopcfg:          loopcfg,
		hooks:            cfg.Env.Hooks,
		agentName:        cfg.AgentName,
		agentsDir:        cfg.AgentsDir,
		sessionsDir:      cfg.SessionsDir,
		sessionID:        cfg.SessionID,
		mcpExec:          cfg.Env.mcpExec,
		builtinExec:      cfg.BuiltinExec,
		confirmPtr:  cfg.Env.confirm,
		ctxOverhead: cfg.Env.CtxOverhead,
	}
}

func (s *agentCore) prompt() string {
	return fmt.Sprintf("[%s :: %s] ", s.loopcfg.Backend.Name(), s.agentName)
}

// Prompt returns the display prompt string for the current session state.
func (s *agentCore) Prompt() string { return s.prompt() }

func (s *agentCore) saveSession() {
	if s.session == nil || s.sessionID == "" || s.sessionsDir == "" {
		return
	}
	path := s.sessionsDir + "/" + s.sessionID + ".json"
	if err := s.session.saveTo(path, s.sessionID, s.agentName); err != nil {
		fmt.Fprintln(os.Stderr, "session save:", err)
	}
}

func (s *agentCore) getActionCancel() context.CancelCauseFunc {
	if a := s.currentAction.Load(); a != nil {
		return a.cancel
	}
	return nil
}

// Interrupt cancels the current in-progress agent turn.
// Returns true if an action was running and was cancelled.
func (s *agentCore) Interrupt(cause error) bool {
	if cancel := s.getActionCancel(); cancel != nil {
		cancel(cause)
		return true
	}
	return false
}

// Submit implements Core. It processes one line of user input: slash commands
// and shell shortcuts are dispatched immediately via handler; any other input
// starts an agent turn that streams events to handler.
func (s *agentCore) Submit(ctx context.Context, input string, handler EventHandler) {
	if s.handleCommand(ctx, input, handler) {
		return
	}

	s.hooks.Run(HookUserPromptSubmit)

	if s.session == nil {
		s.session = newSessionWithConfig(buildFirstPrompt(input), contextConfig{
			FixedOverheadChars: s.ctxOverhead,
		})
	} else {
		s.session.appendUserMessage(input)
	}

	actCtx, actCancel := context.WithCancelCause(ctx)
	handle := &actionHandle{cancel: actCancel}
	s.currentAction.Store(handle)

	s.loopcfg.Output = handler
	*s.confirmPtr = nil // auto-approve all confirmations for now

	s.hooks.Run(HookAgentSpawn)

	err := run(actCtx, s.loopcfg, s.session)
	actCancel(nil)
	s.currentAction.CompareAndSwap(handle, nil)

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrInterrupted) {
		handler(Event{Role: "error", Content: err.Error()})
		s.session.rollback()
	}

	handler(Event{Role: "newline"})

	s.hooks.Run(HookStop)

	s.saveSession()
}

func (s *agentCore) handleCommand(ctx context.Context, input string, handler EventHandler) bool {
	if strings.HasPrefix(input, "!") {
		cmdStr := strings.TrimSpace(input[1:])
		if cmdStr == "" {
			return true
		}
		o, err := exec.Command("sh", "-c", cmdStr).CombinedOutput()
		if err != nil {
			handler(infoEvent("error: " + err.Error()))
		}
		if len(o) > 0 {
			handler(infoEvent(strings.TrimRight(string(o), "\n")))
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
			handler(infoEvent("error: /backend requires an argument (e.g., /backend ollama)"))
			return true
		}
		os.Setenv("OLLIE_BACKEND", args[0])
		be, err := backend.New()
		if err != nil {
			handler(infoEvent(fmt.Sprintf("error: failed to switch backend: %v", err)))
			return true
		}
		s.loopcfg.Backend = be
		handler(infoEvent(fmt.Sprintf("switched backend to: %s (model: %s)", be.Name(), be.Model())))
		return true

	case "/model":
		if len(args) == 0 {
			handler(infoEvent("error: /model requires an argument (e.g., /model qwen3:8b)"))
			return true
		}
		s.loopcfg.Backend.SetModel(args[0])
		handler(infoEvent("switched model to: " + args[0]))
		return true

	case "/agents":
		entries, err := os.ReadDir(s.agentsDir)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("agents: %v", err)))
			return true
		}
		found := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".json")
			marker := "  "
			if name == s.agentName {
				marker = "* "
			}
			handler(infoEvent(marker + name))
			found = true
		}
		if !found {
			handler(infoEvent("no agents found in " + s.agentsDir))
		}
		return true

	case "/agent":
		if len(args) == 0 {
			handler(infoEvent("active agent: " + s.agentName))
			return true
		}
		name := args[0]
		cfgPath := agentConfigPath(s.agentsDir, name)
		cfg, err := config.Load(cfgPath)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("error: agent %q: %v", name, err)))
			return true
		}
		if s.mcpExec != nil {
			s.mcpExec.Close()
		}
		env := BuildAgentEnv(cfg, s.builtinExec)
		s.mcpExec = env.mcpExec
		s.hooks = env.Hooks
		s.loopcfg.systemPrompt = env.systemPrompt
		s.loopcfg.Tools = env.tools
		s.loopcfg.Exec = env.exec
		s.loopcfg.GenerationParams = env.genParams
		s.ctxOverhead = env.CtxOverhead
		s.confirmPtr = env.confirm
		s.agentName = name
		s.session = nil
		s.sessionID = newSessionID()
		for _, msg := range env.Messages {
			handler(infoEvent(msg))
		}
		handler(infoEvent("agent: " + name))
		return true

	case "/compact":
		if s.session == nil {
			handler(infoEvent("nothing to compact"))
			return true
		}
		n, summary, err := s.session.compact(ctx, s.loopcfg.Backend)
		if err != nil {
			handler(infoEvent("compact error: " + err.Error()))
		} else if n == 0 {
			handler(infoEvent("nothing to compact"))
		} else {
			handler(infoEvent(fmt.Sprintf("compacted %d messages", n)))
			if summary != "" {
				handler(Event{Role: "newline"})
				handler(infoEvent(summary))
			}
			s.saveSession()
		}
		return true

	case "/context":
		if s.session == nil {
			handler(infoEvent("no active session"))
			return true
		}
		handler(infoEvent(strings.TrimRight(s.session.contextDebug(), "\n")))
		return true

	case "/history":
		if s.session == nil {
			handler(infoEvent("no active session"))
			return true
		}
		for _, msg := range s.session.history() {
			preview := msg.Content
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			handler(infoEvent(fmt.Sprintf("[%s] %s", msg.Role, preview)))
		}
		return true

	case "/clear":
		s.session = nil
		s.sessionID = newSessionID()
		handler(infoEvent("cleared"))
		return true

	case "/sessions":
		entries, err := os.ReadDir(s.sessionsDir)
		if err != nil {
			handler(infoEvent(fmt.Sprintf("sessions: %v", err)))
			return true
		}
		found := false
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			marker := "  "
			if id == s.sessionID {
				marker = "* "
			}
			label := id
			if data, readErr := os.ReadFile(s.sessionsDir + "/" + e.Name()); readErr == nil {
				var ps PersistedSession
				if json.Unmarshal(data, &ps) == nil {
					goal := ""
					for _, msg := range ps.Messages {
						if msg.Role == "user" {
							goal = msg.Content
							break
						}
					}
					if len(goal) > 60 {
						goal = goal[:60] + "..."
					}
					label = fmt.Sprintf("%-24s  [%s] %q", id, ps.Agent, goal)
				}
			}
			handler(infoEvent(marker + label))
			found = true
		}
		if !found {
			handler(infoEvent("no sessions found in " + s.sessionsDir))
		}
		return true

	case "/help":
		lines := []string{
			"Available commands:",
			"  /agents          - list available agent configs",
			"  /sessions        - list saved sessions",
			"  /agent [name]    - show or switch active agent",
			"  /backend <type>  - switch backend (ollama, openai)",
			"  /model <name>    - switch model",
			"  /queued [pop|clear] - manage queued prompts",
			"  /compact         - summarize evicted context messages",
			"  /context         - show context window debug info",
			"  /history         - dump bounded message history",
			"  /clear           - clear session",
			"  /help            - show this help",
			"  !<cmd>           - run shell command",
		}
		for _, l := range lines {
			handler(infoEvent(l))
		}
		return true
	}

	return false
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

