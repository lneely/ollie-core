package backend

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadEnvFile reads KEY=VALUE pairs from path and sets any key that is not
// already present in the environment. Lines beginning with # and blank lines
// are ignored. Errors opening the file are silently ignored (file is optional).
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		v = strings.TrimSpace(v)
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// New constructs a Backend from environment variables.
// Values are loaded from ~/.config/ollie/env before consulting the environment,
// so the file acts as a default; variables already set in the environment take
// precedence.
//
//	OLLIE_BACKEND        ollama | openai | openrouter | anthropic | copilot | kiro (default: ollama)
//	OLLIE_OLLAMA_URL     base URL for ollama (default: http://localhost:11434)
//	OLLIE_OPENAI_URL     base URL for openai-compatible backends
//	OLLIE_OPENAI_KEY     API key (required for openai/openrouter)
//	OLLIE_ANTHROPIC_KEY  API key (required for anthropic)
//	OLLIE_COPILOT_TOKEN  bearer token (required for copilot)
//	OLLIE_KIRO_TOKEN     bearer token or sqlite:// URL for kiro/codewhisperer
//	                     (default: sqlite path auto-detected from Kiro CLI data dir)
func New() (Backend, error) {
	home, _ := os.UserHomeDir()
	loadEnvFile(home + "/.config/ollie/env")

	which := os.Getenv("OLLIE_BACKEND")
	if which == "" {
		which = "ollama"
	}

	switch which {
	case "ollama":
		return NewOllama(os.Getenv("OLLIE_OLLAMA_URL")), nil
	case "openai", "openrouter":
		url := os.Getenv("OLLIE_OPENAI_URL")
		key := os.Getenv("OLLIE_OPENAI_KEY")
		return NewOpenAI(openAIName(which, url), url, key), nil
	case "anthropic":
		key := os.Getenv("OLLIE_ANTHROPIC_KEY")
		if key == "" {
			return nil, fmt.Errorf("OLLIE_ANTHROPIC_KEY is required for anthropic backend")
		}
		return NewAnthropic(key), nil
	case "copilot":
		token := os.Getenv("OLLIE_COPILOT_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("OLLIE_COPILOT_TOKEN is required for copilot backend")
		}
		return NewCopilot(token), nil
	case "kiro", "codewhisperer":
		return NewCodeWhisperer(os.Getenv("OLLIE_KIRO_TOKEN"))
	default:
		return nil, fmt.Errorf("unknown OLLIE_BACKEND %q (supported: ollama, openai, openrouter, anthropic, copilot, kiro)", which)
	}
}

// Backends returns the list of supported backend names.
func Backends() []string {
	return []string{"ollama", "openai", "openrouter", "anthropic", "copilot", "kiro"}
}

// openAIName derives a short backend label from the OLLIE_BACKEND value and
// the base URL, so openai-compatible endpoints self-identify correctly.
func openAIName(which, url string) string {
	url = strings.ToLower(url)
	switch {
	case strings.Contains(url, "openrouter"):
		return "openrouter"
	case strings.Contains(url, "together"):
		return "together"
	case strings.Contains(url, "groq"):
		return "groq"
	case strings.Contains(url, "mistral"):
		return "mistral"
	case strings.Contains(url, "anthropic"):
		return "anthropic"
	case strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1"):
		return "local"
	default:
		return which
	}
}
