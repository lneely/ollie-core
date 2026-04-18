package elevation

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"ollie/pkg/paths"
)

type superpowersElevator struct {
	script string
}

func newSuperpowers() *superpowersElevator {
	if _, err := exec.LookPath("superpowers"); err != nil {
		return nil
	}
	script := filepath.Join(paths.CfgDir(), "tools", "elevate_superpowers")
	if _, err := os.Stat(script); err != nil {
		return nil
	}
	return &superpowersElevator{script: script}
}

func (s *superpowersElevator) Name() string { return "superpowers" }

func (s *superpowersElevator) Run(ctx context.Context, cmd, dir string, stdout, stderr io.Writer) (int, error) {
	c := exec.CommandContext(ctx, "superpowers", "run-session", "--", "python3", s.script, cmd)
	c.Dir = dir
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
