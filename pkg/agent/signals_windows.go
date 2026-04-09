//go:build windows

package agent

import "os"

const haveSIGTERM = false

var sigTERM = os.Signal(nil)
