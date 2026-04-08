//go:build windows

package main

import "os"

const haveSIGTERM = false

var SIGTERM = os.Signal(nil)
