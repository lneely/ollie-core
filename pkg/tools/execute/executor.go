// Package execute provides the builtin execute_code, execute_tool, and
// execute_pipe tools. The Server struct is the shared sandbox runner.
package execute

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"ollie/internal/sandbox"
)

// Server runs code in a sandboxed environment.
type Server struct {
	// Confirm is an optional function called before executing sensitive operations.
	// If it returns false, the operation is denied.
	Confirm func(string) bool

	// workdir is the working directory for sandboxed commands. If empty,
	// the process working directory is used.
	wdMu    sync.RWMutex
	workdir string

	// rate limiting state (per-Server)
	rateLimitMu        sync.Mutex
	validationFailures int
	lastFailure        time.Time
	blockedUntil       time.Time
}

// New creates a new Server with the given working directory.
func New(workdir string) *Server { return &Server{workdir: workdir} }

// SetWorkDir updates the working directory used for subsequent command executions.
func (e *Server) SetWorkDir(dir string) {
	e.wdMu.Lock()
	e.workdir = dir
	e.wdMu.Unlock()
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

func (e *Server) checkRateLimit() error {
	e.rateLimitMu.Lock()
	defer e.rateLimitMu.Unlock()

	now := time.Now()
	if now.Before(e.blockedUntil) {
		remaining := e.blockedUntil.Sub(now).Round(time.Second)
		return fmt.Errorf("rate limited: too many validation failures, blocked for %v", remaining)
	}
	return nil
}

func (e *Server) recordValidationFailure() {
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
	}
}

// ValidateCode checks code against dangerous patterns.
func (e *Server) ValidateCode(code string) error {
	if err := e.checkRateLimit(); err != nil {
		return err
	}

	normalized := strings.ToLower(code)
	normalized = whitespacePattern.ReplaceAllString(normalized, " ")

	for _, pattern := range dangerousPatterns {
		if pattern.MatchString(normalized) {
			e.recordValidationFailure()
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
func (e *Server) Execute(ctx context.Context, code, language string, timeout int, sandboxName string, trusted bool) (string, error) {
	if timeout <= 0 {
		timeout = 30
	}

	if !trusted {
		if err := e.ValidateCode(code); err != nil {
			return "", err
		}
	}

	cfg, err := loadLayeredConfig(sandboxName)
	if err != nil {
		return "", err
	}

	e.wdMu.RLock()
	workDir := e.workdir
	e.wdMu.RUnlock()
	if workDir == "" {
		workDir, _ = os.Getwd()
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

	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("execution timeout after %d seconds", timeout)
	}
	if err != nil {
		return string(output), fmt.Errorf("execution failed: %v\nOutput: %s", err, string(output))
	}
	return string(output), nil
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
