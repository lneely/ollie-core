package elevation

import (
	"context"
	"io"
	"os/exec"
)

// Elevator runs commands with elevated privileges outside the sandbox.
// Returns the exit code; a non-zero exit code is not itself an error.
type Elevator interface {
	Run(ctx context.Context, cmd, dir string, stdout, stderr io.Writer) (int, error)
	Name() string
}

// Detect returns the best available Elevator, or nil if none is configured.
func Detect() Elevator {
	if sp := newSuperpowers(); sp != nil {
		return sp
	}
	return nil
}

// Available reports whether a named elevator binary is present in PATH.
func Available(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
