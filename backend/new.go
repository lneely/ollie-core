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
//	OLLIE_BACKEND      ollama | openai | openrouter (default: ollama)
//	OLLIE_OLLAMA_URL   base URL for ollama (default: http://localhost:11434)
//	OLLIE_OPENAI_URL   base URL for openai-compatible backends
//	OLLIE_OPENAI_KEY   API key (required for openai)
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
	case "openai":
		return NewOpenAI(os.Getenv("OLLIE_OPENAI_URL"), os.Getenv("OLLIE_OPENAI_KEY")), nil
	default:
		return nil, fmt.Errorf("unknown OLLIE_BACKEND %q (supported: ollama, openai)", which)
	}
}
