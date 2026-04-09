//go:build !windows

package agent

import "syscall"

const (
	haveSIGTERM = true
	sigTERM     = syscall.SIGTERM
)
