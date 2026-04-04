package config

import (
	"encoding/json"
	"os"
)

type Config struct {
	MCPServers   map[string]ServerConfig `json:"mcpServers,omitempty"`
	Hooks        map[string]string       `json:"hooks,omitempty"`
	Prompt       string                  `json:"prompt,omitempty"`
	TrustedTools []string                `json:"trustedTools,omitempty"`
}

type ServerConfig struct {
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
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
