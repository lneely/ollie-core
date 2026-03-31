package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Message is a JSON-RPC 2.0 message.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  interface{}     `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// Client is a JSON-RPC client over a pair of streams (typically stdin/stdout
// of a subprocess).
type Client struct {
	mu     sync.Mutex
	w      io.Writer
	sc     *bufio.Scanner
	nextID int
}

// NewClient creates a Client from a reader and writer.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{w: w, sc: bufio.NewScanner(r)}
}

// Call sends a JSON-RPC request and returns the result.
func (c *Client) Call(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID

	msg := Message{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c.w, "%s\n", data); err != nil {
		return nil, err
	}

	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	var resp Message
	if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}
