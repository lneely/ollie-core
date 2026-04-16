package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"ollie/internal/sandbox"
	"regexp"
	"strings"
	"sync"
)

// CodeStep is one step in a parallel execute_code call or a parallel pipe stage.
// Exactly one of Code or Tool must be set.
type CodeStep struct {
	Code     string   `json:"code"`
	Language string   `json:"language"`
	Tool     string   `json:"tool"`
	Args     []string `json:"args"`
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
	case "execute_tool":
		return dispatchExecuteTool(ctx, e, args)
	case "execute_pipe":
		return dispatchExecutePipe(ctx, e, args)
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

func loadLayeredConfig(name string) (*sandbox.Config, error) {
	return sandbox.LoadMerged(name)
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
	steps, timeout, sandboxName, err := execCodeArgs(args)
	if err != nil {
		return "", fmt.Errorf("execute_code: bad args: %w", err)
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("execute_code: at least one step is required")
	}

	// Build confirmation label and check upfront (before any goroutines).
	stepLabel := func(s CodeStep) string {
		if s.Tool != "" {
			return fmt.Sprintf("tool:%s %s", s.Tool, strings.Join(s.Args, " "))
		}
		return s.Code
	}
	var label string
	if len(steps) == 1 {
		label = fmt.Sprintf("execute_code: %s", stepLabel(steps[0]))
	} else {
		var sb strings.Builder
		fmt.Fprintf(&sb, "execute_code: %d parallel steps", len(steps))
		for i, s := range steps {
			fmt.Fprintf(&sb, "\n  [%d]: %s", i, stepLabel(s))
		}
		label = sb.String()
	}
	if !e.allowed("execute_code", label) {
		return "", fmt.Errorf("execute_code: denied by user")
	}

	if len(steps) == 1 {
		code, lang, trusted, err := resolveCodeStep(steps[0])
		if err != nil {
			return "", err
		}
		return e.executeWithStdin(ctx, code, lang, timeout, sandboxName, trusted, "")
	}

	// Parallel fan-out: all steps run concurrently; results collected in submission order.
	type result struct {
		output string
		err    error
	}
	results := make([]result, len(steps))
	var wg sync.WaitGroup
	for i, step := range steps {
		wg.Add(1)
		go func(idx int, s CodeStep) {
			defer wg.Done()
			code, lang, trusted, err := resolveCodeStep(s)
			if err != nil {
				results[idx] = result{err: err}
				return
			}
			out, err := e.executeWithStdin(ctx, code, lang, timeout, sandboxName, trusted, "")
			results[idx] = result{output: out, err: err}
		}(i, step)
	}
	wg.Wait()

	var sb strings.Builder
	var firstErr error
	for i, r := range results {
		fmt.Fprintf(&sb, "=== step %d ===\n", i)
		sb.WriteString(r.output)
		if r.err != nil {
			fmt.Fprintf(&sb, "\nerror: %v\n", r.err)
			if firstErr == nil {
				firstErr = r.err
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String(), firstErr
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
