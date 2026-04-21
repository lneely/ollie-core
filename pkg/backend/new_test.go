package backend

import (
	"os"
	"path/filepath"
	"testing"
)

// setEnv sets env vars for a test and restores them on cleanup.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		old, existed := os.LookupEnv(k)
		os.Setenv(k, v)
		if existed {
			t.Cleanup(func() { os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
	}
}

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, existed := os.LookupEnv(k)
		os.Unsetenv(k)
		if existed {
			t.Cleanup(func() { os.Setenv(k, old) })
		}
	}
}

func TestNewFromEnv_DefaultOllama(t *testing.T) {
	clearEnv(t, "OLLIE_BACKEND", "OLLIE_OLLAMA_URL")
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "ollama" {
		t.Errorf("name = %q; want ollama", b.Name())
	}
}

func TestNewFromEnv_Ollama(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "ollama", "OLLIE_OLLAMA_URL": "http://myhost:11434"})
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "ollama" {
		t.Errorf("name = %q", b.Name())
	}
}

func TestNewFromEnv_OpenAI(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "openai", "OLLIE_OPENAI_URL": "https://api.openai.com", "OLLIE_OPENAI_KEY": "sk-test"})
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "openai" {
		t.Errorf("name = %q", b.Name())
	}
}

func TestNewFromEnv_OpenRouter(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "openrouter", "OLLIE_OPENAI_URL": "https://openrouter.ai/api", "OLLIE_OPENAI_KEY": "sk-or-test"})
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "openrouter" {
		t.Errorf("name = %q", b.Name())
	}
}

func TestNewFromEnv_Anthropic(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "anthropic", "OLLIE_ANTHROPIC_KEY": "sk-ant-test"})
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "anthropic" {
		t.Errorf("name = %q", b.Name())
	}
}

func TestNewFromEnv_AnthropicMissingKey(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "anthropic"})
	clearEnv(t, "OLLIE_ANTHROPIC_KEY")
	_, err := newFromEnv("/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestNewFromEnv_Copilot(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "copilot", "OLLIE_COPILOT_TOKEN": "tok"})
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "copilot" {
		t.Errorf("name = %q", b.Name())
	}
}

func TestNewFromEnv_CopilotMissingToken(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "copilot"})
	clearEnv(t, "OLLIE_COPILOT_TOKEN")
	_, err := newFromEnv("/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNewFromEnv_Kiro(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "kiro", "OLLIE_KIRO_TOKEN": "fake"})
	b, err := newFromEnv("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "kiro" {
		t.Errorf("name = %q", b.Name())
	}
}

func TestNewFromEnv_Unknown(t *testing.T) {
	setEnv(t, map[string]string{"OLLIE_BACKEND": "bogus"})
	_, err := newFromEnv("/nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestNewOllama_InvalidURL(t *testing.T) {
	_, err := NewOllama("://bad")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNewOpenAI_InvalidURL(t *testing.T) {
	_, err := NewOpenAI("openai", "://bad", "k")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNewFromEnv_EnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	os.WriteFile(path, []byte("OLLIE_BACKEND=anthropic\nOLLIE_ANTHROPIC_KEY=from-file\n"), 0644)

	clearEnv(t, "OLLIE_BACKEND", "OLLIE_ANTHROPIC_KEY")
	b, err := newFromEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "anthropic" {
		t.Errorf("name = %q; want anthropic (from env file)", b.Name())
	}
}

func TestNewFromEnv_EnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	os.WriteFile(path, []byte("OLLIE_BACKEND=anthropic\n"), 0644)

	setEnv(t, map[string]string{"OLLIE_BACKEND": "ollama"})
	clearEnv(t, "OLLIE_OLLAMA_URL")
	b, err := newFromEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "ollama" {
		t.Errorf("name = %q; want ollama (env overrides file)", b.Name())
	}
}

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
