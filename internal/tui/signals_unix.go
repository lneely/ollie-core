//go:build !windows

package tui

import "syscall"

const (
	haveSIGTERM = true
	SIGTERM     = syscall.SIGTERM
)
