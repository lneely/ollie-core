package execute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ExecutionLog records metadata about a code execution.
type ExecutionLog struct {
	Timestamp  time.Time     `json:"timestamp"`
	CodeHash   string        `json:"code_hash"`
	Language   string        `json:"language"`
	Duration   time.Duration `json:"duration"`
	Success    bool          `json:"success"`
	OutputSize int           `json:"output_size"`
	Error      string        `json:"error,omitempty"`
}

// SecurityEvent records a security-relevant event.
type SecurityEvent struct {
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Language  string    `json:"language,omitempty"`
	Details   string    `json:"details"`
}

func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

func logExecution(logDir string, log ExecutionLog) error {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(logDir, "executions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(log)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s\n", data)
	return err
}

func logSecurityEvent(logDir string, event SecurityEvent) error {
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(logDir, "security.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s\n", data)
	return err
}
