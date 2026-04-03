package exec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"9fans.net/go/plan9/client"
	"anvillm/pkg/sandbox"
)

// ReadTool reads a named tool script from the 9P tools directory.
func ReadTool(name string) (string, error) {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid tool name")
	}

	ns := fmt.Sprintf("/tmp/ns.%s.:0", os.Getenv("USER"))
	fsys, err := client.Mount("unix", filepath.Join(ns, "anvillm"))
	if err != nil {
		return "", fmt.Errorf("failed to mount 9P: %v", err)
	}
	defer fsys.Close()

	fid, err := fsys.Open("/tools/"+name, 0)
	if err != nil {
		return "", fmt.Errorf("tool not found: %s", name)
	}
	defer fid.Close()

	var buf []byte
	tmp := make([]byte, 8192)
	for {
		n, err := fid.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil || n < len(tmp) {
			break
		}
	}
	return string(buf), nil
}

// PipeStep is one stage in a tool pipeline.
// Exactly one of Tool or Code must be set.
type PipeStep struct {
	Tool string // named tool read from 9P (trusted)
	Code string // inline bash code (untrusted, validated)
	Args []string
}

// BuildPipeline constructs a single bash pipeline string from the given steps.
// Tool steps are trusted (sourced from 9P); inline code steps are validated
// individually so the combined string is always returned as trusted.
func BuildPipeline(steps []PipeStep) (string, bool, error) {
	if len(steps) == 0 {
		return "", false, fmt.Errorf("pipe requires at least one step")
	}
	validator := &Executor{} // used only for ValidateCode; fresh instance = no shared rate limit state
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		var code string
		if step.Tool != "" {
			var err error
			code, err = ReadTool(step.Tool)
			if err != nil {
				return "", false, fmt.Errorf("pipe step %q: %v", step.Tool, err)
			}
		} else if step.Code != "" {
			if err := validator.ValidateCode(step.Code); err != nil {
				return "", false, fmt.Errorf("pipe step code: %v", err)
			}
			code = step.Code
		} else {
			return "", false, fmt.Errorf("each pipe step requires either 'tool' or 'code'")
		}
		if len(step.Args) > 0 {
			var escaped []string
			for _, arg := range step.Args {
				escaped = append(escaped, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
			}
			parts = append(parts, fmt.Sprintf("( set -- %s\n%s )", strings.Join(escaped, " "), code))
		} else {
			parts = append(parts, fmt.Sprintf("(\n%s\n)", code))
		}
	}
	return strings.Join(parts, " |\n"), true, nil
}

// Executor runs code in a sandboxed environment.
type Executor struct {
	// LogDir is the directory for execution and security event logs.
	// Defaults to ~/.local/state/exec when empty.
	LogDir string

	// WorkspaceBase is the parent directory for temporary execution workspaces.
	// If set, each execution gets a fresh temp directory under WorkspaceBase.
	// If empty, the current working directory is used directly.
	WorkspaceBase string

	// rate limiting state (per-Executor)
	rateLimitMu        sync.Mutex
	validationFailures int
	lastFailure        time.Time
	blockedUntil       time.Time
}

// New creates a new Executor with the given log directory and workspace base.
func New(logDir, workspaceBase string) *Executor {
	return &Executor{LogDir: logDir, WorkspaceBase: workspaceBase}
}

func (e *Executor) logDir() string {
	if e.LogDir != "" {
		return e.LogDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "exec")
}

var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`), // rm -rf, rm -r -f on sensitive paths
	regexp.MustCompile(`rm\s+(-[a-z]*f[a-z]*\s+)*-[a-z]*r[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`), // rm -fr, rm -f -r on sensitive paths
	regexp.MustCompile(`rm\s+.*--recursive.*--force`),                                                                    // rm --recursive --force
	regexp.MustCompile(`rm\s+.*--force.*--recursive`),                                                                    // rm --force --recursive
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\.\.?(/|$)`),                                          // rm -rf ./ or rm -rf ../
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+~`),                                                   // rm -rf ~ (home dir)
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\*`),                                                  // rm -rf * (glob expansion)
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&`),                                                              // fork bomb
	regexp.MustCompile(`\bmkfs\b`),                                                                                       // filesystem format
	regexp.MustCompile(`\bdd\b.*\bif=/dev/`),                                                                             // dd from device
	regexp.MustCompile(`>\s*/dev/sd`),                                                                                    // write to block device
	regexp.MustCompile(`\beval\s+".*\$`),                                                                                 // eval with variable expansion
	regexp.MustCompile(`\b(sudo|su)\s`),                                                                                  // privilege escalation
	regexp.MustCompile(`/etc/(shadow|sudoers)`),                                                                          // sensitive files (not passwd)
}

var whitespacePattern = regexp.MustCompile(`\s+`)

const (
	maxFailures   = 5
	blockDuration = 30 * time.Second
	failureWindow = 60 * time.Second
)

func (e *Executor) checkRateLimit() error {
	e.rateLimitMu.Lock()
	defer e.rateLimitMu.Unlock()

	now := time.Now()
	if now.Before(e.blockedUntil) {
		remaining := e.blockedUntil.Sub(now).Round(time.Second)
		return fmt.Errorf("rate limited: too many validation failures, blocked for %v", remaining)
	}
	return nil
}

func (e *Executor) recordValidationFailure() {
	e.rateLimitMu.Lock()
	defer e.rateLimitMu.Unlock()

	now := time.Now()
	if now.Sub(e.lastFailure) > failureWindow {
		e.validationFailures = 0
	}

	e.validationFailures++
	e.lastFailure = now

	if e.validationFailures >= maxFailures {
		e.blockedUntil = now.Add(blockDuration)
		e.validationFailures = 0
		logSecurityEvent(e.logDir(), SecurityEvent{
			Timestamp: now,
			EventType: "rate_limit_triggered",
			Details:   fmt.Sprintf("blocked for %v after %d failures", blockDuration, maxFailures),
		})
	}
}

// ValidateCode checks code against dangerous patterns.
func (e *Executor) ValidateCode(code string) error {
	if err := e.checkRateLimit(); err != nil {
		return err
	}

	normalized := strings.ToLower(code)
	normalized = whitespacePattern.ReplaceAllString(normalized, " ")

	for _, pattern := range dangerousPatterns {
		if pattern.MatchString(normalized) {
			e.recordValidationFailure()
			logSecurityEvent(e.logDir(), SecurityEvent{
				Timestamp: time.Now(),
				EventType: "validation_failure",
				Details:   fmt.Sprintf("dangerous pattern: %s", pattern.String()),
			})
			return fmt.Errorf("dangerous pattern detected")
		}
	}
	return nil
}

// IsPermissionError returns true if err indicates a permission or missing-file error.
func IsPermissionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "no such file or directory")
}

func loadLayeredConfig(name string) (*sandbox.Config, error) {
	baseCfg, err := sandbox.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load global config: %w", err)
	}

	baseLayer := sandbox.LayeredConfig{
		Filesystem: baseCfg.Filesystem,
		Network:    baseCfg.Network,
		Env:        baseCfg.Env,
	}
	layers := []sandbox.LayeredConfig{baseLayer}

	if name == "" {
		name = "default"
	}
	sbxLayer, err := sandbox.LoadSandbox(name)
	if err != nil {
		return nil, fmt.Errorf("failed to load sandbox %q: %w", name, err)
	}
	layers = append(layers, sbxLayer)

	general := sandbox.GeneralConfig{
		BestEffort: baseCfg.General.BestEffort,
		LogLevel:   baseCfg.General.LogLevel,
	}
	advanced := sandbox.AdvancedConfig{
		LDD:     baseCfg.Advanced.LDD,
		AddExec: baseCfg.Advanced.AddExec,
	}

	return sandbox.Merge(general, advanced, layers...), nil
}

// Execute runs code in a sandbox and returns combined stdout+stderr.
func (e *Executor) Execute(code, language string, timeout int, sandboxName string, trusted bool) (string, error) {
	start := time.Now()

	if timeout <= 0 {
		timeout = 30
	}

	if !trusted {
		if err := e.ValidateCode(code); err != nil {
			logExecution(e.logDir(), ExecutionLog{
				Timestamp:  start,
				CodeHash:   hashCode(code),
				Language:   language,
				Duration:   time.Since(start),
				Success:    false,
				OutputSize: 0,
				Error:      err.Error(),
			})
			return "", err
		}
	}

	cfg, err := loadLayeredConfig(sandboxName)
	if err != nil {
		return "", err
	}

	workDir, _ := os.Getwd()
	var cleanupWorkDir bool

	if e.WorkspaceBase != "" {
		if err := os.MkdirAll(e.WorkspaceBase, 0700); err != nil {
			return "", fmt.Errorf("failed to create workspace base: %v", err)
		}
		workDir, err = os.MkdirTemp(e.WorkspaceBase, "exec-*")
		if err != nil {
			return "", fmt.Errorf("failed to create workspace: %v", err)
		}
		cleanupWorkDir = true
	}

	if cleanupWorkDir {
		defer os.RemoveAll(workDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch language {
	case "bash", "":
		wrapped := sandbox.WrapCommand(cfg, []string{"bash", "-c", code}, workDir)
		cmd = exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
		cmd.Dir = workDir
	default:
		return "", fmt.Errorf("unsupported language: %s (supported: bash)", language)
	}

	const maxToolOutputSize = 8000
	var outputBuf bytes.Buffer
	lw := &limitedWriter{w: &outputBuf, limit: 10 * 1024 * 1024}
	cmd.Stdout = lw
	cmd.Stderr = lw

	err = cmd.Run()
	output := outputBuf.Bytes()

	if lw.truncated {
		output = append(output, []byte("\n[output truncated at 10MB]")...)
	}

	duration := time.Since(start)

	execLog := ExecutionLog{
		Timestamp:  start,
		CodeHash:   hashCode(code),
		Language:   language,
		Duration:   duration,
		Success:    err == nil && ctx.Err() != context.DeadlineExceeded,
		OutputSize: len(output),
	}

	if ctx.Err() == context.DeadlineExceeded {
		execLog.Error = fmt.Sprintf("execution timeout after %d seconds", timeout)
		logExecution(e.logDir(), execLog)
		logSecurityEvent(e.logDir(), SecurityEvent{
			Timestamp: start,
			EventType: "timeout",
			Language:  language,
			Details:   fmt.Sprintf("timeout after %d seconds", timeout),
		})
		return "", fmt.Errorf("execution timeout after %d seconds", timeout)
	}
	if err != nil {
		execLog.Error = err.Error()
		logExecution(e.logDir(), execLog)
		return string(output), fmt.Errorf("execution failed: %v\nOutput: %s", err, string(output))
	}

	logExecution(e.logDir(), execLog)
	result := string(output)
	if len(result) > maxToolOutputSize {
		result = result[:maxToolOutputSize] + "\n... (output truncated)"
	}
	return result, nil
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
