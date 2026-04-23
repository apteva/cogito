package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestIntegration_MinimaxRepeatedSpawn tries to reproduce the Fireworks
// /MiniMax upstream stall we saw in prod where the streaming body falls
// silent mid-response after 2-3 successful spawn cycles.
//
// The test runs main against a real Fireworks + minimax-m2p7 endpoint,
// has it repeatedly spawn → kill → spawn a worker thread, and watches
// for any provider.Chat invocation that doesn't return within a
// generous per-call budget. If one does, ErrStreamIdleTimeout will
// fire (60s default, overridable), the request IDs that came back in
// the response headers will be in the log, and the test fails loudly
// with the captured evidence so we can file it with Fireworks support.
//
// Gate: FIREWORKS_API_KEY + RUN_MINIMAX_STALL_TEST=1. Budget: up to 10
// minutes (MINIMAX_STALL_CYCLES=N overrides the cycle count, default 5).
func TestIntegration_MinimaxRepeatedSpawn(t *testing.T) {
	apiKey := getAPIKey(t)
	if os.Getenv("RUN_MINIMAX_STALL_TEST") == "" {
		t.Skip("set RUN_MINIMAX_STALL_TEST=1 to run the minimax stall repro")
	}
	cycles := 5
	if v := os.Getenv("MINIMAX_STALL_CYCLES"); v != "" {
		var n int
		if _, err := fmtSscan(v, &n); err == nil && n > 0 {
			cycles = n
		}
	}

	model := os.Getenv("MINIMAX_STALL_MODEL")
	if model == "" {
		model = "accounts/fireworks/models/minimax-m2p7"
	}

	// Minimal MCP to give each spawned worker something real to touch
	// so the LLM-side behaviour matches prod (where spawned threads
	// have tools to call). Single `ping` tool.
	var pingCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
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
				"serverInfo":      map[string]string{"name": "mock", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name": "ping", "description": "returns ok",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			pingCalls.Add(1)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()

	directive := `You drive a loop of spawning short-lived workers. On every turn:
  - If no worker is running, spawn ONE with:
      [[spawn id="w" mcp="mock" tools="mock_ping,done" directive="Call mock_ping once. Then call done. Nothing else."]]
  - Once the worker reports done, spawn the next one with the SAME id "w".
Continue until the operator stops you. Keep responses terse.`

	mcpCfg := []MCPServerConfig{{Name: "mock", Transport: "http", URL: srv.URL + "/mcp"}}
	// Note: httptest serves at /; our handler reads any path. Use srv.URL directly.
	mcpCfg[0].URL = srv.URL

	providers := []ProviderConfig{{
		Name:    "fireworks",
		Default: true,
		Models: map[string]string{
			"large":  model,
			"medium": model,
			"small":  model,
		},
	}}

	thinker := newScenarioThinker(t, apiKey, directive, mcpCfg, providers)
	defer thinker.Stop()

	// Observer: track every llm.done + llm.error + the NEW provider.Chat
	// logs in console. We only interpret telemetry here — the stall
	// itself will surface as an llm.error with ErrStreamIdleTimeout.
	var (
		mainCalls      atomic.Int64
		workerSpawns   atomic.Int64
		workerDones    atomic.Int64
		stalls         atomic.Int64
		totalPrompt    atomic.Int64
		totalCompleted atomic.Int64
		lastErr        atomic.Value
	)
	obs := thinker.bus.SubscribeAll("stall-repro-observer", 500)
	var wg sync.WaitGroup
	wg.Add(1)
	stopObs := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case ev := <-obs.C:
				switch ev.Type {
				case EventThinkDone:
					if ev.From == "main" {
						mainCalls.Add(1)
					}
					totalPrompt.Add(int64(ev.Usage.PromptTokens))
					totalCompleted.Add(int64(ev.Usage.CompletionTokens))
				case "thread.spawn":
					workerSpawns.Add(1)
				case "thread.done":
					workerDones.Add(1)
				case EventThinkError:
					if ev.Error != nil {
						errStr := ev.Error.Error()
						if strings.Contains(errStr, "stream idle timeout") ||
							strings.Contains(errStr, "provider went silent") {
							stalls.Add(1)
						}
						lastErr.Store(errStr)
					}
				}
			case <-stopObs:
				return
			}
		}
	}()

	go thinker.Run()
	thinker.InjectConsole("Begin the spawn loop.")

	// Wait for the target number of completed worker cycles OR a
	// stall. Either outcome is a signal — a stall wins (test fails
	// with the evidence), N clean cycles pass the test.
	deadline := time.Now().Add(10 * time.Minute)
	lastProgress := time.Now()
	var lastWorkerDones int64
	for time.Now().Before(deadline) {
		done := workerDones.Load()
		if done > lastWorkerDones {
			lastWorkerDones = done
			lastProgress = time.Now()
		}
		if stalls.Load() > 0 {
			break
		}
		if int(done) >= cycles {
			break
		}
		if time.Since(lastProgress) > 3*time.Minute {
			// Main sitting idle for 3 minutes with no worker activity
			// — treat as a likely hang even if no explicit stall
			// telemetry arrived yet. Surfaces silent-mid-stream bugs
			// that predate the idleReader wrap.
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	close(stopObs)
	wg.Wait()

	errStr, _ := lastErr.Load().(string)
	t.Logf("────────────────────────────────────────")
	t.Logf("model: %s", model)
	t.Logf("cycles target: %d  done: %d  spawned: %d", cycles, workerDones.Load(), workerSpawns.Load())
	t.Logf("main iterations: %d  stalls: %d", mainCalls.Load(), stalls.Load())
	t.Logf("ping tool hits: %d", pingCalls.Load())
	t.Logf("tokens: %d in / %d out", totalPrompt.Load(), totalCompleted.Load())
	if errStr != "" {
		t.Logf("last llm.error: %s", errStr)
	}
	t.Logf("────────────────────────────────────────")

	if stalls.Load() > 0 {
		t.Errorf("CAPTURED STALL: %d provider stream idle timeout(s) on model %q — request IDs are in the core log under [PROVIDER] and [FIREWORKS-STALL]",
			stalls.Load(), model)
	}
	if workerDones.Load() < 1 {
		t.Errorf("never saw a worker complete a cycle — possible silent hang pre-idleReader")
	}
}

// fmtSscan shims fmt.Sscan into an int without pulling the fmt import
// just for this single use — keeps the import list tidy on a test
// file that's already importing enough.
func fmtSscan(s string, n *int) (int, error) {
	var parsed int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadDigit
		}
		parsed = parsed*10 + int(r-'0')
	}
	*n = parsed
	return len(s), nil
}

var errBadDigit = &parseErr{"not a digit"}

type parseErr struct{ msg string }

func (e *parseErr) Error() string { return e.msg }
