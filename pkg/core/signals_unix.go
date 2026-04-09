//go:build !windows

package core

import "syscall"

const (
	haveSIGTERM = true
	sigTERM     = syscall.SIGTERM
)
