// Package execute provides the builtin execute_code, execute_tool, and
// execute_pipe tools. The Executor struct is the shared sandbox runner.
package execute

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ollie/internal/sandbox"
)

const defaultMaxOutputChars = 8000

// Executor runs code in a sandboxed environment.
type Executor struct {
	// LogDir is the directory for execution and security event logs.
	// Defaults to ~/.local/state/exec when empty.
	LogDir string

	// WorkspaceBase is the parent directory for temporary execution workspaces.
	// If set, each execution gets a fresh temp directory under WorkspaceBase.
	// If empty, the current working directory is used directly.
	WorkspaceBase string

	// MaxOutputChars is the maximum number of characters returned from Execute.
	// Set via OLLIE_TOOL_OUTPUT_CHARS env var or directly on the struct.
	// Defaults to 8000.
	MaxOutputChars int

	// rate limiting state (per-Executor)
	rateLimitMu        sync.Mutex
	validationFailures int
	lastFailure        time.Time
	blockedUntil       time.Time
}

// New creates a new Executor with the given log directory and workspace base.
// MaxOutputChars is initialised from OLLIE_TOOL_OUTPUT_CHARS if set,
// otherwise defaults to 8000.
func New(logDir, workspaceBase string) *Executor {
	maxOutput := defaultMaxOutputChars
	if s := os.Getenv("OLLIE_TOOL_OUTPUT_CHARS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxOutput = n
		}
	}
	return &Executor{
		LogDir:         logDir,
		WorkspaceBase:  workspaceBase,
		MaxOutputChars: maxOutput,
	}
}

func (e *Executor) logDir() string {
	if e.LogDir != "" {
		return e.LogDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "exec")
}

var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`),
	regexp.MustCompile(`rm\s+(-[a-z]*f[a-z]*\s+)*-[a-z]*r[a-z]*\s*/(home|var|usr|etc|boot|root|bin|sbin|lib|opt|srv)?`),
	regexp.MustCompile(`rm\s+.*--recursive.*--force`),
	regexp.MustCompile(`rm\s+.*--force.*--recursive`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\.\.?(/|$)`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+~`),
	regexp.MustCompile(`rm\s+(-[a-z]*r[a-z]*\s+)*-[a-z]*f[a-z]*\s+\*`),
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&`),
	regexp.MustCompile(`\bmkfs\b`),
	regexp.MustCompile(`\bdd\b.*\bif=/dev/`),
	regexp.MustCompile(`>\s*/dev/sd`),
	regexp.MustCompile(`\beval\s+".*\$`),
	regexp.MustCompile(`\b(sudo|su)\s`),
	regexp.MustCompile(`/etc/(shadow|sudoers)`),
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
func (e *Executor) Execute(ctx context.Context, code, language string, timeout int, sandboxName string, trusted bool) (string, error) {
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

	if cleanupWorkDir {
		defer os.RemoveAll(workDir)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch language {
	case "bash", "":
		wrapped := sandbox.WrapCommand(cfg, []string{"bash", "-c", code}, workDir)
		cmd = exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
		cmd.Dir = workDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
	default:
		return "", fmt.Errorf("unsupported language: %s (supported: bash)", language)
	}

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
	limit := e.MaxOutputChars
	if limit <= 0 {
		limit = defaultMaxOutputChars
	}
	if len(result) > limit {
		result = result[:limit] + "\n... (output truncated)"
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
