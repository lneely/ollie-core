package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"ollie/pkg/tools"
)

// dispatchMemoryBackend implements tools.MemoryBackend by dispatching
// memory_remember and memory_recall calls through the agent's dispatcher to a
// registered MCP server (e.g. denote-mcp).
type dispatchMemoryBackend struct {
	d      tools.Dispatcher
	server string // name of the memory MCP server
}

// Remember dispatches memory_remember to the registered MCP server.
func (b *dispatchMemoryBackend) Remember(ctx context.Context, title string, tags []string, body string) (string, error) {
	args := map[string]any{
		"title": title,
		"tags":  tags,
		"body":  body,
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	result, err := b.d.Dispatch(ctx, b.server, "memory_remember", raw)
	if err != nil {
		return "", err
	}
	return extractText(result)
}

// Recall dispatches memory_recall to the registered MCP server.
func (b *dispatchMemoryBackend) Recall(ctx context.Context, query string) (string, error) {
	args := map[string]any{"query": query}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	result, err := b.d.Dispatch(ctx, b.server, "memory_recall", raw)
	if err != nil {
		return "", err
	}
	return extractText(result)
}

// extractText pulls the first text content item out of an MCP tool response.
func extractText(result json.RawMessage) (string, error) {
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &resp); err != nil || len(resp.Content) == 0 {
		return "", fmt.Errorf("unexpected response: %s", string(result))
	}
	return resp.Content[0].Text, nil
}
