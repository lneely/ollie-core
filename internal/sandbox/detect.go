package sandbox

import (
	"os/exec"
	"sync"
)

var (
	available     bool
	availableOnce sync.Once
)

// isAvailable checks if landrun is available on the system.
// The result is cached after the first call for performance.
func isAvailable() bool {
	availableOnce.Do(func() {
		_, err := exec.LookPath("landrun")
		available = (err == nil)
	})
	return available
}
