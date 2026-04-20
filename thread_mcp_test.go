package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestSpawn_WithMCP_RegistersToolsPrefixed verifies the spawn → MCP wiring
// in isolation, without a real LLM. It covers the exact code path whose
// failure produces the "sub-thread got 0 tools" symptom we just fixed in
// prod (mcp_http.go allowed_tools filter drift):
//
//   1. Parent has an HTTP MCP named "catalog-mcp" in its config.
//   2. We spawn a sub-thread with MCPNames=["catalog-mcp"] and
//      tools=["send"] (core tool; MCP tools get auto-added).
//   3. Post-spawn we assert:
//      - The thread registered the MCP's two tools with a "catalog-mcp_"
//        prefix (matches how registration writes them).
//      - Thread.Tools contains the prefixed names (so spawn/allowlist
//        tracking sees them) and the original "send".
//      - Calling the registered tool round-trips through the mock server.
//
// No Fireworks, no subprocesses — a single httptest.Server and the
// ThreadManager code path we care about.
func TestSpawn_WithMCP_RegistersToolsPrefixed(t *testing.T) {
	var callsReceived atomic.Int64

	// Minimal MCP Streamable-HTTP server: initialize + tools/list +
	// tools/call. Responds as plain JSON (not SSE) — simpler, and the
	// core client accepts both.
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// Notifications (no ID): 202, no body.
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "mock-catalog", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": "ping", "description": "returns pong", "inputSchema": map[string]any{"type": "object"}},
				{"name": "echo", "description": "echoes input", "inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				}},
			}}
		case "tools/call":
			callsReceived.Add(1)
			var p struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			text := "pong"
			if p.Name == "echo" {
				if t, ok := p.Arguments["text"].(string); ok {
					text = t
				}
			}
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Parent thinker wired with the MCP config + a real registry (spawn
	// skips the MCP block entirely when registry is nil).
	thinker := newTestThinker()
	defer thinker.Stop()
	thinker.registry = NewToolRegistry("test")
	thinker.config = &Config{
		Directive: "test parent",
		MCPServers: []MCPServerConfig{
			{Name: "catalog-mcp", Transport: "http", URL: srv.URL + "/mcp"},
		},
	}

	err := thinker.threads.SpawnWithOpts("worker", "worker directive", []string{"send"}, SpawnOpts{
		MCPNames: []string{"catalog-mcp"},
		ParentID: "main",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Find the spawned thread.
	var thread *Thread
	for _, th := range thinker.threads.List() {
		if th.ID == "worker" {
			// thinker.threads.List() returns a snapshot type; reach in
			// through the manager for the concrete Thread.
		}
	}
	thinker.threads.mu.RLock()
	thread = thinker.threads.threads["worker"]
	thinker.threads.mu.RUnlock()
	if thread == nil {
		t.Fatal("thread not in manager")
	}

	// Tool names should be namespaced with the MCP server name.
	for _, want := range []string{"catalog-mcp_ping", "catalog-mcp_echo", "send"} {
		if !thread.Tools[want] {
			t.Errorf("thread.Tools missing %q; have: %v", want, keys(thread.Tools))
		}
	}

	// The registry used by the sub-thread must actually resolve and
	// dispatch the registered tool. Look up echo and call it.
	threadThinker := thread.Thinker
	if threadThinker == nil {
		t.Fatal("thread has no thinker")
	}
	def := threadThinker.registry.Get("catalog-mcp_echo")
	if def == nil {
		t.Fatal("registry lookup for catalog-mcp_echo failed")
	}
	if !strings.Contains(def.Description, "[catalog-mcp]") {
		t.Errorf("registered description missing MCP-name tag: %q", def.Description)
	}
	resp := def.Handler(map[string]string{"text": "hi"})
	if !strings.Contains(resp.Text, "hi") {
		t.Errorf("expected echoed text in result, got %q", resp.Text)
	}
	if callsReceived.Load() != 1 {
		t.Errorf("expected 1 tools/call to the mock server, got %d", callsReceived.Load())
	}
}

// TestSpawn_WithMCP_UnknownNameLogsAndContinues verifies that asking for
// an MCP the parent doesn't know about doesn't blow up the spawn — it
// logs and continues (the runtime behaviour documented at
// thread.go:346-349). Regression guard: we don't want a typo in a
// saved sub-thread's mcp_names list to crash the parent thinker.
func TestSpawn_WithMCP_UnknownNameLogsAndContinues(t *testing.T) {
	thinker := newTestThinker()
	defer thinker.Stop()
	thinker.registry = NewToolRegistry("test")
	thinker.config = &Config{Directive: "test parent"}

	err := thinker.threads.SpawnWithOpts("worker", "d", []string{"send"}, SpawnOpts{
		MCPNames: []string{"not-registered"},
		ParentID: "main",
	})
	if err != nil {
		t.Fatalf("spawn should have succeeded despite unknown MCP: %v", err)
	}
	thinker.threads.mu.RLock()
	thread := thinker.threads.threads["worker"]
	thinker.threads.mu.RUnlock()
	if thread == nil {
		t.Fatal("thread not created")
	}
	// No MCP tools should have been registered.
	for name := range thread.Tools {
		if strings.HasPrefix(name, "not-registered_") {
			t.Errorf("unexpected tool %q on thread — unknown MCP should be skipped", name)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
