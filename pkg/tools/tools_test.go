package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"ollie/pkg/tools"
)

// stubServer is a minimal Server used to verify the contract.
type stubServer struct {
	name  string
	tools []tools.ToolInfo
}

func (s *stubServer) ListTools() ([]tools.ToolInfo, error) { return s.tools, nil }
func (s *stubServer) CallTool(_ context.Context, tool string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"tool":"` + tool + `"}`), nil
}

func newStub(name string, toolNames ...string) *stubServer {
	var ti []tools.ToolInfo
	for _, n := range toolNames {
		ti = append(ti, tools.ToolInfo{Name: n, Description: n + " desc"})
	}
	return &stubServer{name: name, tools: ti}
}

// checkServerContract verifies Server invariants.
func checkServerContract(t *testing.T, s tools.Server) {
	t.Helper()
	tl, err := s.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if tl == nil {
		t.Fatal("ListTools returned nil")
	}
	for _, ti := range tl {
		res, err := s.CallTool(context.Background(), ti.Name, json.RawMessage(`{}`))
		if err != nil {
			t.Errorf("CallTool(%q): %v", ti.Name, err)
		}
		if len(res) == 0 {
			t.Errorf("CallTool(%q) returned empty result", ti.Name)
		}
	}
}

// checkDispatcherContract verifies Dispatcher invariants.
func checkDispatcherContract(t *testing.T, d tools.Dispatcher, servers map[string]*stubServer) {
	t.Helper()

	for name, s := range servers {
		d.AddServer(name, s)
	}

	// GetServer round-trip
	for name := range servers {
		s, ok := d.GetServer(name)
		if !ok || s == nil {
			t.Errorf("GetServer(%q) not found after AddServer", name)
		}
	}
	if _, ok := d.GetServer("nonexistent"); ok {
		t.Error("GetServer returned true for unregistered server")
	}

	// ListTools aggregates all servers
	all, err := d.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var wantCount int
	for _, s := range servers {
		wantCount += len(s.tools)
	}
	if len(all) != wantCount {
		t.Errorf("ListTools returned %d tools, want %d", len(all), wantCount)
	}
	for _, ti := range all {
		if ti.Server == "" {
			t.Errorf("tool %q has empty Server field", ti.Name)
		}
	}

	// Dispatch routes to correct server
	for name, s := range servers {
		for _, ti := range s.tools {
			res, err := d.Dispatch(context.Background(), name, ti.Name, json.RawMessage(`{}`))
			if err != nil {
				t.Errorf("Dispatch(%q, %q): %v", name, ti.Name, err)
			}
			if len(res) == 0 {
				t.Errorf("Dispatch(%q, %q) returned empty", name, ti.Name)
			}
		}
	}

	// Dispatch to unknown server must error
	_, err = d.Dispatch(context.Background(), "nonexistent", "tool", json.RawMessage(`{}`))
	if err == nil {
		t.Error("Dispatch to unknown server should error")
	}
}

func TestStubServerContract(t *testing.T) {
	checkServerContract(t, newStub("s", "a", "b"))
}

func TestDispatcherContract(t *testing.T) {
	servers := map[string]*stubServer{
		"alpha": newStub("alpha", "tool1", "tool2"),
		"beta":  newStub("beta", "tool3"),
	}
	checkDispatcherContract(t, tools.NewDispatcher(), servers)
}

// failServer is a Server whose ListTools always errors.
type failServer struct{ stubServer }

func (f *failServer) ListTools() ([]tools.ToolInfo, error) {
	return nil, fmt.Errorf("boom")
}

func TestDispatcherListToolsError(t *testing.T) {
	d := tools.NewDispatcher()
	d.AddServer("bad", &failServer{})
	_, err := d.ListTools()
	if err == nil {
		t.Error("expected error from failing server")
	}
}

func TestNewDispatcherFunc(t *testing.T) {
	factory := tools.NewDispatcherFunc(map[string]func() tools.Server{
		"s1": func() tools.Server { return newStub("s1", "t1") },
	})
	d := factory()
	tl, err := d.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tl) != 1 {
		t.Errorf("got %d tools, want 1", len(tl))
	}
}
