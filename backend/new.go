package backend

import (
	"fmt"
	"os"
)

// New constructs a Backend from environment variables.
//
//	OLLIE_BACKEND   ollama | openai | openrouter (default: ollama)
//	OLLIE_API_URL   override base URL for the selected backend
//	OLLIE_API_KEY   API key (required for openai / openrouter)
func New() (Backend, error) {
	which := os.Getenv("OLLIE_BACKEND")
	if which == "" {
		which = "ollama"
	}

	apiURL := os.Getenv("OLLIE_API_URL")
	apiKey := os.Getenv("OLLIE_API_KEY")

	switch which {
	case "ollama":
		return NewOllama(apiURL), nil
	case "openai":
		return NewOpenAI(apiURL, apiKey), nil
	case "openrouter":
		url := apiURL
		if url == "" {
			url = "https://openrouter.ai/api"
		}
		return NewOpenAI(url, apiKey), nil
	default:
		return nil, fmt.Errorf("unknown OLLIE_BACKEND %q (supported: ollama, openai, openrouter)", which)
	}
}
