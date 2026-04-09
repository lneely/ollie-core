//go:build windows

package core

import "os"

const haveSIGTERM = false

var sigTERM = os.Signal(nil)
