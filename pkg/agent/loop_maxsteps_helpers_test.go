package agent

import (
	"context"
	"sync/atomic"

	"ollie/pkg/backend"
)

// mockResponse defines a canned response for sequentialStream.
type mockResponse struct {
	content    string
	toolCalls  []backend.ToolCall
	stopReason string
}

// sequentialStream returns a respond function that plays back responses in order.
func sequentialStream(responses []mockResponse) func(context.Context, []backend.Message, []backend.Tool, backend.GenerationParams) (<-chan backend.StreamEvent, error) {
	var n int32
	return func(_ context.Context, _ []backend.Message, _ []backend.Tool, _ backend.GenerationParams) (<-chan backend.StreamEvent, error) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(responses) {
			return textStream("done"), nil
		}
		r := responses[i]
		ch := make(chan backend.StreamEvent, 1)
		ch <- backend.StreamEvent{
			Content:    r.content,
			ToolCalls:  r.toolCalls,
			Done:       true,
			StopReason: r.stopReason,
		}
		close(ch)
		return ch, nil
	}
}

// simpleState is a minimal state implementation for direct run() calls.
type simpleState struct {
	msgs []backend.Message
}

func newState() *simpleState {
	return &simpleState{}
}

func (s *simpleState) history() []backend.Message {
	return s.msgs
}

func (s *simpleState) update(msg backend.Message, results []toolResult) {
	s.msgs = append(s.msgs, msg)
	for _, r := range results {
		s.msgs = append(s.msgs, backend.Message{
			Role:       "tool",
			Content:    r.Content,
			ToolCallID: r.ToolCallID,
		})
	}
}
