package config

import (
	"encoding/json"
	"io"
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

// Prompt holds the agent prompt. It unmarshals from either a JSON string
// (treated as literal text, with the existing resolvePrompt semantics) or
// an array of strings (each element is a shell command whose stdout is
// concatenated with newlines).
type Prompt struct {
	Value []string // len==1 for a plain string; len>1 for command list
	IsExec bool    // true when the value came from an array (execute mode)
}

func (p *Prompt) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		p.Value = []string{s}
		p.IsExec = false
		return nil
	}
	var ss []string
	if err := json.Unmarshal(data, &ss); err != nil {
		return err
	}
	p.Value = ss
	p.IsExec = true
	return nil
}

func (p Prompt) MarshalJSON() ([]byte, error) {
	if p.IsExec {
		return json.Marshal(p.Value)
	}
	if len(p.Value) == 1 {
		return json.Marshal(p.Value[0])
	}
	return json.Marshal(p.Value)
}

type Config struct {
	Hooks               map[string]HookCmds `json:"hooks,omitempty"`
	Prompt              Prompt              `json:"prompt,omitempty"`
	Tools               *bool               `json:"tools,omitempty"`
	TrustedTools        []string            `json:"trustedTools,omitempty"`
	AllowExecutors      []string            `json:"allowExecutors,omitempty"`
	AllowTools          []string            `json:"allowTools,omitempty"`
	MaxTokens           int                 `json:"maxTokens,omitempty"`
	MaxCompletionTokens int                 `json:"maxCompletionTokens,omitempty"`
	// MaxSteps caps the number of tool-call rounds per turn. 0 means unlimited.
	MaxSteps          int      `json:"maxSteps,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"topP,omitempty"`
	TopK              *int     `json:"topK,omitempty"`
	MinP              *float64 `json:"minP,omitempty"`
	TopA              *float64 `json:"topA,omitempty"`
	FrequencyPenalty  *float64 `json:"frequencyPenalty,omitempty"`
	PresencePenalty   *float64 `json:"presencePenalty,omitempty"`
	RepetitionPenalty *float64 `json:"repetitionPenalty,omitempty"`
	Reasoning         int      `json:"reasoning,omitempty"`
	ReasoningEffort   string   `json:"reasoningEffort,omitempty"`
	IncludeReasoning  *bool    `json:"includeReasoning,omitempty"`
	ResponseFormat    string   `json:"responseFormat,omitempty"`
	Stop              []string `json:"stop,omitempty"`
	Verbosity         string   `json:"verbosity,omitempty"`
}

// Load parses a Config from r.
func Load(r io.Reader) (*Config, error) {
	var cfg Config
	if err := json.NewDecoder(r).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ToolsEnabled reports whether tool use is enabled. Defaults to true when
// the field is omitted from the config.
func (c *Config) ToolsEnabled() bool {
	return c.Tools == nil || *c.Tools
}
