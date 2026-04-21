package backend

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAIName(t *testing.T) {
	tests := []struct {
		which, url, want string
	}{
		{"openai", "https://openrouter.ai/api", "openrouter"},
		{"openai", "https://api.together.xyz", "together"},
		{"openai", "https://api.groq.com", "groq"},
		{"openai", "https://api.mistral.ai", "mistral"},
		{"openai", "https://api.anthropic.com", "anthropic"},
		{"openai", "http://localhost:8080", "local"},
		{"openai", "http://127.0.0.1:1234", "local"},
		{"openai", "https://api.openai.com", "openai"},
		{"openrouter", "", "openrouter"},
		{"openai", "", "openai"},
		// case insensitive
		{"openai", "https://API.GROQ.COM", "groq"},
	}
	for _, tt := range tests {
		if got := openAIName(tt.which, tt.url); got != tt.want {
			t.Errorf("openAIName(%q, %q) = %q; want %q", tt.which, tt.url, got, tt.want)
		}
	}
}

func TestBackends(t *testing.T) {
	bs := Backends()
	if len(bs) != 6 {
		t.Fatalf("len = %d; want 6", len(bs))
	}
	want := map[string]bool{"ollama": true, "openai": true, "openrouter": true, "anthropic": true, "copilot": true, "kiro": true}
	for _, b := range bs {
		if !want[b] {
			t.Errorf("unexpected backend %q", b)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	os.WriteFile(path, []byte(
		"# comment\n"+
			"\n"+
			"FOO_TEST_LOAD=bar\n"+
			"BAZ_TEST_LOAD=qux # inline comment\n"+
			"NOVAL\n"+
			"  SPACED_TEST_LOAD = trimmed \n",
	), 0644)

	// Clear any existing values.
	os.Unsetenv("FOO_TEST_LOAD")
	os.Unsetenv("BAZ_TEST_LOAD")
	os.Unsetenv("SPACED_TEST_LOAD")

	loadEnvFile(path)

	if v := os.Getenv("FOO_TEST_LOAD"); v != "bar" {
		t.Errorf("FOO_TEST_LOAD = %q; want bar", v)
	}
	if v := os.Getenv("BAZ_TEST_LOAD"); v != "qux" {
		t.Errorf("BAZ_TEST_LOAD = %q; want qux", v)
	}
	if v := os.Getenv("SPACED_TEST_LOAD"); v != "trimmed" {
		t.Errorf("SPACED_TEST_LOAD = %q; want trimmed", v)
	}

	// Cleanup.
	os.Unsetenv("FOO_TEST_LOAD")
	os.Unsetenv("BAZ_TEST_LOAD")
	os.Unsetenv("SPACED_TEST_LOAD")
}

func TestLoadEnvFile_NoOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	os.WriteFile(path, []byte("EXISTING_TEST_LOAD=new\n"), 0644)

	os.Setenv("EXISTING_TEST_LOAD", "old")
	defer os.Unsetenv("EXISTING_TEST_LOAD")

	loadEnvFile(path)

	if v := os.Getenv("EXISTING_TEST_LOAD"); v != "old" {
		t.Errorf("EXISTING_TEST_LOAD = %q; want old (should not override)", v)
	}
}

func TestLoadEnvFile_Missing(t *testing.T) {
	// Should not panic on missing file.
	loadEnvFile("/nonexistent/path/env")
}
