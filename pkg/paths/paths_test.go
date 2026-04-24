package paths

import (
	"os"
	"testing"
)

func TestCfgDirFromEnv(t *testing.T) {
	t.Setenv("OLLIE_CFG_PATH", "/custom/cfg")
	if got := CfgDir(); got != "/custom/cfg" {
		t.Errorf("CfgDir() = %q; want /custom/cfg", got)
	}
}

func TestCfgDirDefault(t *testing.T) {
	t.Setenv("OLLIE_CFG_PATH", "")
	home, _ := os.UserHomeDir()
	if got := CfgDir(); got != home+"/.config/ollie" {
		t.Errorf("CfgDir() = %q; want %s/.config/ollie", got, home)
	}
}

func TestDataDirFromEnv(t *testing.T) {
	t.Setenv("OLLIE_DATA_PATH", "/custom/data")
	if got := DataDir(); got != "/custom/data" {
		t.Errorf("DataDir() = %q; want /custom/data", got)
	}
}

func TestDataDirDefault(t *testing.T) {
	t.Setenv("OLLIE_DATA_PATH", "")
	home, _ := os.UserHomeDir()
	if got := DataDir(); got != home+"/.local/share/ollie" {
		t.Errorf("DataDir() = %q; want %s/.local/share/ollie", got, home)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct{ in, want string }{
		{"~", home},
		{"~/src", home + "/src"},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~user/foo", "~user/foo"}, // only bare ~ is expanded
		{"", ""},
	}
	for _, tt := range tests {
		if got := ExpandHome(tt.in); got != tt.want {
			t.Errorf("ExpandHome(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}
