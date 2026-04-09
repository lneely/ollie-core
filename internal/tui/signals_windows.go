//go:build windows

package tui

import "os"

const haveSIGTERM = false

var SIGTERM = os.Signal(nil)
