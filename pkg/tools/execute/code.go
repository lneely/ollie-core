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

// CodeStep is one stage in an execute_code pipeline.
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

// execute_code is implemented directly by Executor.Execute with trusted=false.
// See executor.go for the implementation.

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
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\.\.?(/|$)`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+~`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\*`),
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&`), // fork bomb
	regexp.MustCompile(`>\s*/dev/sd`),
	regexp.MustCompile(`\beval\s+".*\$`),
}

// pythonPatterns apply to python3/python.
var pythonPatterns = []*regexp.Regexp{
	regexp.MustCompile(`shutil\.rmtree\s*\(\s*['"]/`),
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

// Dispatch routes a named execute tool call. Called by tools.BuiltinServer.
func (e *Server) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "execute_code":
		return dispatchExecuteCode(ctx, e, args)
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

func dispatchExecuteCode(ctx context.Context, e *Server, args json.RawMessage) (string, error) {
	stages, timeout, sandboxName, err := execCodeArgs(args)
	if err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}
	if len(stages) == 0 {
		return "", fmt.Errorf("execute_code: at least one step is required")
	}

	if e.Strict {
		if err := enforceStrict(stages); err != nil {
			return "", err
		}
	}

	label := buildExecLabel(stages)
	if !e.allowed("execute_code", label) {
		return "", fmt.Errorf("execute_code: denied by user")
	}

	// Degenerate case: single simple stage, return raw output.
	if len(stages) == 1 && len(stages[0].Parallel) == 0 {
		if stages[0].Elevated {
			e.wdMu.RLock()
			dir := e.cwd
			e.wdMu.RUnlock()
			return e.executeElevated(ctx, stages[0].Code, dir, timeout)
		}
		code, lang, trusted, err := resolveCodeStep(stages[0])
		if err != nil {
			return "", err
		}
		return e.executeWithStdin(ctx, code, lang, timeout, sandboxName, trusted, "")
	}

	// Pipeline: stages run sequentially, stdout piped to next stdin.
	var input string
	for i, stage := range stages {
		out, err := e.runStage(ctx, i, stage, timeout, sandboxName, input)
		if err != nil {
			return out, fmt.Errorf("step %d: %w", i, err)
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

func execCodeArgs(args json.RawMessage) (steps []CodeStep, timeout int, sandboxName string, err error) {
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
