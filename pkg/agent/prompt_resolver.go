package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ollie/pkg/config"
)

// resolvePrompt interprets a Prompt from an agent config.
//
// If the prompt was parsed from a JSON array, each element is executed as a
// shell command and the combined stdout (joined by newlines) is returned.
//
// If the prompt was parsed from a JSON string, the existing single-string
// semantics apply: environment variables are expanded, then:
//   - If the string contains a newline, it is treated as literal text.
//   - If the string starts with '!', the rest is executed as a shell command.
//   - If the expanded string names an existing file, the file is read.
//   - Otherwise the string is used as-is.
func resolvePrompt(p config.Prompt, cwd string) (string, error) {
	if len(p.Value) == 0 {
		return "", nil
	}
	if p.IsExec {
		return resolveExecPrompt(p.Value, cwd)
	}
	return resolveStringPrompt(p.Value[0], cwd)
}

func resolveExecPrompt(cmds []string, cwd string) (string, error) {
	var parts []string
	for _, cmdStr := range cmds {
		cmdStr = strings.TrimSpace(cmdStr)
		if cmdStr == "" {
			continue
		}
		// Entries containing a newline are literal text, not commands.
		if strings.Contains(cmdStr, "\n") {
			parts = append(parts, cmdStr)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		if cwd != "" {
			cmd.Dir = cwd
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			return "", fmt.Errorf("prompt command %q failed: %v: %s", cmdStr, err, stderr.String())
		}
		if out := strings.TrimRight(stdout.String(), "\n"); out != "" {
			parts = append(parts, out)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func resolveStringPrompt(prompt, cwd string) (string, error) {
	if prompt == "" {
		return "", nil
	}
	expanded := os.Expand(prompt, func(key string) string {
		if val := os.Getenv(key); val != "" {
			return val
		}
		return ""
	})
	expanded = strings.TrimSpace(expanded)
	if expanded == "" {
		return "", nil
	}
	if strings.Contains(expanded, "\n") {
		return expanded, nil
	}
	if strings.HasPrefix(expanded, "!") {
		cmdStr := strings.TrimPrefix(expanded, "!")
		cmdStr = strings.TrimSpace(cmdStr)
		if cmdStr == "" {
			return "", nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		if cwd != "" {
			cmd.Dir = cwd
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("prompt command failed: %v: %s", err, stderr.String())
		}
		return strings.TrimRight(stdout.String(), "\n"), nil
	}
	path := expanded
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read prompt file: %w", err)
		}
		return strings.TrimRight(string(data), "\n"), nil
	}
	return expanded, nil
}
