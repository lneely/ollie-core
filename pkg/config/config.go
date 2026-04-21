package config

import (
	"encoding/json"
	"os"
)

// HookCmds holds one or more shell commands for a hook. It unmarshals from
// either a JSON string ("cmd") or array (["cmd1","cmd2"]).
type HookCmds []string

func (h *HookCmds) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*h = HookCmds{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(data, &ss); err != nil {
		return err
	}
	*h = HookCmds(ss)
	return nil
}

type Config struct {
	Hooks            map[string]HookCmds     `json:"hooks,omitempty"`
	Prompt           string                  `json:"prompt,omitempty"`
	TrustedTools     []string                `json:"trustedTools,omitempty"`
	MaxTokens        int                     `json:"maxTokens,omitempty"`
	Temperature      *float64                `json:"temperature,omitempty"`
	FrequencyPenalty *float64                `json:"frequencyPenalty,omitempty"`
	PresencePenalty  *float64                `json:"presencePenalty,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
