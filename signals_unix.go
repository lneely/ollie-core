//go:build !windows

package main

import "syscall"

const (
	haveSIGTERM = true
	SIGTERM     = syscall.SIGTERM
)
