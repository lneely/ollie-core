package backend

import (
	"net/http"
	"net/url"
)

// NewCopilot returns an OpenAI-compatible backend configured for the GitHub
// Copilot chat completions API. token is a short-lived Copilot bearer token.
func NewCopilot(token string) (*OpenAIBackend, error) {
	u, _ := url.Parse("https://api.githubcopilot.com")
	return &OpenAIBackend{
		name:    "copilot",
		baseURL: u,
		apiKey:  token,
		client:  &http.Client{},
		extraHeaders: map[string]string{
			"Openai-Intent": "conversation-edits",
			"User-Agent":    "opencode/local",
		},
	}, nil
}
