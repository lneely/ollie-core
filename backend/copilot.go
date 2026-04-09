package backend

import "net/http"

// NewCopilot returns an OpenAI-compatible backend configured for the GitHub
// Copilot chat completions API. token is a short-lived Copilot bearer token.
//
// The required Copilot-specific headers (Openai-Intent, User-Agent) are
// injected on every request via the OpenAIBackend's extraHeaders mechanism.
func NewCopilot(token string) *OpenAIBackend {
	return &OpenAIBackend{
		baseURL: "https://api.githubcopilot.com",
		apiKey:  token,
		client:  &http.Client{},
		extraHeaders: map[string]string{
			"Openai-Intent": "conversation-edits",
			"User-Agent":    "opencode/local",
		},
	}
}
