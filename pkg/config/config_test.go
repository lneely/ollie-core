package config

import (
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	content := `{"hooks": {"stop": "notify-send done"}}`

	tmpfile, err := os.CreateTemp("", "config*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Hooks["stop"] != "notify-send done" {
		t.Errorf("Expected hook 'notify-send done', got %q", cfg.Hooks["stop"])
	}
}

func TestLoadEmpty(t *testing.T) {
	content := `{}`

	tmpfile, err := os.CreateTemp("", "config*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Hooks) != 0 {
		t.Errorf("Expected no hooks, got %v", cfg.Hooks)
	}
}
