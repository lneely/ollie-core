package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	r := strings.NewReader(`{"hooks": {"postTurn": "notify-send done"}}`)
	cfg, err := Load(r)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Hooks["postTurn"]) != 1 || cfg.Hooks["postTurn"][0] != "notify-send done" {
		t.Errorf("Expected hook 'notify-send done', got %q", cfg.Hooks["postTurn"])
	}
}

func TestLoadEmpty(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Hooks) != 0 {
		t.Errorf("Expected no hooks, got %v", cfg.Hooks)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	_, err := Load(strings.NewReader(`{bad`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestHookCmdsString(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"hooks": {"pre": "single"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks["pre"]) != 1 || cfg.Hooks["pre"][0] != "single" {
		t.Errorf("got %v, want [single]", cfg.Hooks["pre"])
	}
}

func TestHookCmdsArray(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"hooks": {"pre": ["a", "b"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks["pre"]) != 2 || cfg.Hooks["pre"][0] != "a" || cfg.Hooks["pre"][1] != "b" {
		t.Errorf("got %v, want [a b]", cfg.Hooks["pre"])
	}
}

func TestHookCmdsInvalid(t *testing.T) {
	_, err := Load(strings.NewReader(`{"hooks": {"pre": 42}}`))
	if err == nil {
		t.Error("expected error for invalid hook type")
	}
}

func TestLoadAllFields(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{
		"prompt": "be helpful",
		"trustedTools": ["execute_code"],
		"maxTokens": 4096,
		"temperature": 0.7,
		"frequencyPenalty": 0.5,
		"presencePenalty": 0.3
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Prompt != "be helpful" {
		t.Errorf("Prompt = %q", cfg.Prompt)
	}
	if len(cfg.TrustedTools) != 1 || cfg.TrustedTools[0] != "execute_code" {
		t.Errorf("TrustedTools = %v", cfg.TrustedTools)
	}
	if cfg.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d", cfg.MaxTokens)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
		t.Errorf("Temperature = %v", cfg.Temperature)
	}
	if cfg.FrequencyPenalty == nil || *cfg.FrequencyPenalty != 0.5 {
		t.Errorf("FrequencyPenalty = %v", cfg.FrequencyPenalty)
	}
	if cfg.PresencePenalty == nil || *cfg.PresencePenalty != 0.3 {
		t.Errorf("PresencePenalty = %v", cfg.PresencePenalty)
	}
}
