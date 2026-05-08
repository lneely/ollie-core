package env

import (
	"os"
	"strings"
	"testing"
)

func TestEnsureDefaults(t *testing.T) {
	// Clear managed vars so defaults apply.
	for _, k := range managed {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	EnsureDefaults()
	for _, k := range []string{"OLLIE", "OLLIE_TOOLS_PATH", "OLLIE_SKILLS_PATH", "OLLIE_PROMPTS_PATH", "OLLIE_MEMORY_PATH", "OLLIE_TMP_PATH", "OLLIE_TRANSCRIPT_PATH"} {
		if v := os.Getenv(k); v == "" {
			t.Errorf("%s not set after EnsureDefaults", k)
		}
	}
}

func TestEnsureDefaultsNoOverwrite(t *testing.T) {
	t.Setenv("OLLIE", "/custom")
	EnsureDefaults()
	if v := os.Getenv("OLLIE"); v != "/custom" {
		t.Errorf("OLLIE = %q, want /custom (should not overwrite)", v)
	}
}

func TestSetGet(t *testing.T) {
	t.Setenv("OLLIE_TEST_VAR", "")
	Set("OLLIE_TEST_VAR", "hello")
	if got := Get("OLLIE_TEST_VAR"); got != "hello" {
		t.Errorf("Get = %q, want %q", got, "hello")
	}
}

func TestAll(t *testing.T) {
	t.Setenv("OLLIE_TEST_ALL", "val")
	m := All()
	if m["OLLIE_TEST_ALL"] != "val" {
		t.Errorf("All() missing OLLIE_TEST_ALL")
	}
}

func TestFormat(t *testing.T) {
	t.Setenv("OLLIE_TEST_FMT", "xyz")
	out := string(Format())
	if !strings.Contains(out, "OLLIE_TEST_FMT=xyz") {
		t.Errorf("Format() missing expected var, got:\n%s", out)
	}
	// Every line should be NAME=VALUE\n
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Errorf("malformed line: %q", line)
		}
	}
}
