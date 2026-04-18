package paths

import "os"

// CfgDir returns the ollie config root from OLLIE_CFG_PATH, defaulting to ~/.config/ollie.
func CfgDir() string {
	if p := os.Getenv("OLLIE_CFG_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie"
}

// DataDir returns the ollie data root from OLLIE_DATA_PATH, defaulting to ~/.local/share/ollie.
func DataDir() string {
	if p := os.Getenv("OLLIE_DATA_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.local/share/ollie"
}
