package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"ollie/internal/sandbox"
	"ollie/pkg/paths"

	"regexp"
	"strings"
)

// CodeStep is one stage in an execution pipeline.
// Set Code/Language for inline code, Tool/Args for a named script, or Parallel for concurrent fan-out.
// Set Elevated to run the step outside the sandbox via the configured elevation backend.
type CodeStep struct {
	Code     string     `json:"code"`
	Language string     `json:"language"`
	Tool     string     `json:"tool"`
	Args     []string   `json:"args"`
	Parallel []CodeStep `json:"parallel"`
	Elevated bool       `json:"elevated"`
}

// resolveCodeStep loads a CodeStep into executable (code, language, trusted).
// Tool steps are read from the tools directory and treated as trusted.
// Inline code steps default to bash and are validated by the caller.
func resolveCodeStep(s CodeStep) (code, language string, trusted bool, err error) {
	if s.Tool != "" {
		toolCode, terr := ReadTool(s.Tool)
		if terr != nil {
			return "", "", false, terr
		}
		language = detectLanguage(toolCode)
		code = toolCode
		if len(s.Args) > 0 {
			code = injectArgs(language, s.Tool, s.Args, toolCode)
			switch language {
			case "awk", "sed", "jq", "ed", "expect", "bc":
				language = "bash"
			}
		}
		return code, language, true, nil
	}
	language = s.Language
	if language == "" {
		language = "bash"
	}
	return s.Code, language, false, nil
}

// universalPatterns apply to all general-purpose languages.
var universalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bmkfs\b`),
	regexp.MustCompile(`\bdd\b.*\bif=/dev/`),
	regexp.MustCompile(`\b(sudo|su)\s`),
	regexp.MustCompile(`/etc/(shadow|sudoers)`),
}

// bashPatterns apply only to bash (flag syntax, redirects, shell-specific constructs).
var bashPatterns = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`),
	regexp.MustCompile(`rm\s+(-[a-z]*f[a-z]*\s+)*-[a-z]*r[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`),
	regexp.MustCompile(`rm\s+.*--recursive.*--force`),
	regexp.MustCompile(`rm\s+.*--force.*--recursive`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\.\.[/]`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+~`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\*`),
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&`), // fork bomb
	regexp.MustCompile(`>\s*/dev/sd`),
	regexp.MustCompile(`\beval\s+".*\$`),
}

// pythonPatterns apply to python3/python.
var pythonPatterns = []*regexp.Regexp{
	regexp.MustCompile(`shutil\.rmtree\s*\(\s*['"/]`),
	regexp.MustCompile(`(os\.system|subprocess\.(call|run|popen))\s*\(.*\brm\s+-[a-z]*r[a-z]*f`),
	regexp.MustCompile(`os\.(remove|unlink)\s*\(\s*['"]/(etc|usr|bin|sbin|lib|boot)`),
}

// perlPatterns apply to perl.
var perlPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(system|exec)\s*\(.*\brm\s+-[a-z]*r[a-z]*f`),
	regexp.MustCompile("`.+rm\\s+-[a-z]*r[a-z]*f"), // backtick execution
	regexp.MustCompile(`unlink\s+glob\s*\(['"]/(etc|usr|bin|sbin|lib|boot)`),
}

// luaPatterns apply to lua.
var luaPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(os\.execute|io\.popen)\s*\(.*\brm\s+-[a-z]*r[a-z]*f`),
	regexp.MustCompile(`os\.remove\s*\(\s*['"]/(etc|usr|bin|sbin|lib|boot)`),
}

var languagePatterns = map[string][]*regexp.Regexp{
	"bash":    bashPatterns,
	"":        bashPatterns,
	"python3": pythonPatterns,
	"python":  pythonPatterns,
	"perl":    perlPatterns,
	"lua":     luaPatterns,
}

// Dispatch routes a named execute tool call.
func (e *Server) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "execute_code":
		return dispatchExecuteCode(ctx, e, args)
	case "call_tool":
		return dispatchCallTool(ctx, e, args)
	case "pipe":
		return dispatchPipeCall(ctx, e, args)
	default:
		return "", fmt.Errorf("unknown execute tool: %s", name)
	}
}

// ValidateCode checks code against dangerous patterns for the given language.
func (e *Server) ValidateCode(code, language string) error {
	if err := e.checkRateLimit(); err != nil {
		return err
	}

	normalized := strings.ToLower(code)
	normalized = whitespacePattern.ReplaceAllString(normalized, " ")

	patterns := append(universalPatterns, languagePatterns[language]...)
	for _, pattern := range patterns {
		if pattern.MatchString(normalized) {
			e.recordValidationFailure()
			return fmt.Errorf("dangerous pattern detected")
		}
	}
	return nil
}

func loadSandboxConfig(name string) (*sandbox.Config, error) {
	if name == "" {
		name = "default"
	}
	path := filepath.Join(paths.CfgDir(), "sandbox", name+".yaml")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q not found: %w", name, err)
	}
	defer f.Close()
	return sandbox.LoadSandbox(f)
}

type limitedWriter struct {
	w         io.Writer
	written   int
	limit     int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (n int, err error) {
	if lw.written >= lw.limit {
		lw.truncated = true
		return len(p), nil
	}

	remaining := lw.limit - lw.written
	toWrite := p
	if len(p) > remaining {
		toWrite = p[:remaining]
		lw.truncated = true
	}

	written, err := lw.w.Write(toWrite)
	lw.written += written
	if err != nil {
		return written, err
	}
	return len(p), nil
}

// dispatchExecuteCode handles the execute_code tool: inline code steps only.
// The legacy "pipe" field is no longer accepted here; use the pipe tool instead.
func dispatchExecuteCode(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	steps, timeout, sandboxName, err := execCodeOnlyArgs(args)
	if err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("execute_code: steps is required")
	}
	return e.dispatchSteps(ctx, steps, timeout, sandboxName)
}

// dispatchCallTool handles the call_tool tool: named tool scripts only.
// Inline code fields are rejected. Fan-out behaviour is preserved.
func dispatchCallTool(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	calls, timeout, sandboxName, err := execCallToolArgs(args)
	if err != nil {
		return "", fmt.Errorf("call_tool: bad args: %w", err)
	}
	if len(calls) == 0 {
		return "", fmt.Errorf("call_tool: calls is required")
	}
	// Validate: every top-level call must name a tool (or be a parallel group of tools).
	for i, c := range calls {
		if len(c.Parallel) > 0 {
			for j, p := range c.Parallel {
				if p.Tool == "" {
					return "", fmt.Errorf("call_tool: calls[%d].parallel[%d]: tool name is required", i, j)
				}
			}
			continue
		}
		if c.Tool == "" {
			return "", fmt.Errorf("call_tool: calls[%d]: tool name is required", i)
		}
	}
	return e.dispatchSteps(ctx, calls, timeout, sandboxName)
}

// dispatchPipeCall handles the pipe tool: sequential pipeline of code and/or tool stages.
func dispatchPipeCall(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	stages, timeout, sandboxName, err := execPipeArgs(args)
	if err != nil {
		return "", fmt.Errorf("pipe: bad args: %w", err)
	}
	if len(stages) == 0 {
		return "", fmt.Errorf("pipe: stages is required")
	}
	return e.dispatchPipe(ctx, stages, timeout, sandboxName)
}

// dispatchSteps runs steps in parallel when safe (annotation-based), serially otherwise.
func (e *Server) dispatchSteps(ctx context.Context, steps []CodeStep, timeout int, sandboxName string) (string, error) {
	if e.Strict {
		if err := enforceStrict(steps); err != nil {
			return "", err
		}
	}

	label := buildExecLabel(steps)
	if !e.allowed("execute_code", label) {
		return "", fmt.Errorf("execute_code: denied by user")
	}

	// Single step: run directly.
	if len(steps) == 1 && len(steps[0].Parallel) == 0 {
		if steps[0].Elevated {
			e.wdMu.RLock()
			dir := e.cwd
			e.wdMu.RUnlock()
			return e.executeElevated(ctx, steps[0].Code, dir, timeout)
		}
		code, lang, trusted, err := resolveCodeStep(steps[0])
		if err != nil {
			return "", err
		}
		return e.executeWithStdin(ctx, code, lang, timeout, sandboxName, trusted, "")
	}

	// Multiple steps: auto-batch consecutive read-safe steps in parallel;
	// write/global steps run serially between batches.
	var output string
	for i := 0; i < len(steps); {
		if classifyStep(steps[i]) == lockClassRead {
			j := i + 1
			for j < len(steps) && classifyStep(steps[j]) == lockClassRead {
				j++
			}
			out, err := e.runReadBatch(ctx, i, steps[i:j], timeout, sandboxName, "")
			if err != nil {
				return out, err
			}
			output += out
			i = j
		} else {
			lf, err := acquireFlock(e.lockDir, "rw", true)
			if err != nil {
				return "", fmt.Errorf("step %d: lock: %w", i, err)
			}
			out, runErr := e.runStage(ctx, i, steps[i], timeout, sandboxName, "")
			if lf != nil {
				lf.Close()
			}
			if runErr != nil {
				return output + out, fmt.Errorf("step %d: %w", i, runErr)
			}
			output += out
			i++
		}
	}
	return output, nil
}

// dispatchPipe runs stages as an explicit pipeline: sequential, stdout of each
// stage feeds stdin of the next.
func (e *Server) dispatchPipe(ctx context.Context, pipe []CodeStep, timeout int, sandboxName string) (string, error) {
	if e.Strict {
		if err := enforceStrict(pipe); err != nil {
			return "", err
		}
	}

	label := buildExecLabel(pipe)
	if !e.allowed("execute_code", label) {
		return "", fmt.Errorf("execute_code: denied by user")
	}

	var input string
	for i, stage := range pipe {
		out, err := e.runStage(ctx, i, stage, timeout, sandboxName, input)
		if err != nil {
			return out, fmt.Errorf("pipe stage %d: %w", i, err)
		}
		input = out
	}
	return input, nil
}

func buildExecLabel(stages []CodeStep) string {
	stageLabel := func(s CodeStep) string {
		prefix := ""
		if s.Elevated {
			prefix = "[elevated] "
		}
		if len(s.Parallel) > 0 {
			return fmt.Sprintf("%sparallel(%d)", prefix, len(s.Parallel))
		}
		if s.Tool != "" {
			return fmt.Sprintf("%stool:%s %s", prefix, s.Tool, strings.Join(s.Args, " "))
		}
		return prefix + s.Code
	}
	if len(stages) == 1 {
		return fmt.Sprintf("execute_code: %s", stageLabel(stages[0]))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "execute_code: %d stages", len(stages))
	for i, s := range stages {
		fmt.Fprintf(&sb, "\n  [%d]: %s", i, stageLabel(s))
	}
	return sb.String()
}

// enforceStrict rejects any step that uses inline code rather than a named tool.
func enforceStrict(stages []CodeStep) error {
	for i, s := range stages {
		if len(s.Parallel) > 0 {
			if err := enforceStrict(s.Parallel); err != nil {
				return err
			}
			continue
		}
		if s.Tool == "" {
			return fmt.Errorf("execute_code: step %d rejected: inline code not allowed in strict mode", i)
		}
	}
	return nil
}

// execCodeOnlyArgs parses args for the execute_code tool (steps only).
func execCodeOnlyArgs(args json.RawMessage) (steps []CodeStep, timeout int, sandboxName string, err error) {
	var a struct {
		Steps   []CodeStep `json:"steps"`
		Timeout int        `json:"timeout"`
		Sandbox string     `json:"sandbox"`
	}
	if err = json.Unmarshal(args, &a); err != nil {
		return
	}
	steps = a.Steps
	timeout = a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandboxName = a.Sandbox
	if sandboxName == "" {
		sandboxName = "default"
	}
	return
}

// execCallToolArgs parses args for the call_tool tool (calls array).
func execCallToolArgs(args json.RawMessage) (calls []CodeStep, timeout int, sandboxName string, err error) {
	var a struct {
		Calls   []CodeStep `json:"calls"`
		Timeout int        `json:"timeout"`
		Sandbox string     `json:"sandbox"`
	}
	if err = json.Unmarshal(args, &a); err != nil {
		return
	}
	calls = a.Calls
	timeout = a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandboxName = a.Sandbox
	if sandboxName == "" {
		sandboxName = "default"
	}
	return
}

// execPipeArgs parses args for the pipe tool (stages array).
func execPipeArgs(args json.RawMessage) (stages []CodeStep, timeout int, sandboxName string, err error) {
	var a struct {
		Stages  []CodeStep `json:"stages"`
		Timeout int        `json:"timeout"`
		Sandbox string     `json:"sandbox"`
	}
	if err = json.Unmarshal(args, &a); err != nil {
		return
	}
	stages = a.Stages
	timeout = a.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	sandboxName = a.Sandbox
	if sandboxName == "" {
		sandboxName = "default"
	}
	return
}
