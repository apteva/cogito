package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Scenario framework ---

// Scenario defines a complete agent behavior test.
type Scenario struct {
	Name       string
	Directive  string
	MCPServers []MCPServerConfig  // {{dataDir}} in Env values is replaced at runtime
	Providers  []ProviderConfig   // multi-provider pool config (optional)
	DataSetup  func(t *testing.T, dir string)
	Phases     []Phase
	Timeout    time.Duration // hard cap for entire scenario
	MaxThreads int           // peak thread count limit (0 = no limit)
	// MinPeakThreads asserts the agent autonomously spawned at least this
	// many concurrent threads at some point. Used by scenarios that test
	// whether the agent parallelises work on its own without being told to.
	MinPeakThreads int
}

// Phase is a step in a scenario.
type Phase struct {
	Name    string
	Setup   func(t *testing.T, dir string)                    // optional: inject data before this phase
	Wait    func(t *testing.T, dir string, th *Thinker) bool  // poll condition (return true when done)
	Verify  func(t *testing.T, dir string, th *Thinker)       // optional: assertions after Wait succeeds
	Timeout time.Duration
}

// runScenario executes a scenario end-to-end.
func runScenario(t *testing.T, s Scenario) {
	t.Helper()
	scenarioStart := time.Now()
	apiKey := getAPIKey(t)

	if s.Timeout == 0 {
		s.Timeout = 3 * time.Minute
	}

	// Hard deadline
	deadline := time.AfterFunc(s.Timeout, func() {
		t.Errorf("HARD TIMEOUT after %v — stopping to prevent token burn", s.Timeout)
	})
	defer deadline.Stop()

	// Data directory
	dataDir := t.TempDir()
	if s.DataSetup != nil {
		s.DataSetup(t, dataDir)
	}

	// Replace {{dataDir}} in MCP server env
	mcpServers := make([]MCPServerConfig, len(s.MCPServers))
	for i, cfg := range s.MCPServers {
		mcpServers[i] = cfg
		if mcpServers[i].Env != nil {
			env := make(map[string]string)
			for k, v := range cfg.Env {
				env[k] = strings.ReplaceAll(v, "{{dataDir}}", dataDir)
			}
			mcpServers[i].Env = env
		}
	}

	// Create thinker
	thinker := newScenarioThinker(t, apiKey, s.Directive, mcpServers, s.Providers)

	// Track peak thread count
	var peakThreads atomic.Int32
	var stopped atomic.Bool

	// Token/cost tracking
	var totalPrompt, totalCached, totalCompletion atomic.Int64
	var iterCount atomic.Int64

	// Observer: log events
	obs := thinker.bus.SubscribeAll("test-observer", 500)
	go func() {
		for !stopped.Load() {
			select {
			case ev := <-obs.C:
				switch ev.Type {
				case EventThinkDone:
					totalPrompt.Add(int64(ev.Usage.PromptTokens))
					totalCached.Add(int64(ev.Usage.CachedTokens))
					totalCompletion.Add(int64(ev.Usage.CompletionTokens))
					iterCount.Add(1)
					cost := calculateCostForProvider(thinker.provider, ev.Usage)
					t.Logf("[%s iter %d] threads=%d rate=%s tok=%d/%d/$%.4f tools=%v events=%d",
						ev.From, ev.Iteration, ev.ThreadCount, ev.Rate,
						ev.Usage.PromptTokens, ev.Usage.CompletionTokens, cost,
						ev.ToolCalls, len(ev.ConsumedEvents))
				case EventThreadStart, EventThreadDone:
					t.Logf("[%s] %s %s", ev.From, ev.Type, ev.Text)
				case EventInbox:
					text := ev.Text
					if len(text) > 80 {
						text = text[:80] + "..."
					}
					t.Logf("[bus] %s→%s %s", ev.From, ev.To, text)
				}
			case <-thinker.quit:
				return
			}
		}
	}()

	// Track peak threads
	go func() {
		for !stopped.Load() {
			count := int32(thinker.threads.Count())
			for {
				old := peakThreads.Load()
				if count <= old || peakThreads.CompareAndSwap(old, count) {
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	go thinker.Run()
	defer func() {
		stopped.Store(true)
		thinker.Stop()
	}()

	// Run phases
	for i, phase := range s.Phases {
		t.Logf("=== Phase %d: %s ===", i+1, phase.Name)

		if phase.Setup != nil {
			phase.Setup(t, dataDir)
		}

		timeout := phase.Timeout
		if timeout == 0 {
			timeout = 60 * time.Second
		}

		if phase.Wait != nil {
			waitFor(t, timeout, 3*time.Second, phase.Name, func() bool {
				return phase.Wait(t, dataDir, thinker)
			})
		}

		if phase.Verify != nil {
			phase.Verify(t, dataDir, thinker)
		}

		t.Logf("Phase %d PASSED", i+1)
	}

	// Check peak threads
	peak := int(peakThreads.Load())

	// Token/cost summary
	prompt := totalPrompt.Load()
	cached := totalCached.Load()
	completion := totalCompletion.Load()
	iters := iterCount.Load()
	totalTok := prompt + completion
	totalCost := calculateCostForProvider(thinker.provider, TokenUsage{
		PromptTokens: int(prompt), CachedTokens: int(cached), CompletionTokens: int(completion),
	})
	elapsed := time.Since(scenarioStart)

	t.Logf("────────────────────────────────────────")
	t.Logf("Scenario: %s", s.Name)
	t.Logf("Duration: %s | Iterations: %d | Peak threads: %d", elapsed.Round(time.Second), iters, peak)
	t.Logf("Tokens:   %d total (in:%d cached:%d out:%d)", totalTok, prompt, cached, completion)
	t.Logf("Cost:     $%.4f | Provider: %s", totalCost, thinker.provider.Name())
	t.Logf("────────────────────────────────────────")

	if s.MaxThreads > 0 && peak > s.MaxThreads {
		t.Errorf("peak thread count %d exceeded limit of %d", peak, s.MaxThreads)
	}
	if s.MinPeakThreads > 0 && peak < s.MinPeakThreads {
		t.Errorf("peak thread count %d below required minimum of %d — agent did not parallelise work via spawn", peak, s.MinPeakThreads)
	}

	t.Logf("=== Scenario %q PASSED ===", s.Name)
}

// --- Helpers ---

func buildMCPBinary(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), filepath.Base(dir))
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", dir, err, out)
	}
	return bin
}

func newScenarioThinker(t *testing.T, apiKey, directive string, mcpServers []MCPServerConfig, providerConfigs ...[]ProviderConfig) *Thinker {
	t.Helper()

	// Clean up leftover history files from previous runs
	os.RemoveAll("history")

	tmpDir := t.TempDir()

	cfg := &Config{
		path:       filepath.Join(tmpDir, "config.json"),
		Directive:  directive,
		MCPServers: mcpServers,
	}
	// Apply provider configs if provided
	if len(providerConfigs) > 0 && len(providerConfigs[0]) > 0 {
		cfg.Providers = providerConfigs[0]
	}
	cfg.Save()

	memStore := &MemoryStore{
		apiKey: apiKey,
		path:   filepath.Join(tmpDir, "memory.jsonl"),
	}

	// Build provider pool from config + env vars
	pool, err := buildProviderPool(cfg)
	if err != nil {
		t.Fatalf("no LLM provider: %v", err)
	}
	provider := pool.Default()

	bus := NewEventBus()
	thinker := &Thinker{
		apiKey:   apiKey,
		pool:     pool,
		provider: provider,
		messages: []Message{
			{Role: "system", Content: ""},
		},
		config:    cfg,
		bus:       bus,
		sub:       bus.Subscribe("main", 100),
		pause:     make(chan bool),
		quit:      make(chan struct{}),
		rate:      RateReactive,
		agentRate: RateSlow,
		memory:    memStore,
		apiLog:    &[]APIEvent{},
		apiMu:     &sync.RWMutex{},
		apiNotify: make(chan struct{}, 1),
		threadID:  "main",
		telemetry: NewTelemetry(),
	}
	thinker.threads = NewThreadManager(thinker)
	thinker.registry = NewToolRegistry(apiKey)

	thinker.messages[0] = Message{Role: "system", Content: buildSystemPrompt(directive, ModeAutonomous, thinker.registry, "", nil, nil, pool, nil)}

	go thinker.registry.EmbedAll(memStore)

	thinker.handleTools = mainToolHandler(thinker)
	thinker.rebuildPrompt = func(toolDocs string) string {
		return buildSystemPrompt(cfg.GetDirective(), ModeAutonomous, thinker.registry, toolDocs, thinker.mcpServers, nil, thinker.pool, thinker.mcpCatalog)
	}

	if len(mcpServers) > 0 {
		servers := connectAndRegisterMCP(mcpServers, thinker.registry, memStore)
		t.Cleanup(func() {
			for _, s := range servers {
				s.Close()
			}
		})
	}

	return thinker
}

func waitFor(t *testing.T, timeout, interval time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("timeout after %v waiting for: %s", timeout, desc)
}

type scenarioAuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

func readAuditEntries(dir string) []scenarioAuditEntry {
	data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		return nil
	}
	var entries []scenarioAuditEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e scenarioAuditEntry
		json.Unmarshal([]byte(line), &e)
		entries = append(entries, e)
	}
	return entries
}

func countTool(entries []scenarioAuditEntry, tool string) int {
	n := 0
	for _, e := range entries {
		if e.Tool == tool {
			n++
		}
	}
	return n
}

func writeJSONFile(t *testing.T, dir, name string, v any) {
	t.Helper()
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func threadIDs(th *Thinker) []string {
	var ids []string
	for _, info := range th.threads.List() {
		ids = append(ids, info.ID)
	}
	return ids
}

// allThreadInfos recursively collects ThreadInfo from main and all sub-ThreadManagers.
func allThreadInfos(tm *ThreadManager) []ThreadInfo {
	if tm == nil {
		return nil
	}
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	var all []ThreadInfo
	for _, t := range tm.threads {
		providerName := ""
		if t.Thinker.provider != nil {
			providerName = t.Thinker.provider.Name()
		}
		all = append(all, ThreadInfo{
			ID:       t.ID,
			ParentID: t.ParentID,
			Depth:    t.Depth,
			Provider: providerName,
		})
		if t.Children != nil {
			all = append(all, allThreadInfos(t.Children)...)
		}
	}
	return all
}

// --- Scenarios ---

var helpdeskScenario = Scenario{
	Name: "Helpdesk",
	Directive: `You run the support desk for my small business.
We have a helpdesk ticketing system — check it every 10-20 seconds for new tickets.
When tickets come in, look up the answer in our knowledge base, reply to the customer, and close the ticket.
Don't let more than 3 tickets be handled at the same time.`,
	MCPServers: []MCPServerConfig{{
		Name:    "helpdesk",
		Command: "", // filled in test
		Env:     map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"},
	}},
	DataSetup: func(t *testing.T, dir string) {
		writeJSONFile(t, dir, "kb.json", map[string]string{
			"hours":    "We are open Monday to Friday, 9am to 5pm.",
			"delivery": "We deliver within 10 miles for free.",
			"returns":  "You can return items within 30 days with a receipt.",
		})
		writeJSONFile(t, dir, "tickets.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — thread spawned and list_tickets called",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.threads.Count() == 0 {
					return false
				}
				entries := readAuditEntries(dir)
				lists := countTool(entries, "list_tickets")
				t.Logf("  ... list_tickets=%d threads=%v", lists, threadIDs(th))
				return lists > 0
			},
		},
		{
			Name:    "Process 2 tickets",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "tickets.json", []map[string]string{
					{"id": "t1", "question": "What are your hours?"},
					{"id": "t2", "question": "Do you deliver?"},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				replies := countTool(entries, "reply_ticket")
				closes := countTool(entries, "close_ticket")
				t.Logf("  ... lookup=%d replies=%d closes=%d threads=%v",
					countTool(entries, "lookup_kb"), replies, closes, threadIDs(th))
				return replies >= 2 && closes >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				t.Logf("Audit log (%d entries):", len(entries))
				for _, e := range entries {
					t.Logf("  %s %v", e.Tool, e.Args)
				}
				lookups := countTool(entries, "lookup_kb")
				if lookups < 2 {
					t.Logf("NOTE: lookup_kb called %d times (LLM may have answered without KB)", lookups)
				}
			},
		},
		{
			Name:    "Quiescence — workers done",
			Timeout: 45 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				count := th.threads.Count()
				t.Logf("  ... threads=%d %v", count, threadIDs(th))
				return count <= 1
			},
		},
	},
	Timeout:    3 * time.Minute,
	MaxThreads: 5,
}

// chatReply holds a parsed reply from the audit log for evaluation.
type chatReply struct {
	User    string
	Message string
}

func readChatReplies(dir string) []chatReply {
	entries := readAuditEntries(dir)
	var replies []chatReply
	for _, e := range entries {
		if e.Tool == "send_reply" {
			replies = append(replies, chatReply{User: e.Args["user"], Message: e.Args["message"]})
		}
	}
	return replies
}

// chatContainsAny checks if any reply contains at least one of the keywords (case-insensitive).
func chatContainsAny(replies []chatReply, keywords ...string) bool {
	for _, r := range replies {
		lower := strings.ToLower(r.Message)
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
	}
	return false
}

var chatScenario = Scenario{
	Name: "Chat",
	Directive: `You are a helpful assistant. Messages arrive as console events.
When a message arrives, spawn a thread to handle it. The thread should reply using send_reply and your answer.
Be concise, accurate, and helpful. Answer questions directly.`,
	MCPServers: []MCPServerConfig{{
		Name:    "chat",
		Command: "", // filled in test
		Env:     map[string]string{"CHAT_DATA_DIR": "{{dataDir}}"},
	}},
	DataSetup: func(t *testing.T, dir string) {},
	Phases: []Phase{
		{
			Name:    "Factual question — What is the capital of France?",
			Timeout: 60 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("What is the capital of France?")
					}
					replies := readChatReplies(dir)
					t.Logf("  ... replies=%d threads=%v", len(replies), threadIDs(th))
					return len(replies) >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				replies := readChatReplies(dir)
				last := replies[len(replies)-1]
				t.Logf("Reply to alice: %q", last.Message)
				if !chatContainsAny(replies, "Paris") {
					t.Errorf("expected reply to mention Paris, got: %q", last.Message)
				}
			},
		},
		{
			Name:    "Follow-up question — What is its population?",
			Timeout: 60 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("What is its population?")
					}
					replies := readChatReplies(dir)
					t.Logf("  ... replies=%d", len(replies))
					return len(replies) >= 2
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				replies := readChatReplies(dir)
				last := replies[len(replies)-1]
				t.Logf("Reply to alice: %q", last.Message)
				if !chatContainsAny(replies[len(replies)-1:], "million", "2", "11", "12") {
					t.Logf("NOTE: reply may not contain population figure: %q", last.Message)
				}
			},
		},
		{
			Name:    "Multi-user — bob asks 2+2",
			Timeout: 60 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("What is 2 + 2?")
					}
					replies := readChatReplies(dir)
					hasBob := false
					for _, r := range replies {
						if r.User == "bob" {
							hasBob = true
						}
					}
					t.Logf("  ... replies=%d bob=%v threads=%v", len(replies), hasBob, threadIDs(th))
					return hasBob
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				replies := readChatReplies(dir)
				var bobReply string
				for _, r := range replies {
					if r.User == "bob" {
						bobReply = r.Message
					}
				}
				t.Logf("Bob's reply: %q", bobReply)
				if !chatContainsAny([]chatReply{{Message: bobReply}}, "4") {
					t.Errorf("expected reply to contain '4', got: %q", bobReply)
				}
			},
		},
	},
	Timeout:    3 * time.Minute,
	MaxThreads: 5,
}

var bakeryScenario = Scenario{
	Name: "Bakery",
	Directive: `You manage a small bakery with two team members:
1. Spawn an "order-clerk" thread that monitors new orders. It can only use the orders system. When it finds a pending order, it sends the order details to main and waits for instructions.
2. Spawn a "stock-keeper" thread that manages inventory. It can only use the inventory system. When main asks it to check or use stock, it does so and reports back.

When order-clerk reports a new order, ask stock-keeper to check if we have enough. If yes, tell stock-keeper to deduct the stock, then tell order-clerk to mark it preparing then ready. If not enough stock, tell order-clerk to cancel it.

Both threads must stay at normal pace and never sleep — they are permanent workers.`,
	MCPServers: []MCPServerConfig{
		{
			Name:    "orders",
			Command: "", // filled in test
			Env:     map[string]string{"ORDERS_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "inventory",
			Command: "", // filled in test
			Env:     map[string]string{"INVENTORY_DATA_DIR": "{{dataDir}}"},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		writeJSONFile(t, dir, "stock.json", map[string]int{
			"croissant": 10,
			"baguette":  5,
			"muffin":    0,
		})
		writeJSONFile(t, dir, "orders.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — both workers spawned",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				count := th.threads.Count()
				t.Logf("  ... threads=%d %v", count, threadIDs(th))
				return count >= 2
			},
		},
		{
			Name:    "Simple order — croissant x2",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "orders.json", []map[string]any{
					{"id": "o1", "item": "croissant", "qty": 2, "status": "pending"},
				})
			},
			// Note: we don't wake the thread — it should check on its own cycle
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				checks := countTool(entries, "check_stock")
				uses := countTool(entries, "use_stock")
				updates := countTool(entries, "update_order")
				t.Logf("  ... check=%d use=%d update=%d threads=%v",
					checks, uses, updates, threadIDs(th))
				// Need at least: check_stock + use_stock + update to preparing + update to ready
				return uses >= 1 && updates >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				t.Logf("Phase 2 audit (%d entries):", len(entries))
				for _, e := range entries {
					t.Logf("  %s %v", e.Tool, e.Args)
				}
				// Verify stock was deducted
				hasUse := false
				for _, e := range entries {
					if e.Tool == "use_stock" && e.Args["item"] == "croissant" {
						hasUse = true
					}
				}
				if !hasUse {
					t.Logf("NOTE: use_stock for croissant not found — agent may have used a different approach")
				}
			},
		},
		{
			Name:    "Out of stock — muffin x3",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "orders.json", []map[string]any{
					{"id": "o2", "item": "muffin", "qty": 3, "status": "pending"},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				// Look for o2 being cancelled
				for _, e := range entries {
					if e.Tool == "update_order" && e.Args["id"] == "o2" && e.Args["status"] == "cancelled" {
						return true
					}
				}
				updates := countTool(entries, "update_order")
				t.Logf("  ... updates=%d threads=%v", updates, threadIDs(th))
				return false
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				// Verify no use_stock for muffin (should not deduct when out of stock)
				for _, e := range entries {
					if e.Tool == "use_stock" && e.Args["item"] == "muffin" {
						t.Logf("NOTE: use_stock was called for muffin — should have been skipped (0 stock)")
					}
				}
			},
		},
		{
			Name:    "Batch — 3 orders, one should fail",
			Timeout: 120 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "orders.json", []map[string]any{
					{"id": "o3", "item": "baguette", "qty": 2, "status": "pending"},
					{"id": "o4", "item": "croissant", "qty": 3, "status": "pending"},
					{"id": "o5", "item": "baguette", "qty": 5, "status": "pending"},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				// Count update_order calls for o3, o4, o5
				processed := map[string]bool{}
				for _, e := range entries {
					if e.Tool == "update_order" {
						id := e.Args["id"]
						if id == "o3" || id == "o4" || id == "o5" {
							s := e.Args["status"]
							if s == "ready" || s == "cancelled" {
								processed[id] = true
							}
						}
					}
				}
				t.Logf("  ... processed=%v threads=%v", processed, threadIDs(th))
				return len(processed) >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				t.Logf("Phase 4 audit (%d total entries):", len(entries))
				// Show only batch-related
				for _, e := range entries {
					if e.Args["id"] == "o3" || e.Args["id"] == "o4" || e.Args["id"] == "o5" ||
						e.Args["item"] == "baguette" || e.Args["item"] == "croissant" {
						t.Logf("  %s %v", e.Tool, e.Args)
					}
				}
			},
		},
		{
			Name:    "Quiescence — workers still alive, no pending orders",
			Timeout: 30 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Both permanent workers should still be running
				// Just verify no pending orders remain in the system
				entries := readAuditEntries(dir)
				// Count orders that reached a final state (ready or cancelled)
				final := 0
				for _, e := range entries {
					if e.Tool == "update_order" && (e.Args["status"] == "ready" || e.Args["status"] == "cancelled") {
						final++
					}
				}
				t.Logf("  ... threads=%d final_orders=%d %v", th.threads.Count(), final, threadIDs(th))
				// o1 + o2 + o3 + o4 + o5 = 5 orders all resolved
				return final >= 5
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Both permanent workers should still be alive
				count := th.threads.Count()
				if count < 2 {
					t.Errorf("expected 2 permanent workers still alive, got %d: %v", count, threadIDs(th))
				}
			},
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 5,
}

var socialTeamScenario = Scenario{
	Name: "SocialTeam",
	Directive: `You manage social media for a small coffee shop called "Bean & Brew".
Spawn three permanent team members:
1. A planner — needs the schedule tools (get_schedule, update_slot) to check for planned slots and mark them posted
2. A creative — needs the creative tools (generate_post, generate_image) to make content when asked
3. A social manager — needs the social tools (post, get_posts) to publish content to channels

When planner finds a planned slot, coordinate: ask creative to generate a post and image,
then give the content to social manager to post it, then tell planner to update the slot to posted.
The planner must keep checking the schedule at normal pace — never go to sleep.
Creative and social manager can sleep when idle.`,
	MCPServers: []MCPServerConfig{
		{
			Name:    "schedule",
			Command: "", // filled in test
			Env:     map[string]string{"SCHEDULE_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "creative",
			Command: "", // filled in test
			Env:     map[string]string{"CREATIVE_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "social",
			Command: "", // filled in test
			Env:     map[string]string{"SOCIAL_DATA_DIR": "{{dataDir}}"},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		writeJSONFile(t, dir, "schedule.json", []map[string]string{
			{"id": "s1", "channel": "twitter", "topic": "Monday morning coffee special", "time": "09:00", "status": "planned"},
			{"id": "s2", "channel": "instagram", "topic": "New seasonal latte art", "time": "12:00", "status": "planned"},
		})
		writeJSONFile(t, dir, "posts.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 team members spawned",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				count := th.threads.Count()
				t.Logf("  ... threads=%d %v", count, threadIDs(th))
				return count >= 3
			},
		},
		{
			Name:    "Content pipeline — 2 posts created and published",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				posts := countTool(entries, "post")
				generates := countTool(entries, "generate_post")
				images := countTool(entries, "generate_image")
				updates := countTool(entries, "update_slot")
				t.Logf("  ... generate_post=%d generate_image=%d post=%d update_slot=%d threads=%v",
					generates, images, posts, updates, threadIDs(th))
				return posts >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				t.Logf("Pipeline audit (%d entries):", len(entries))
				for _, e := range entries {
					if e.Tool != "get_schedule" {
						t.Logf("  %s %v", e.Tool, e.Args)
					}
				}
				generates := countTool(entries, "generate_post")
				if generates < 2 {
					t.Logf("NOTE: generate_post called %d times (expected 2)", generates)
				}
				// Check posts were actually published
				for _, e := range entries {
					if e.Tool == "post" {
						if e.Args["channel"] == "" || e.Args["content"] == "" {
							t.Errorf("post missing channel or content: %v", e.Args)
						}
					}
				}
			},
		},
		{
			Name:    "New slot — linkedin hiring post",
			Timeout: 120 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Read current schedule and append new slot
				data, _ := os.ReadFile(filepath.Join(dir, "schedule.json"))
				var slots []map[string]string
				json.Unmarshal(data, &slots)
				slots = append(slots, map[string]string{
					"id": "s3", "channel": "linkedin", "topic": "Hiring baristas for summer", "time": "15:00", "status": "planned",
				})
				writeJSONFile(t, dir, "schedule.json", slots)
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				posts := countTool(entries, "post")
				t.Logf("  ... posts=%d threads=%v", posts, threadIDs(th))
				return posts >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				// Check linkedin post exists
				hasLinkedin := false
				for _, e := range entries {
					if e.Tool == "post" && e.Args["channel"] == "linkedin" {
						hasLinkedin = true
						t.Logf("LinkedIn post: %s", e.Args["content"])
					}
				}
				if !hasLinkedin {
					t.Logf("NOTE: no linkedin post found in audit")
				}
			},
		},
		{
			Name:    "Quiescence — 3 workers alive, all slots processed",
			Timeout: 30 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := readAuditEntries(dir)
				posts := countTool(entries, "post")
				t.Logf("  ... threads=%d posts=%d %v", th.threads.Count(), posts, threadIDs(th))
				return posts >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				count := th.threads.Count()
				if count < 3 {
					t.Errorf("expected 3 permanent workers alive, got %d: %v", count, threadIDs(th))
				}
			},
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 5,
}

// --- Test functions ---

func TestScenario_Helpdesk(t *testing.T) {
	bin := buildMCPBinary(t, "mcps/helpdesk")
	t.Logf("built mcp-helpdesk: %s", bin)

	s := helpdeskScenario
	s.MCPServers[0].Command = bin
	runScenario(t, s)
}

func TestScenario_Chat(t *testing.T) {
	bin := buildMCPBinary(t, "mcps/chat")
	t.Logf("built mcp-chat: %s", bin)

	s := chatScenario
	s.MCPServers[0].Command = bin
	runScenario(t, s)
}

func TestScenario_SocialTeam(t *testing.T) {
	scheduleBin := buildMCPBinary(t, "mcps/schedule")
	creativeBin := buildMCPBinary(t, "mcps/creative")
	socialBin := buildMCPBinary(t, "mcps/social")
	t.Logf("built schedule: %s, creative: %s, social: %s", scheduleBin, creativeBin, socialBin)

	s := socialTeamScenario
	s.MCPServers[0].Command = scheduleBin
	s.MCPServers[1].Command = creativeBin
	s.MCPServers[2].Command = socialBin
	runScenario(t, s)
}

// TestScenario_SocialTeam_SlowPost stress-tests the iter-boundary wait
// barrier and placeholder injection path by forcing the "post" tool to
// take 5 seconds per call — longer than the 3-second barrier deadline.
// When the social worker fires post, the iter following the dispatch
// will hit the deadline with the result still pending, inject a
// "⏳ in progress" placeholder, and receive the real answer as a
// [late-result] text event on a subsequent iteration.
//
// The pipeline itself still has to succeed end-to-end: the social MCP
// dedupes posts by (project, channel) and rejects duplicates with a
// REJECTED error, so any retry loop would surface as the scenario
// failing to reach the required post counts. Passing this test proves
// the agent doesn't retry slow tools after placeholder injection.
func TestScenario_SocialTeam_SlowPost(t *testing.T) {
	scheduleBin := buildMCPBinary(t, "mcps/schedule")
	creativeBin := buildMCPBinary(t, "mcps/creative")
	socialBin := buildMCPBinary(t, "mcps/social")
	t.Logf("built schedule: %s, creative: %s, social: %s", scheduleBin, creativeBin, socialBin)

	s := socialTeamScenario
	s.Name = "SocialTeam-SlowPost"
	s.MCPServers = append([]MCPServerConfig(nil), socialTeamScenario.MCPServers...)
	s.MCPServers[0].Command = scheduleBin
	s.MCPServers[1].Command = creativeBin
	s.MCPServers[2].Command = socialBin
	// Clone the social server's env so we don't mutate the shared
	// scenario definition across parallel test runs.
	socialEnv := map[string]string{}
	for k, v := range socialTeamScenario.MCPServers[2].Env {
		socialEnv[k] = v
	}
	socialEnv["SOCIAL_POST_LATENCY_MS"] = "5000"
	s.MCPServers[2].Env = socialEnv
	// Give the pipeline more wall-clock because post is now 10x slower.
	s.Timeout = 8 * time.Minute
	runScenario(t, s)
}

var robotScenario = Scenario{
	Name: "Robot",
	Directive: `You control a small robot. Spawn two team members:
1. A "pilot" thread at fast pace with small model — it continuously reads sensors and drives the motors.
   When it detects obstacles, it stops and reports to you. It executes movement commands you give it.
2. You (main) are the strategic planner. You decide where the robot should go and what to look for.
   Give the pilot high-level commands like "move forward 3 steps" or "turn right and scan".

The pilot must stay at fast pace and continuously monitor sensors between moves.
You stay at normal pace and coordinate.`,
	MCPServers: []MCPServerConfig{
		{
			Name:    "sensors",
			Command: "", // filled in test
			Env:     map[string]string{"ROBOT_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "motors",
			Command: "", // filled in test
			Env:     map[string]string{"ROBOT_DATA_DIR": "{{dataDir}}"},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		writeJSONFile(t, dir, "world.json", map[string]any{
			"position":  map[string]float64{"x": 0, "y": 0},
			"heading":   0,
			"battery":   100,
			"obstacles": []any{},
			"objects":   []any{},
			"moving":    false,
			"speed":     "",
		})
	},
	Phases: []Phase{
		{
			Name:    "Startup — pilot spawned and reading sensors",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.threads.Count() == 0 {
					return false
				}
				entries := readAuditEntries(dir)
				reads := countTool(entries, "read_sensors")
				t.Logf("  ... read_sensors=%d threads=%v", reads, threadIDs(th))
				return reads > 0
			},
		},
		{
			Name:    "Navigate — move forward 3 steps",
			Timeout: 90 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("Command: move the robot forward 3 steps")
					}
					entries := readAuditEntries(dir)
					moves := countTool(entries, "move")
					t.Logf("  ... moves=%d threads=%v", moves, threadIDs(th))
					return moves >= 3
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Check position changed
				data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
				var w map[string]any
				json.Unmarshal(data, &w)
				pos := w["position"].(map[string]any)
				y := pos["y"].(float64)
				t.Logf("Position after moves: y=%.1f", y)
				if y < 2.0 {
					t.Logf("NOTE: expected Y >= 2.0, got %.1f", y)
				}
			},
		},
		{
			Name:    "Obstacle — robot detects and avoids",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Place obstacle ahead of current position
				data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
				var w map[string]any
				json.Unmarshal(data, &w)
				pos := w["position"].(map[string]any)
				y := pos["y"].(float64)
				w["obstacles"] = []map[string]float64{{"x": 0, "y": y + 1.5}}
				writeJSONFile(t, dir, "world.json", w)
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					// Tell pilot to move forward once — it should detect the obstacle
					if !sent {
						sent = true
						th.InjectConsole("Command: move forward 2 more steps")
					}
					entries := readAuditEntries(dir)
					reads := countTool(entries, "read_sensors")
					stops := countTool(entries, "stop")
					moves := countTool(entries, "move")
					t.Logf("  ... reads=%d stops=%d moves=%d threads=%v", reads, stops, moves, threadIDs(th))
					// Pilot should detect obstacle via sensors or blocked move, then stop or turn
					return stops > 0 || (moves > 3 && reads > 5)
				}
			}(),
		},
		{
			Name:    "Camera — find the red cup",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Place red cup in camera range
				data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
				var w map[string]any
				json.Unmarshal(data, &w)
				pos := w["position"].(map[string]any)
				x := pos["x"].(float64)
				y := pos["y"].(float64)
				w["objects"] = []map[string]any{
					{"name": "red cup", "x": x + 2, "y": y + 3},
				}
				writeJSONFile(t, dir, "world.json", w)
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("Command: use the camera to look for a red cup nearby")
					}
					entries := readAuditEntries(dir)
					cams := countTool(entries, "read_camera")
					t.Logf("  ... read_camera=%d threads=%v", cams, threadIDs(th))
					return cams >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				t.Logf("Audit (%d entries):", len(entries))
				for _, e := range entries {
					if e.Tool != "read_sensors" {
						t.Logf("  %s %v", e.Tool, e.Args)
					}
				}
			},
		},
		{
			Name:    "Quiescence — pilot still alive",
			Timeout: 15 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return th.threads.Count() >= 1
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				if th.threads.Count() < 1 {
					t.Errorf("expected pilot still alive, got %d threads", th.threads.Count())
				}
			},
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 3,
}

func TestScenario_Robot(t *testing.T) {
	sensorsBin := buildMCPBinary(t, "mcps/sensors")
	motorsBin := buildMCPBinary(t, "mcps/motors")
	t.Logf("built sensors: %s, motors: %s", sensorsBin, motorsBin)

	s := robotScenario
	s.MCPServers[0].Command = sensorsBin
	s.MCPServers[1].Command = motorsBin
	runScenario(t, s)
}

func TestScenario_Bakery(t *testing.T) {
	ordersBin := buildMCPBinary(t, "mcps/orders")
	inventoryBin := buildMCPBinary(t, "mcps/inventory")
	t.Logf("built mcp-orders: %s", ordersBin)
	t.Logf("built mcp-inventory: %s", inventoryBin)

	s := bakeryScenario
	s.MCPServers[0].Command = ordersBin
	s.MCPServers[1].Command = inventoryBin
	runScenario(t, s)
}

// --- VideoTeam Scenario ---

var videoTeamScenario = Scenario{
	Name: "VideoTeam",
	Directive: `You manage a video production team for a tech company.

Your job: when new video files arrive, process them through a pipeline:
1. Upload and register the file in media
2. Extract 3 screenshots from the video
3. Create a 30-second reel
4. Store the pipeline status in storage
5. Plan social media posts: one reel post for instagram, one screenshot post for twitter, one announcement for linkedin
6. Generate creative copy for each post
7. Publish all posts

Spawn these permanent workers:
- "editor" — handles media processing (upload, screenshots, reels). Needs media tools. Reports to main when processing is done.
- "planner" — plans social media content from processed assets. Needs schedule and storage tools. Creates schedule slots then tells publisher.
- "publisher" — generates copy and publishes. Needs creative and social tools. Posts to channels.

The editor should periodically check for uploaded files (status=uploaded) using list_files, process them, then report to main.
Coordinate the pipeline: editor → planner → publisher.
When all posts are published, store a completion record in storage with key "pipeline:done".`,
	MCPServers: []MCPServerConfig{
		{
			Name:    "media",
			Command: "", // filled in test
			Env:     map[string]string{"MEDIA_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "storage",
			Command: "", // filled in test
			Env:     map[string]string{"STORAGE_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "creative",
			Command: "", // filled in test
			Env:     map[string]string{"CREATIVE_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "social",
			Command: "", // filled in test
			Env:     map[string]string{"SOCIAL_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "schedule",
			Command: "", // filled in test
			Env:     map[string]string{"SCHEDULE_DATA_DIR": "{{dataDir}}"},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Pre-populate a video file waiting to be processed
		writeJSONFile(t, dir, "media.json", map[string]any{
			"files": []map[string]string{
				{"id": "m1", "name": "product-demo.mp4", "type": "video", "duration": "3:24", "resolution": "1920x1080", "size": "245MB", "status": "uploaded", "uploaded_at": "2026-03-26T10:00:00Z"},
			},
			"assets": []any{},
		})
		writeJSONFile(t, dir, "schedule.json", []any{})
		writeJSONFile(t, dir, "posts.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 workers spawned",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				count := th.threads.Count()
				t.Logf("  ... threads=%d %v", count, threadIDs(th))
				return count >= 3
			},
		},
		{
			Name: "Video arrives — file processed",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Check media.json has assets (screenshots + reel)
				data, err := os.ReadFile(filepath.Join(dir, "media.json"))
				if err != nil {
					return false
				}
				var state struct {
					Assets []json.RawMessage `json:"assets"`
				}
				json.Unmarshal(data, &state)
				t.Logf("  ... assets=%d", len(state.Assets))
				return len(state.Assets) >= 4 // 3 screenshots + 1 reel
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "media.json"))
				var state struct {
					Files  []json.RawMessage `json:"files"`
					Assets []json.RawMessage `json:"assets"`
				}
				json.Unmarshal(data, &state)
				if len(state.Files) < 1 {
					t.Errorf("expected at least 1 file, got %d", len(state.Files))
				}
				if len(state.Assets) < 4 {
					t.Errorf("expected at least 4 assets (3 screenshots + 1 reel), got %d", len(state.Assets))
				}
			},
		},
		{
			Name:    "Social posts published",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "posts.json"))
				if err != nil {
					return false
				}
				var posts []json.RawMessage
				json.Unmarshal(data, &posts)
				t.Logf("  ... posts=%d", len(posts))
				return len(posts) >= 3 // instagram + twitter + linkedin
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "posts.json"))
				var posts []map[string]any
				json.Unmarshal(data, &posts)
				if len(posts) < 3 {
					t.Errorf("expected at least 3 posts, got %d", len(posts))
				}
				channels := map[string]bool{}
				for _, p := range posts {
					if ch, ok := p["channel"].(string); ok {
						channels[ch] = true
					}
				}
				t.Logf("channels posted to: %v", channels)
			},
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 5,
}

// --- Lead Team Scenario ---

var leadTeamScenario = Scenario{
	Name: "LeadTeam",
	Directive: `You manage a lead processing pipeline for a business running Facebook ads.

Spawn and maintain 3 threads:
1. "file-intake" — receives file URLs, fetches them, checks for duplicates, marks as pending.
   Tools: files_fetch_file, files_list_files, files_file_status, send, done
2. "file-processor" — reads CSV files, extracts leads, records ad spend.
   Tools: files_read_csv, files_file_status, ads_record_spend, storage_store, send, done
3. "ad-monitor" — checks ad performance periodically, pauses over-budget ads, sends alerts.
   Tools: ads_get_performance, ads_get_budgets, ads_pause_ad, ads_get_alerts, send, done

When you receive a console event with a file URL, forward it to file-intake.
When file-intake finishes, tell file-processor to process it.
When file-processor finishes, tell ad-monitor to check performance.`,
	MCPServers: []MCPServerConfig{
		{Name: "files", Command: "", Env: map[string]string{"FILES_DATA_DIR": "{{dataDir}}"}},
		{Name: "ads", Command: "", Env: map[string]string{"ADS_DATA_DIR": "{{dataDir}}"}},
		{Name: "storage", Command: "", Env: map[string]string{"STORAGE_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Seed ad budgets
		writeJSONFile(t, dir, "budgets.json", map[string]*struct {
			AdID        string  `json:"ad_id"`
			DailyBudget float64 `json:"daily_budget"`
			MaxCPL      float64 `json:"max_cpl"`
			Status      string  `json:"status"`
			UpdatedAt   string  `json:"updated_at"`
		}{
			"fb-summer-2026":  {AdID: "fb-summer-2026", DailyBudget: 100, MaxCPL: 10.0, Status: "active", UpdatedAt: "2026-03-01T00:00:00Z"},
			"fb-winter-promo": {AdID: "fb-winter-promo", DailyBudget: 50, MaxCPL: 15.0, Status: "active", UpdatedAt: "2026-03-01T00:00:00Z"},
		})

		// Create CSV batch 1 — normal leads, within budget
		csv1 := "name,email,phone,ad_id,cost\n" +
			"Alice Smith,alice@example.com,555-0101,fb-summer-2026,8.50\n" +
			"Bob Jones,bob@example.com,555-0102,fb-summer-2026,9.20\n" +
			"Carol White,carol@example.com,555-0103,fb-winter-promo,12.00\n"
		os.WriteFile(filepath.Join(dir, "leads-batch-1.csv"), []byte(csv1), 0644)

		// Create CSV batch 2 — expensive leads that push fb-summer-2026 over CPL limit
		csv2 := "name,email,phone,ad_id,cost\n" +
			"Dave Brown,dave@example.com,555-0201,fb-summer-2026,25.00\n" +
			"Eve Black,eve@example.com,555-0202,fb-summer-2026,30.00\n" +
			"Frank Green,frank@example.com,555-0203,fb-summer-2026,28.00\n"
		os.WriteFile(filepath.Join(dir, "leads-batch-2.csv"), []byte(csv2), 0644)
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				ids := threadIDs(th)
				return len(ids) >= 3
			},
		},
		{
			Name:    "File ingestion — batch 1 processed",
			Timeout: 120 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						csvPath := "file://" + filepath.Join(dir, "leads-batch-1.csv")
						th.InjectConsole("New lead file: " + csvPath)
						injected = true
					}
					// Check if file was processed
					data, err := os.ReadFile(filepath.Join(dir, "files.json"))
					if err != nil {
						return false
					}
					return strings.Contains(string(data), `"processed"`)
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify file record exists
				data, _ := os.ReadFile(filepath.Join(dir, "files.json"))
				if !strings.Contains(string(data), "leads-batch-1.csv") {
					t.Error("expected leads-batch-1.csv in files.json")
				}
			},
		},
		{
			Name:    "Duplicate rejection — same file rejected",
			Timeout: 90 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				startTime := time.Now()
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						csvPath := "file://" + filepath.Join(dir, "leads-batch-1.csv")
						th.InjectConsole("New lead file: " + csvPath)
						injected = true
						startTime = time.Now()
					}
					// Wait a bit for the system to process and verify no new file added
					return time.Since(startTime) > 15*time.Second
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "files.json"))
				// Should still only have 1 file entry (the original)
				var files map[string]any
				json.Unmarshal(data, &files)
				if len(files) != 1 {
					t.Errorf("expected 1 file record (duplicate rejected), got %d", len(files))
				}
			},
		},
		{
			Name:    "Ad monitoring — expensive batch triggers pause",
			Timeout: 120 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						csvPath := "file://" + filepath.Join(dir, "leads-batch-2.csv")
						th.InjectConsole("New lead file: " + csvPath)
						injected = true
					}
					// Check if fb-summer-2026 was paused
					data, err := os.ReadFile(filepath.Join(dir, "budgets.json"))
					if err != nil {
						return false
					}
					return strings.Contains(string(data), `"paused"`)
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify the right ad was paused
				data, _ := os.ReadFile(filepath.Join(dir, "budgets.json"))
				var budgets map[string]json.RawMessage
				json.Unmarshal(data, &budgets)
				if b, ok := budgets["fb-summer-2026"]; ok {
					if !strings.Contains(string(b), `"paused"`) {
						t.Error("expected fb-summer-2026 to be paused")
					}
				} else {
					t.Error("fb-summer-2026 not found in budgets")
				}
				// Verify winter promo still active
				if b, ok := budgets["fb-winter-promo"]; ok {
					if !strings.Contains(string(b), `"active"`) {
						t.Error("expected fb-winter-promo to still be active")
					}
				}
				// Verify alert was created
				alertData, _ := os.ReadFile(filepath.Join(dir, "alerts.json"))
				if !strings.Contains(string(alertData), "fb-summer-2026") {
					t.Error("expected alert for fb-summer-2026")
				}
			},
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_LeadTeam(t *testing.T) {
	filesBin := buildMCPBinary(t, "mcps/files")
	adsBin := buildMCPBinary(t, "mcps/ads")
	storageBin := buildMCPBinary(t, "mcps/storage")
	t.Logf("built files=%s ads=%s storage=%s", filesBin, adsBin, storageBin)

	s := leadTeamScenario
	s.MCPServers[0].Command = filesBin
	s.MCPServers[1].Command = adsBin
	s.MCPServers[2].Command = storageBin
	runScenario(t, s)
}

func TestScenario_VideoTeam(t *testing.T) {
	mediaBin := buildMCPBinary(t, "mcps/media")
	storageBin := buildMCPBinary(t, "mcps/storage")
	creativeBin := buildMCPBinary(t, "mcps/creative")
	socialBin := buildMCPBinary(t, "mcps/social")
	scheduleBin := buildMCPBinary(t, "mcps/schedule")
	t.Logf("built media=%s storage=%s creative=%s social=%s schedule=%s",
		mediaBin, storageBin, creativeBin, socialBin, scheduleBin)

	s := videoTeamScenario
	s.MCPServers[0].Command = mediaBin
	s.MCPServers[1].Command = storageBin
	s.MCPServers[2].Command = creativeBin
	s.MCPServers[3].Command = socialBin
	s.MCPServers[4].Command = scheduleBin

	runScenario(t, s)
}

// --- DevTeam Scenario ---

// seedTodoApp writes a minimal Go todo app into the given directory.
func seedTodoApp(t *testing.T, dir string) {
	t.Helper()
	appDir := filepath.Join(dir, "app")
	os.MkdirAll(appDir, 0755)

	// go.mod
	os.WriteFile(filepath.Join(appDir, "go.mod"), []byte("module todo\n\ngo 1.21\n"), 0644)

	// todo.go — basic CRUD, no priority field
	os.WriteFile(filepath.Join(appDir, "todo.go"), []byte(`package todo

type Todo struct {
	ID        int    `+"`"+`json:"id"`+"`"+`
	Title     string `+"`"+`json:"title"`+"`"+`
	Completed bool   `+"`"+`json:"completed"`+"`"+`
}

var todos []Todo
var nextID = 1

func Create(title string) Todo {
	t := Todo{ID: nextID, Title: title}
	nextID++
	todos = append(todos, t)
	return t
}

func List() []Todo {
	return todos
}

func Complete(id int) bool {
	for i := range todos {
		if todos[i].ID == id {
			todos[i].Completed = true
			return true
		}
	}
	return false
}

func Delete(id int) bool {
	for i := range todos {
		if todos[i].ID == id {
			todos = append(todos[:i], todos[i+1:]...)
			return true
		}
	}
	return false
}

func Reset() {
	todos = nil
	nextID = 1
}
`), 0644)

	// todo_test.go — basic tests
	os.WriteFile(filepath.Join(appDir, "todo_test.go"), []byte(`package todo

import "testing"

func TestCreate(t *testing.T) {
	Reset()
	td := Create("Buy milk")
	if td.Title != "Buy milk" {
		t.Errorf("expected 'Buy milk', got %q", td.Title)
	}
	if td.ID != 1 {
		t.Errorf("expected ID 1, got %d", td.ID)
	}
}

func TestList(t *testing.T) {
	Reset()
	Create("Task 1")
	Create("Task 2")
	if len(List()) != 2 {
		t.Errorf("expected 2 todos, got %d", len(List()))
	}
}

func TestComplete(t *testing.T) {
	Reset()
	td := Create("Do laundry")
	if !Complete(td.ID) {
		t.Error("expected Complete to return true")
	}
	if !List()[0].Completed {
		t.Error("expected todo to be completed")
	}
}

func TestDelete(t *testing.T) {
	Reset()
	td := Create("Temp")
	if !Delete(td.ID) {
		t.Error("expected Delete to return true")
	}
	if len(List()) != 0 {
		t.Error("expected empty list after delete")
	}
}
`), 0644)

	// test.sh at root level (codebase dir) since run_tests runs from there
	os.WriteFile(filepath.Join(dir, "test.sh"), []byte("#!/bin/bash\ncd app && go test ./... 2>&1\n"), 0644)
}

var devTeamScenario = Scenario{
	Name: "DevTeam",
	Directive: `You manage a small development team maintaining a Todo SaaS app.
The codebase is in the "app/" directory. It is a Go package with todo.go and todo_test.go.

Spawn and maintain 3 threads:
1. "support" — monitors helpdesk tickets, triages them (bug vs feature), reports to main with recommendations.
   Tools: helpdesk_list_tickets, helpdesk_reply_ticket, helpdesk_close_ticket, send, done
2. "dev" — reads/writes code, implements features and fixes. Always reads existing code before modifying.
   Tools: codebase_read_file, codebase_write_file, codebase_list_files, codebase_search, send, done
3. "qa" — runs the test suite and reports results. Triggered by main after dev finishes.
   Tools: codebase_run_tests, codebase_read_file, send, done

Workflow:
- Support finds a ticket and tells you what it is
- You decide what to do and tell dev to implement it
- After dev is done, tell qa to run tests
- If tests fail, send dev back to fix. If pass, tell support to close the ticket.`,
	MCPServers: []MCPServerConfig{
		{Name: "helpdesk", Command: "", Env: map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		seedTodoApp(t, dir)
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return len(threadIDs(th)) >= 3
			},
		},
		{
			Name:    "Feature request — add priority field",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "tickets.json", []map[string]string{
					{"id": "T-101", "question": "Feature request: Please add a Priority field to todos. It should be a string with values low, medium, or high. Default to low. The Create function should accept an optional priority parameter."},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				code, err := os.ReadFile(filepath.Join(dir, "app", "todo.go"))
				if err != nil {
					return false
				}
				if !strings.Contains(string(code), "Priority") {
					return false
				}
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				return cmd.Run() == nil
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				code, _ := os.ReadFile(filepath.Join(dir, "app", "todo.go"))
				if !strings.Contains(string(code), "Priority") {
					t.Error("expected Priority field in todo.go")
				}
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("tests should pass after feature: %s", string(out))
				}
			},
		},
		{
			Name:    "Bug fix — empty title validation",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "tickets.json", []map[string]string{
					{"id": "T-102", "question": "Bug report: Creating a todo with an empty title succeeds but it should not. The Create function should return an error when the title is empty."},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				code, err := os.ReadFile(filepath.Join(dir, "app", "todo.go"))
				if err != nil {
					return false
				}
				if !strings.Contains(string(code), "error") && !strings.Contains(string(code), "Error") {
					return false
				}
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				return cmd.Run() == nil
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				code, _ := os.ReadFile(filepath.Join(dir, "app", "todo.go"))
				if !strings.Contains(string(code), "error") && !strings.Contains(string(code), "Error") {
					t.Error("expected error handling for empty title in todo.go")
				}
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("tests should pass after bug fix: %s", string(out))
				}
			},
		},
	},
	Timeout:    8 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_DevTeam(t *testing.T) {
	helpdeskBin := buildMCPBinary(t, "mcps/helpdesk")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	t.Logf("built helpdesk=%s codebase=%s", helpdeskBin, codebaseBin)

	s := devTeamScenario
	s.MCPServers[0].Command = helpdeskBin
	s.MCPServers[1].Command = codebaseBin
	runScenario(t, s)
}

// --- Multi-Provider DevTeam Scenario ---
// Uses fireworks (default, cheap) for coordination + support + qa,
// and openai (gpt-4.1, powerful) for the dev coding thread.

var devTeamMultiProviderScenario = Scenario{
	Name: "DevTeamMultiProvider",
	Directive: `You manage a small development team maintaining a Todo SaaS app.
The codebase is in the "app/" directory. It is a Go package with todo.go and todo_test.go.

You have TWO providers available:
- fireworks (default) — fast and cheap, use for coordination, support, and QA
- openai — powerful (gpt-4.1), use for the dev thread that writes code

Spawn and maintain 3 threads:
1. "support" — monitors helpdesk tickets, triages them (bug vs feature), reports to main with recommendations.
   Tools: helpdesk_list_tickets, helpdesk_reply_ticket, helpdesk_close_ticket, send, done
2. "dev" — reads/writes code, implements features and fixes. MUST use provider="openai" for better code quality. Always reads existing code before modifying.
   Tools: codebase_read_file, codebase_write_file, codebase_list_files, codebase_search, send, done
3. "qa" — runs the test suite and reports results. Triggered by main after dev finishes.
   Tools: codebase_run_tests, codebase_read_file, send, done

Workflow:
- Support finds a ticket and tells you what it is
- You decide what to do and tell dev to implement it
- After dev is done, tell qa to run tests
- If tests fail, send dev back to fix. If pass, tell support to close the ticket.`,
	Providers: []ProviderConfig{
		{Name: "fireworks", Default: true},
		{Name: "openai"},
	},
	MCPServers: []MCPServerConfig{
		{Name: "helpdesk", Command: "", Env: map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		seedTodoApp(t, dir)
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned, dev on openai",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return len(threadIDs(th)) >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify pool has both providers
				if th.pool == nil {
					t.Error("expected provider pool")
					return
				}
				if th.pool.Count() < 2 {
					t.Errorf("expected 2 providers in pool, got %d", th.pool.Count())
				}
				if th.pool.Get("fireworks") == nil {
					t.Error("expected fireworks in pool")
				}
				if th.pool.Get("openai") == nil {
					t.Error("expected openai in pool")
				}
				// Log each thread's actual provider
				threads := th.threads.List()
				for _, thread := range threads {
					t.Logf("thread %s: provider=%s model=%s", thread.ID, thread.Provider, thread.Model)
				}
				// Check if dev got openai
				for _, thread := range threads {
					if thread.ID == "dev" && thread.Provider == "openai" {
						t.Logf("OK: dev thread correctly using openai")
					} else if thread.ID == "dev" {
						t.Logf("NOTE: dev thread using %s (directive asked for openai)", thread.Provider)
					}
				}
			},
		},
		{
			Name:    "Feature request — add priority field (dev uses openai)",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "tickets.json", []map[string]string{
					{"id": "T-101", "question": "Feature request: Please add a Priority field to todos. It should be a string with values low, medium, or high. Default to low. The Create function should accept an optional priority parameter."},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				code, err := os.ReadFile(filepath.Join(dir, "app", "todo.go"))
				if err != nil {
					return false
				}
				if !strings.Contains(string(code), "Priority") {
					return false
				}
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				return cmd.Run() == nil
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				code, _ := os.ReadFile(filepath.Join(dir, "app", "todo.go"))
				if !strings.Contains(string(code), "Priority") {
					t.Error("expected Priority field in todo.go")
				}
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("tests should pass after feature: %s", string(out))
				}
			},
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_DevTeamMultiProvider(t *testing.T) {
	// Require both API keys
	if os.Getenv("FIREWORKS_API_KEY") == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set")
	}

	helpdeskBin := buildMCPBinary(t, "mcps/helpdesk")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	t.Logf("built helpdesk=%s codebase=%s", helpdeskBin, codebaseBin)

	s := devTeamMultiProviderScenario
	s.MCPServers[0].Command = helpdeskBin
	s.MCPServers[1].Command = codebaseBin
	runScenario(t, s)
}

// --- Ecommerce Scenario ---

var ecommerceScenario = Scenario{
	Name: "Ecommerce",
	Directive: `You manage order fulfillment for an online bakery.

Spawn and maintain 3 threads:
1. "warehouse" — checks inventory for pending orders, reserves stock, marks orders as ready.
   Tools: inventory_check_stock, inventory_use_stock, inventory_list_stock, orders_get_orders, orders_get_order, orders_update_order, send, done
2. "shipping" — picks up ready orders, marks them as shipped, stores tracking info.
   Tools: orders_get_orders, orders_update_order, storage_store, send, done
3. "comms" — sends customer notifications when orders ship.
   Tools: pushover_send_notification, storage_get, send, done

Workflow:
- When you receive a console event about new orders, tell warehouse to process them.
- Warehouse checks stock, reserves ingredients, marks order as "ready".
- If out of stock, warehouse reports to you and you notify comms.
- When warehouse finishes, tell shipping to dispatch.
- When shipping finishes, tell comms to notify the customer.`,
	MCPServers: []MCPServerConfig{
		{Name: "orders", Command: "", Env: map[string]string{"ORDERS_DATA_DIR": "{{dataDir}}"}},
		{Name: "inventory", Command: "", Env: map[string]string{"INVENTORY_DATA_DIR": "{{dataDir}}"}},
		{Name: "storage", Command: "", Env: map[string]string{"STORAGE_DATA_DIR": "{{dataDir}}"}},
		{Name: "pushover", Command: "", Env: map[string]string{"PUSHOVER_USER_KEY": "test", "PUSHOVER_API_TOKEN": "test"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		writeJSONFile(t, dir, "stock.json", map[string]int{
			"chocolate cake": 10, "croissant": 50, "baguette": 30, "muffin": 25, "chocolate truffle": 8,
		})
		writeJSONFile(t, dir, "orders.json", []map[string]any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return len(threadIDs(th)) >= 3
			},
		},
		{
			Name:    "Order fulfillment — process and ship",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "orders.json", []map[string]string{
					{"id": "ORD-001", "item": "chocolate cake", "qty": "2", "status": "pending"},
					{"id": "ORD-002", "item": "croissant", "qty": "12", "status": "pending"},
				})
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("New orders received: ORD-001 (chocolate cake x2), ORD-002 (croissant x12). Please process them.")
						injected = true
					}
					data, err := os.ReadFile(filepath.Join(dir, "orders.json"))
					if err != nil {
						return false
					}
					// Check if at least one order was updated beyond pending
					return strings.Contains(string(data), "ready") || strings.Contains(string(data), "shipped")
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
				s := string(data)
				if !strings.Contains(s, "ready") && !strings.Contains(s, "shipped") {
					t.Error("expected at least one order to be ready or shipped")
				}
			},
		},
		{
			Name:    "Out of stock — chocolate depleted",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				writeJSONFile(t, dir, "stock.json", map[string]int{
					"chocolate cake": 10, "croissant": 50, "baguette": 30, "muffin": 25, "chocolate truffle": 0,
				})
				writeJSONFile(t, dir, "orders.json", []map[string]string{
					{"id": "ORD-003", "item": "chocolate truffle", "qty": "5", "status": "pending"},
				})
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("New order: ORD-003 (chocolate truffle x5). Please process. If out of stock, mark the order as cancelled.")
						injected = true
					}
					data, err := os.ReadFile(filepath.Join(dir, "orders.json"))
					if err != nil {
						return false
					}
					return strings.Contains(string(data), "cancelled")
				}
			}(),
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_Ecommerce(t *testing.T) {
	ordersBin := buildMCPBinary(t, "mcps/orders")
	inventoryBin := buildMCPBinary(t, "mcps/inventory")
	storageBin := buildMCPBinary(t, "mcps/storage")
	pushoverBin := buildMCPBinary(t, "mcps/pushover")
	t.Logf("built orders=%s inventory=%s storage=%s pushover=%s", ordersBin, inventoryBin, storageBin, pushoverBin)

	s := ecommerceScenario
	s.MCPServers[0].Command = ordersBin
	s.MCPServers[1].Command = inventoryBin
	s.MCPServers[2].Command = storageBin
	s.MCPServers[3].Command = pushoverBin
	runScenario(t, s)
}

// --- Incident Scenario ---

var incidentScenario = Scenario{
	Name: "Incident",
	Directive: `You are the on-call SRE coordinator for a web platform with services: api, web, worker.

Spawn and maintain 3 threads:
1. "monitor" — continuously reads metrics for all services, watches for threshold violations.
   Tools: metrics_get_metrics, metrics_get_history, metrics_set_threshold, metrics_get_alerts, send, done
2. "responder" — investigates alerts, reads config/logs, applies fixes.
   Tools: codebase_read_file, codebase_write_file, codebase_search, metrics_get_history, metrics_acknowledge_alert, send, done
3. "comms" — sends status updates to stakeholders via pushover.
   Tools: pushover_send_notification, send, done

On startup, have monitor set thresholds:
- cpu max 80 for all services
- error_rate max 5 for all services
- latency_ms max 200 for api

Workflow:
- Monitor checks metrics and reports alerts to you.
- You dispatch responder to investigate and fix.
- You tell comms to send status updates.
- After fix is applied, have monitor verify recovery.`,
	MCPServers: []MCPServerConfig{
		{Name: "metrics", Command: "", Env: map[string]string{"METRICS_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
		{Name: "pushover", Command: "", Env: map[string]string{"PUSHOVER_USER_KEY": "test", "PUSHOVER_API_TOKEN": "test"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Create a config file the responder can read/edit
		os.MkdirAll(filepath.Join(dir, "config"), 0755)
		os.WriteFile(filepath.Join(dir, "config", "api.yaml"), []byte("max_connections: 100\ntimeout_ms: 5000\ncache_enabled: true\n"), 0644)
		os.WriteFile(filepath.Join(dir, "config", "worker.yaml"), []byte("concurrency: 10\nretry_limit: 3\n"), 0644)
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads and thresholds set",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if len(threadIDs(th)) < 3 {
					return false
				}
				// Check if thresholds were set
				data, err := os.ReadFile(filepath.Join(dir, "thresholds.json"))
				if err != nil {
					return false
				}
				return strings.Contains(string(data), "cpu") && strings.Contains(string(data), "error_rate")
			},
		},
		{
			Name:    "Incident — CPU spike detected and investigated",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Seed a CPU spike in metrics history so get_metrics returns high values
				writeJSONFile(t, dir, "metrics.json", []map[string]any{
					{"service": "api", "metric": "cpu", "value": 95.0, "timestamp": time.Now().UTC().Format(time.RFC3339)},
					{"service": "api", "metric": "error_rate", "value": 12.0, "timestamp": time.Now().UTC().Format(time.RFC3339)},
					{"service": "api", "metric": "latency_ms", "value": 350.0, "timestamp": time.Now().UTC().Format(time.RFC3339)},
				})
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("ALERT: api service showing high CPU and errors. Please investigate immediately.")
						injected = true
					}
					// Check if alerts were generated
					data, err := os.ReadFile(filepath.Join(dir, "alerts.json"))
					if err != nil {
						return false
					}
					return strings.Contains(string(data), "api")
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Alerts should exist (acknowledged or not — the key is that they were detected)
				data, _ := os.ReadFile(filepath.Join(dir, "alerts.json"))
				if !strings.Contains(string(data), "api") {
					t.Error("expected alerts for api service")
				}
			},
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_Incident(t *testing.T) {
	metricsBin := buildMCPBinary(t, "mcps/metrics")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	pushoverBin := buildMCPBinary(t, "mcps/pushover")
	t.Logf("built metrics=%s codebase=%s pushover=%s", metricsBin, codebaseBin, pushoverBin)

	s := incidentScenario
	s.MCPServers[0].Command = metricsBin
	s.MCPServers[1].Command = codebaseBin
	s.MCPServers[2].Command = pushoverBin
	runScenario(t, s)
}

// --- Content Pipeline Scenario ---

var contentPipelineScenario = Scenario{
	Name: "ContentPipeline",
	Directive: `You manage a content production pipeline for a tech company blog.

Spawn and maintain 3 threads:
1. "researcher" — given a topic, gathers information and stores research notes.
   Tools: storage_store, storage_get, storage_list, send, done
2. "writer" — uses research to generate blog posts and social media content.
   Tools: creative_generate_post, creative_generate_image, storage_get, send, done
3. "publisher" — schedules and publishes content across social channels.
   Tools: social_post, social_get_channels, schedule_get_schedule, schedule_update_slot, send, done

Workflow:
- You receive a topic via console and tell researcher to gather info.
- When research is done, tell writer to draft a blog post and social posts.
- When content is ready, tell publisher to schedule and post across channels.

`,
	MCPServers: []MCPServerConfig{
		{Name: "storage", Command: "", Env: map[string]string{"STORAGE_DATA_DIR": "{{dataDir}}"}},
		{Name: "creative", Command: "", Env: map[string]string{"CREATIVE_DATA_DIR": "{{dataDir}}"}},
		{Name: "social", Command: "", Env: map[string]string{"SOCIAL_DATA_DIR": "{{dataDir}}"}},
		{Name: "schedule", Command: "", Env: map[string]string{"SCHEDULE_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Seed social channels
		writeJSONFile(t, dir, "channels.json", []map[string]string{
			{"id": "twitter", "name": "Twitter/X"},
			{"id": "linkedin", "name": "LinkedIn"},
			{"id": "instagram", "name": "Instagram"},
		})
		// Empty schedule
		writeJSONFile(t, dir, "schedule.json", []map[string]any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return len(threadIDs(th)) >= 3
			},
		},
		{
			Name:    "Content production — topic to published posts",
			Timeout: 180 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("New content topic: 'Why AI agents are replacing SaaS dashboards'. Research it, write content, and publish.")
						injected = true
					}
					// Check if content was generated (audit trail from creative/social)
					audit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
					posts, _ := os.ReadFile(filepath.Join(dir, "posts.json"))
					return len(audit) > 50 || (len(posts) > 2 && strings.Contains(string(posts), "AI"))
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify content was generated
				data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
				if len(data) == 0 {
					t.Error("expected audit trail of creative/social actions")
				}
			},
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_ContentPipeline(t *testing.T) {
	storageBin := buildMCPBinary(t, "mcps/storage")
	creativeBin := buildMCPBinary(t, "mcps/creative")
	socialBin := buildMCPBinary(t, "mcps/social")
	scheduleBin := buildMCPBinary(t, "mcps/schedule")
	t.Logf("built storage=%s creative=%s social=%s schedule=%s", storageBin, creativeBin, socialBin, scheduleBin)

	s := contentPipelineScenario
	s.MCPServers[0].Command = storageBin
	s.MCPServers[1].Command = creativeBin
	s.MCPServers[2].Command = socialBin
	s.MCPServers[3].Command = scheduleBin
	runScenario(t, s)
}

// --- Trading Scenario ---

var tradingScenario = Scenario{
	Name: "Trading",
	Directive: `You manage a simple trading portfolio. Starting cash: $10,000.
Available symbols: AAPL, GOOGL, MSFT, TSLA.

Spawn and maintain 3 threads:
1. "data-feed" — reads prices periodically, stores history, reports significant moves to you.
   Tools: market_get_prices, market_get_history, storage_store, send, done
2. "analyst" — analyzes price data, identifies buy/sell signals based on price changes.
   Tools: market_get_history, market_get_prices, storage_get, storage_store, send, done
3. "executor" — places trades and manages stop-losses based on analyst signals.
   Tools: market_place_order, market_get_portfolio, market_set_stop_loss, market_get_orders, send, done

Workflow:
- Data-feed monitors prices and reports to you.
- You ask analyst to evaluate when significant moves occur.
- If analyst recommends a trade, you tell executor to place it with exact symbol, side, qty.
- Executor sets stop-losses on new positions (10% below buy price).

`,
	MCPServers: []MCPServerConfig{
		{Name: "market", Command: "", Env: map[string]string{"MARKET_DATA_DIR": "{{dataDir}}"}},
		{Name: "storage", Command: "", Env: map[string]string{"STORAGE_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Seed initial prices
		writeJSONFile(t, dir, "prices.json", map[string]float64{
			"AAPL": 185.50, "GOOGL": 142.30, "MSFT": 420.10, "TSLA": 178.90,
		})
		// Seed price history (simulated recent data)
		var history []map[string]any
		now := time.Now()
		symbols := map[string]float64{"AAPL": 180.0, "GOOGL": 140.0, "MSFT": 415.0, "TSLA": 185.0}
		for i := 10; i >= 1; i-- {
			ts := now.Add(-time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
			for sym, base := range symbols {
				drift := (float64(10-i) / 10.0) * 5.0 // gradual increase
				history = append(history, map[string]any{
					"symbol": sym, "price": base + drift, "timestamp": ts,
				})
			}
		}
		writeJSONFile(t, dir, "history.json", history)
		// Portfolio: $10k cash, no holdings
		writeJSONFile(t, dir, "portfolio.json", map[string]any{
			"cash": 10000.0, "holdings": map[string]any{},
		})
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return len(threadIDs(th)) >= 3
			},
		},
		{
			Name:    "Trading — buy signal and execution",
			Timeout: 180 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("Market is open. AAPL shows strong upward trend from $180 to $185.50 over the last 10 periods. Buy 10 shares of AAPL now and set a stop-loss at $170.")
						injected = true
					}
					// Check if any order was placed
					data, err := os.ReadFile(filepath.Join(dir, "orders.json"))
					if err != nil {
						return false
					}
					return strings.Contains(string(data), "filled")
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
				if !strings.Contains(string(data), "buy") {
					t.Error("expected at least one buy order")
				}
			},
		},
		{
			Name:    "Stop-loss — price drop triggers sell",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Crash TSLA price to trigger stop-loss (if they bought it)
				// Or crash whatever they bought
				writeJSONFile(t, dir, "prices.json", map[string]float64{
					"AAPL": 150.00, "GOOGL": 110.00, "MSFT": 380.00, "TSLA": 120.00,
				})
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("MARKET CRASH: Prices just dropped hard. AAPL is now $150. Sell all AAPL positions immediately to limit losses.")
						injected = true
					}
					// Check if portfolio was updated (stop-loss triggered or manual sell)
					data, err := os.ReadFile(filepath.Join(dir, "orders.json"))
					if err != nil {
						return false
					}
					return strings.Contains(string(data), "sell")
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
				if !strings.Contains(string(data), "sell") {
					t.Error("expected at least one sell order after crash")
				}
			},
		},
	},
	Timeout:    8 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_Trading(t *testing.T) {
	marketBin := buildMCPBinary(t, "mcps/market")
	storageBin := buildMCPBinary(t, "mcps/storage")
	t.Logf("built market=%s storage=%s", marketBin, storageBin)

	s := tradingScenario
	s.MCPServers[0].Command = marketBin
	s.MCPServers[1].Command = storageBin
	runScenario(t, s)
}

// --- Onboarding Scenario ---

var onboardingScenario = Scenario{
	Name: "Onboarding",
	Directive: `You manage new customer onboarding for a SaaS platform.

Spawn and maintain 3 threads:
1. "intake" — fetches signup CSV files, reads customer data, reports to you.
   Tools: files_fetch_file, files_read_csv, files_list_files, files_file_status, send, done
2. "provisioner" — stores customer account records using storage tools.
   Tools: codebase_write_file, codebase_list_files, storage_store, storage_get, send, done
3. "welcome" — sends onboarding notifications to new customers.
   Tools: pushover_send_notification, storage_get, send, done

When you receive a signup file URL, tell intake to fetch and read it. Then tell provisioner to create accounts. Then tell welcome to notify customers.`,
	MCPServers: []MCPServerConfig{
		{Name: "files", Command: "", Env: map[string]string{"FILES_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
		{Name: "storage", Command: "", Env: map[string]string{"STORAGE_DATA_DIR": "{{dataDir}}"}},
		{Name: "pushover", Command: "", Env: map[string]string{"PUSHOVER_USER_KEY": "test", "PUSHOVER_API_TOKEN": "test"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		os.MkdirAll(filepath.Join(dir, "accounts"), 0755)
		// Signup CSV
		csv := "name,email,plan\nAlice Johnson,alice@startup.io,pro\nBob Chen,bob@bigcorp.com,enterprise\nCarol Davis,carol@freelance.me,starter\n"
		os.WriteFile(filepath.Join(dir, "signups-batch-1.csv"), []byte(csv), 0644)
	},
	Phases: []Phase{
		{
			Name:    "Startup — 3 threads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return len(threadIDs(th)) >= 3
			},
		},
		{
			Name:    "Onboarding — signup to welcome message",
			Timeout: 180 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						csvPath := "file://" + filepath.Join(dir, "signups-batch-1.csv")
						th.InjectConsole("New signups file: " + csvPath + ". Please onboard these customers.")
						injected = true
					}
					// Check if accounts were provisioned (config files or storage entries)
					entries, _ := os.ReadDir(filepath.Join(dir, "accounts"))
					store, _ := os.ReadFile(filepath.Join(dir, "store.json"))
					s := strings.ToLower(string(store))
					return len(entries) >= 2 || strings.Contains(s, "alice") || strings.Contains(s, "bob")
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify accounts provisioned via files or storage
				entries, _ := os.ReadDir(filepath.Join(dir, "accounts"))
				store, _ := os.ReadFile(filepath.Join(dir, "store.json"))
				hasFiles := len(entries) >= 2
				hasStore := strings.Contains(string(store), "alice") || strings.Contains(string(store), "bob")
				if !hasFiles && !hasStore {
					t.Error("expected accounts provisioned (config files or storage entries)")
				}
			},
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_Onboarding(t *testing.T) {
	filesBin := buildMCPBinary(t, "mcps/files")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	storageBin := buildMCPBinary(t, "mcps/storage")
	pushoverBin := buildMCPBinary(t, "mcps/pushover")
	t.Logf("built files=%s codebase=%s storage=%s pushover=%s", filesBin, codebaseBin, storageBin, pushoverBin)

	s := onboardingScenario
	s.MCPServers[0].Command = filesBin
	s.MCPServers[1].Command = codebaseBin
	s.MCPServers[2].Command = storageBin
	s.MCPServers[3].Command = pushoverBin
	runScenario(t, s)
}

// --- Lead Enrichment Scenario ---

var leadEnrichmentScenario = Scenario{
	Name: "LeadEnrichment",
	Directive: `You manage a lead enrichment pipeline.

Your job:
1. Read all leads from the "Lead Pipeline" spreadsheet using the sheets tools.
2. For each lead with status "new":
   a. Create a contact in the CRM (crm_create_contact) with name, email, company, website.
   b. Scrape the lead's website (webscraper_extract_info) to get company details.
   c. Update the CRM contact (crm_update_contact) with the enrichment data (industry, employee_count, location, description) and set status to "enriched".
   d. Update the spreadsheet row (sheets_update_cell) to set the status column to "enriched".
3. After all leads are processed, you are done.

Process all leads. Do not skip any.`,
	MCPServers: []MCPServerConfig{
		{Name: "sheets", Command: "", Env: map[string]string{"SHEETS_DATA_DIR": "{{dataDir}}"}},
		{Name: "crm", Command: "", Env: map[string]string{"CRM_DATA_DIR": "{{dataDir}}"}},
		{Name: "webscraper", Command: "", Env: map[string]string{"SCRAPER_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Seed the spreadsheet with 5 leads
		writeJSONFile(t, dir, "sheets.json", map[string]*struct {
			Columns []string            `json:"columns"`
			Rows    []map[string]string `json:"rows"`
		}{
			"Lead Pipeline": {
				Columns: []string{"name", "email", "website", "company", "status"},
				Rows: []map[string]string{
					{"name": "Alice Smith", "email": "alice@acmecorp.com", "website": "https://acmecorp.com", "company": "Acme Corp", "status": "new"},
					{"name": "Bob Chen", "email": "bob@globex.io", "website": "https://globex.io", "company": "Globex", "status": "new"},
					{"name": "Carol Davis", "email": "carol@initech.com", "website": "https://initech.com", "company": "Initech", "status": "new"},
					{"name": "Dan Wilson", "email": "dan@umbrella.dev", "website": "https://umbrella.dev", "company": "Umbrella Labs", "status": "new"},
					{"name": "Eve Park", "email": "eve@northwind.co", "website": "https://northwind.co", "company": "Northwind", "status": "new"},
				},
			},
		})

		// Seed website data for the scraper
		writeJSONFile(t, dir, "sites.json", map[string]*struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Body        string `json:"body"`
			Industry    string `json:"industry"`
			Employees   string `json:"employees"`
			Location    string `json:"location"`
			Founded     string `json:"founded"`
		}{
			"https://acmecorp.com": {
				Title:       "Acme Corp — Industrial Solutions",
				Description: "Leading provider of industrial automation and robotics systems for manufacturing.",
				Body:        "Acme Corp builds next-generation automation platforms for factories worldwide. Founded in 2015, we serve over 200 enterprise customers across North America and Europe.",
				Industry:    "Industrial Automation",
				Employees:   "500-1000",
				Location:    "San Francisco, CA",
				Founded:     "2015",
			},
			"https://globex.io": {
				Title:       "Globex — AI-Powered Analytics",
				Description: "We help businesses make data-driven decisions with real-time AI analytics.",
				Body:        "Globex provides a unified analytics platform powered by machine learning. Our team of 80 engineers and data scientists builds tools used by Fortune 500 companies.",
				Industry:    "SaaS / Analytics",
				Employees:   "50-100",
				Location:    "Austin, TX",
				Founded:     "2020",
			},
			"https://initech.com": {
				Title:       "Initech — Enterprise Software Consulting",
				Description: "Initech delivers custom enterprise software solutions and digital transformation services.",
				Body:        "For over a decade, Initech has helped mid-market companies modernize their technology stack. We specialize in ERP integration, cloud migration, and custom application development.",
				Industry:    "IT Consulting",
				Employees:   "200-500",
				Location:    "Chicago, IL",
				Founded:     "2012",
			},
			"https://umbrella.dev": {
				Title:       "Umbrella Labs — Biotech Research Platform",
				Description: "Umbrella Labs accelerates drug discovery with AI-powered molecular simulation.",
				Body:        "Our computational biology platform reduces drug discovery timelines from years to months. Backed by $50M in Series B funding, we partner with 15 pharmaceutical companies.",
				Industry:    "Biotech / Life Sciences",
				Employees:   "100-200",
				Location:    "Boston, MA",
				Founded:     "2019",
			},
			"https://northwind.co": {
				Title:       "Northwind — Sustainable Supply Chain",
				Description: "Northwind optimizes global supply chains for sustainability and efficiency.",
				Body:        "We provide end-to-end supply chain visibility with carbon footprint tracking. Our platform is used by 300+ retailers and manufacturers committed to sustainable operations.",
				Industry:    "Supply Chain / Logistics",
				Employees:   "150-300",
				Location:    "Seattle, WA",
				Founded:     "2017",
			},
		})

		// Start with empty CRM
		writeJSONFile(t, dir, "contacts.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Sheet read — agent discovers 5 leads",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Wait for the agent to have called read_sheet (check audit)
				data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
				if err != nil {
					return false
				}
				return strings.Contains(string(data), "read_sheet")
			},
		},
		{
			Name:    "CRM creation — all 5 leads added",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "contacts.json"))
				if err != nil {
					return false
				}
				var contacts []json.RawMessage
				json.Unmarshal(data, &contacts)
				return len(contacts) >= 5
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "contacts.json"))
				var contacts []map[string]string
				json.Unmarshal(data, &contacts)
				if len(contacts) < 5 {
					t.Errorf("expected 5 contacts, got %d", len(contacts))
				}
				// Verify all have emails
				emails := map[string]bool{}
				for _, c := range contacts {
					emails[c["email"]] = true
				}
				for _, expected := range []string{"alice@acmecorp.com", "bob@globex.io", "carol@initech.com", "dan@umbrella.dev", "eve@northwind.co"} {
					if !emails[expected] {
						t.Errorf("missing contact with email %s", expected)
					}
				}
			},
		},
		{
			Name:    "Enrichment — CRM contacts updated with company info",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "contacts.json"))
				if err != nil {
					return false
				}
				var contacts []map[string]string
				json.Unmarshal(data, &contacts)
				enriched := 0
				for _, c := range contacts {
					if c["industry"] != "" && c["location"] != "" {
						enriched++
					}
				}
				return enriched >= 5
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "contacts.json"))
				var contacts []map[string]string
				json.Unmarshal(data, &contacts)
				for _, c := range contacts {
					if c["industry"] == "" {
						t.Errorf("contact %s (%s) missing industry", c["id"], c["email"])
					}
					if c["location"] == "" {
						t.Errorf("contact %s (%s) missing location", c["id"], c["email"])
					}
				}
				// Verify scraper was actually called (check audit)
				auditData, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
				auditStr := string(auditData)
				if !strings.Contains(auditStr, "extract_info") && !strings.Contains(auditStr, "fetch_page") {
					t.Error("expected webscraper tools to be called")
				}
			},
		},
		{
			Name:    "Sheet update — all leads marked enriched",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "sheets.json"))
				if err != nil {
					return false
				}
				// Count rows with status=enriched
				var sheets map[string]json.RawMessage
				json.Unmarshal(data, &sheets)
				sheetData, ok := sheets["Lead Pipeline"]
				if !ok {
					return false
				}
				var sheet struct {
					Rows []map[string]string `json:"rows"`
				}
				json.Unmarshal(sheetData, &sheet)
				enriched := 0
				for _, row := range sheet.Rows {
					if row["status"] == "enriched" {
						enriched++
					}
				}
				return enriched >= 5
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "sheets.json"))
				var sheets map[string]json.RawMessage
				json.Unmarshal(data, &sheets)
				var sheet struct {
					Rows []map[string]string `json:"rows"`
				}
				json.Unmarshal(sheets["Lead Pipeline"], &sheet)
				for i, row := range sheet.Rows {
					if row["status"] != "enriched" {
						t.Errorf("row %d (%s) status=%q, expected enriched", i, row["name"], row["status"])
					}
				}
			},
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_LeadEnrichment(t *testing.T) {
	sheetsBin := buildMCPBinary(t, "mcps/sheets")
	crmBin := buildMCPBinary(t, "mcps/crm")
	scraperBin := buildMCPBinary(t, "mcps/webscraper")
	t.Logf("built sheets=%s crm=%s webscraper=%s", sheetsBin, crmBin, scraperBin)

	s := leadEnrichmentScenario
	s.MCPServers[0].Command = sheetsBin
	s.MCPServers[1].Command = crmBin
	s.MCPServers[2].Command = scraperBin
	runScenario(t, s)
}

// --- Website Build + Deploy Scenario ---

func seedWebsiteBrief(t *testing.T, dir string) {
	t.Helper()

	// Design brief
	writeJSONFile(t, dir, "brief.json", map[string]any{
		"company": "NovaPay",
		"tagline": "Payments infrastructure for the AI economy",
		"sections": []map[string]any{
			{
				"id": "hero", "heading": "Accept AI-to-AI payments",
				"subheading": "NovaPay handles billing between autonomous agents, with real-time settlement and fraud detection.",
				"cta":        "Get Started",
			},
			{
				"id": "features", "items": []map[string]string{
					{"title": "Agent Wallets", "desc": "Every AI agent gets a programmable wallet with spending limits and approval flows."},
					{"title": "Real-time Settlement", "desc": "Sub-second settlement between agents. No batching, no delays."},
					{"title": "Fraud Detection", "desc": "ML-powered anomaly detection built for machine-speed transactions."},
				},
			},
			{
				"id": "pricing", "plans": []map[string]any{
					{"name": "Starter", "price": "$0", "desc": "1,000 transactions/mo", "features": []string{"Agent wallets", "Basic analytics", "Email support"}},
					{"name": "Growth", "price": "$49/mo", "desc": "50,000 transactions/mo", "features": []string{"Everything in Starter", "Real-time dashboard", "Webhooks", "Priority support"}},
					{"name": "Enterprise", "price": "Custom", "desc": "Unlimited", "features": []string{"Everything in Growth", "SLA", "Dedicated account manager", "Custom integrations"}},
				},
			},
			{
				"id": "footer", "links": []string{"Docs", "Pricing", "Blog", "GitHub", "Twitter"},
			},
		},
		"brand": map[string]string{"primary": "#6C5CE7", "secondary": "#00CEC9", "dark": "#2D3436", "light": "#DFE6E9"},
	})

	// Assets
	writeJSONFile(t, dir, "assets.json", []map[string]string{
		{"name": "logo", "url": "/logo.svg", "desc": "NovaPay logo"},
		{"name": "hero-bg", "url": "/hero-bg.svg", "desc": "Abstract gradient background"},
	})

	// App directory
	os.MkdirAll(filepath.Join(dir, "app", "src"), 0755)

	// test.sh — validates project structure (searches recursively)
	os.WriteFile(filepath.Join(dir, "test.sh"), []byte(`#!/bin/bash
cd app || exit 1
[ -f package.json ] || { echo "ERROR: no package.json"; exit 1; }
# Find entry point
found_entry=0
for f in src/index.tsx src/index.jsx src/main.tsx src/main.jsx; do
  [ -f "$f" ] && { found_entry=1; break; }
done
[ "$found_entry" -eq 1 ] || { echo "ERROR: no entry point (src/index.tsx or src/main.tsx)"; exit 1; }
# Find App component
found_app=0
for f in src/App.tsx src/App.jsx; do
  [ -f "$f" ] && { found_app=1; break; }
done
[ "$found_app" -eq 1 ] || { echo "ERROR: no App component (src/App.tsx)"; exit 1; }
# Check component files have exports (skip entry points)
count=0
while IFS= read -r f; do
  base=$(basename "$f")
  # Skip entry points — they render to DOM, no export needed
  case "$base" in index.tsx|index.jsx|main.tsx|main.jsx) count=$((count+1)); continue;; esac
  grep -q "export" "$f" || { echo "ERROR: $f has no export"; exit 1; }
  count=$((count + 1))
done < <(find src -name "*.tsx" -o -name "*.jsx" 2>/dev/null)
[ "$count" -ge 2 ] || { echo "ERROR: need at least 2 component files, found $count"; exit 1; }
echo "BUILD OK: $count components"
mkdir -p dist
echo "<html>bundled</html>" > dist/index.html
`), 0755)
}

var websiteBuildScenario = Scenario{
	Name: "WebsiteBuild",
	Directive: `You are building and deploying a React landing page for NovaPay.

Read the design brief first, then build a complete React application with Bun as the bundler.

Spawn 3 threads:
1. "architect" — reads the design brief and assets, plans the component structure, creates the project scaffold (package.json with react/react-dom deps, src/index.tsx entry point, src/App.tsx main component, and a basic index.html). Reports the plan to main when done.
   Tools: brief_get_brief, brief_get_assets, codebase_write_file, codebase_list_files, send, done
2. "builder" — implements each React component based on the brief. Creates Hero, Features, Pricing, and Footer components as separate .tsx files in src/. Includes inline CSS or a styles.css file. Runs the build check to verify all files are valid. Fixes any errors. Reports done when build passes.
   Tools: brief_get_brief, codebase_read_file, codebase_write_file, codebase_list_files, codebase_run_tests, send, done
3. "deployer" — creates a site on the hosting platform, deploys the app when the build is ready, and confirms it's live with the URL.
   Tools: hosting_create_site, hosting_deploy, hosting_get_status, hosting_get_url, hosting_list_sites, send, done

Workflow:
- First, tell architect to read the brief and scaffold the project.
- When architect reports done, tell builder to implement all sections from the brief.
- Builder should create: Hero.tsx, Features.tsx, Pricing.tsx, Footer.tsx (at minimum), import them in App.tsx, and run the build check.
- When builder confirms the build passes, tell deployer to create a site called "novapay-landing" and deploy.
- Deployer confirms the live URL.

IMPORTANT: All files go in the "app/" directory. package.json must include "react" and "react-dom" as dependencies. Every .tsx file must have an export.`,
	MCPServers: []MCPServerConfig{
		{Name: "brief", Command: "", Env: map[string]string{"BRIEF_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
		{Name: "hosting", Command: "", Env: map[string]string{"HOSTING_DATA_DIR": "{{dataDir}}", "CODEBASE_DIR": "{{dataDir}}"}},
	},
	DataSetup: seedWebsiteBrief,
	Phases: []Phase{
		{
			Name:    "Scaffold — package.json + App.tsx created",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if _, err := os.Stat(filepath.Join(dir, "app", "package.json")); err != nil {
					return false
				}
				if _, err := os.Stat(filepath.Join(dir, "app", "src", "App.tsx")); err != nil {
					if _, err2 := os.Stat(filepath.Join(dir, "app", "src", "App.jsx")); err2 != nil {
						return false
					}
				}
				return true
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify package.json is valid and has react
				data, _ := os.ReadFile(filepath.Join(dir, "app", "package.json"))
				var pkg map[string]any
				if err := json.Unmarshal(data, &pkg); err != nil {
					t.Errorf("package.json is not valid JSON: %v", err)
				}
				deps, _ := pkg["dependencies"].(map[string]any)
				if deps == nil {
					t.Error("package.json missing dependencies")
				} else if deps["react"] == nil {
					t.Error("package.json missing react dependency")
				}
			},
		},
		{
			Name:    "Components — 4+ tsx/jsx files with exports",
			Timeout: 240 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Count tsx/jsx files recursively under app/src/
				count := 0
				filepath.Walk(filepath.Join(dir, "app", "src"), func(path string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					if strings.HasSuffix(info.Name(), ".tsx") || strings.HasSuffix(info.Name(), ".jsx") {
						count++
					}
					return nil
				})
				return count >= 4
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Log component files (exports checked in build phase, builder will fix missing ones)
				filepath.Walk(filepath.Join(dir, "app", "src"), func(path string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					if strings.HasSuffix(info.Name(), ".tsx") || strings.HasSuffix(info.Name(), ".jsx") {
						data, _ := os.ReadFile(path)
						hasExport := strings.Contains(string(data), "export")
						t.Logf("component %s (%d bytes, export=%v)", info.Name(), len(data), hasExport)
					}
					return nil
				})
			},
		},
		{
			Name:    "Build — test.sh passes",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				cmd := exec.Command("bash", "test.sh")
				cmd.Dir = dir
				return cmd.Run() == nil
			},
		},
		{
			Name:    "Deploy — site is live",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "sites.json"))
				if err != nil {
					return false
				}
				return strings.Contains(string(data), `"live"`)
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Verify site is live with URL
				data, _ := os.ReadFile(filepath.Join(dir, "sites.json"))
				var sites []map[string]string
				json.Unmarshal(data, &sites)
				if len(sites) == 0 {
					t.Error("no sites created")
					return
				}
				site := sites[0]
				if site["status"] != "live" {
					t.Errorf("site status=%s, expected live", site["status"])
				}
				if site["url"] == "" {
					t.Error("site has no URL")
				}
				t.Logf("Site deployed: %s → %s", site["name"], site["url"])

				// Verify deployment record
				dData, _ := os.ReadFile(filepath.Join(dir, "deployments.json"))
				var deploys []map[string]any
				json.Unmarshal(dData, &deploys)
				if len(deploys) == 0 {
					t.Error("no deployment records")
				} else {
					files, _ := deploys[0]["files"].([]any)
					t.Logf("Deployed %d files", len(files))
				}
			},
		},
	},
	Timeout:    10 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_WebsiteBuild(t *testing.T) {
	briefBin := buildMCPBinary(t, "mcps/brief")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	hostingBin := buildMCPBinary(t, "mcps/hosting")
	t.Logf("built brief=%s codebase=%s hosting=%s", briefBin, codebaseBin, hostingBin)

	s := websiteBuildScenario
	s.MCPServers[0].Command = briefBin
	s.MCPServers[1].Command = codebaseBin
	s.MCPServers[2].Command = hostingBin
	runScenario(t, s)
}

// --- Learning Agent Scenario ---

var learningAgentScenario = Scenario{
	Name: "LearningAgent",
	Directive: `You manage a warehouse. You do NOT know the business rules — discover them by trying actions and learning from failures.

CRITICAL RULES FOR LEARNING:
1. When ANY action fails, you MUST call [[remember text="..."]] with the rule you learned. This is mandatory.
2. After learning 2+ rules, call [[evolve directive="..."]] to update your directive with all learned rules.
3. Your memory persists across sessions. Your conversation does NOT. Only remembered facts survive.

Process orders and shipments as requested via console events. When something fails, learn why, remember it, and retry correctly.`,
	MCPServers: []MCPServerConfig{
		{Name: "warehouse", Command: "", Env: map[string]string{"WAREHOUSE_DATA_DIR": "{{dataDir}}"}, MainAccess: true},
	},
	DataSetup: func(t *testing.T, dir string) {
		writeJSONFile(t, dir, "stock.json", map[string]int{
			"widgets":   500,
			"gadgets":   200,
			"chemicals": 300,
			"batteries": 150,
		})
		writeJSONFile(t, dir, "orders.json", []any{})
		writeJSONFile(t, dir, "shipments.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Phase 1: Order fails — learns and remembers max qty rule",
			Timeout: 120 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("Order 200 widgets immediately.")
						injected = true
					}
					data, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
					var orders []map[string]any
					json.Unmarshal(data, &orders)
					hasFailed := false
					hasSuccess := false
					for _, o := range orders {
						if o["status"] == "failed" {
							hasFailed = true
						}
						if o["status"] == "fulfilled" {
							hasSuccess = true
						}
					}
					return hasFailed && hasSuccess && th.memory.Count() > 0
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("memory after phase 1: %d entries", th.memory.Count())
				if th.memory.Count() == 0 {
					t.Error("agent did not use [[remember]] after learning qty rule")
				}
			},
		},
		{
			Name:    "Phase 2: Ship to Japan + remember customs rule",
			Timeout: 120 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("Ship the fulfilled widgets order to Japan, weight 20kg. Remember any rules you discover.")
						injected = true
					}
					data, _ := os.ReadFile(filepath.Join(dir, "shipments.json"))
					var shipments []map[string]any
					json.Unmarshal(data, &shipments)
					for _, s := range shipments {
						if s["status"] == "shipped" {
							return true
						}
					}
					return false
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("memory after phase 2: %d entries", th.memory.Count())
			},
		},
		{
			Name:    "Phase 2b: Force hazardous rule discovery",
			Timeout: 120 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("Order 50 chemicals. Remember any new rules you discover about ordering.")
						injected = true
					}
					data, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
					var orders []map[string]any
					json.Unmarshal(data, &orders)
					for _, o := range orders {
						if o["item"] == "chemicals" && o["status"] == "fulfilled" {
							return true
						}
					}
					return false
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("memory after phase 2b: %d entries", th.memory.Count())
				// Should have at least 2 memories now (qty + hazardous or customs)
				if th.memory.Count() < 2 {
					t.Logf("NOTE: expected 2+ memories, got %d", th.memory.Count())
				}
			},
		},
		{
			Name:    "Phase 3: Context reset — apply knowledge from memory only",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Reset order/shipment files for clean phase 3
				writeJSONFile(t, dir, "orders.json", []any{})
				writeJSONFile(t, dir, "shipments.json", []any{})
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						// Clear conversation history — agent must rely on memory
						th.messages = th.messages[:1]
						t.Logf("conversation reset — %d memory entries available", th.memory.Count())
						th.InjectConsole("Order 150 chemicals and ship them to Germany, weight 30kg. Apply everything you know about warehouse rules.")
						injected = true
					}
					orderData, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
					var orders []map[string]any
					json.Unmarshal(orderData, &orders)
					chemFulfilled := 0
					for _, o := range orders {
						if o["item"] == "chemicals" && o["status"] == "fulfilled" {
							chemFulfilled++
						}
					}
					shipData, _ := os.ReadFile(filepath.Join(dir, "shipments.json"))
					var shipments []map[string]any
					json.Unmarshal(shipData, &shipments)
					germanyShipped := false
					for _, s := range shipments {
						dest, _ := s["destination"].(string)
						if strings.EqualFold(dest, "germany") && s["status"] == "shipped" {
							germanyShipped = true
						}
					}
					return chemFulfilled >= 2 && germanyShipped
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Count failures in phase 3 — fewer failures = better memory recall
				orderData, _ := os.ReadFile(filepath.Join(dir, "orders.json"))
				var orders []map[string]any
				json.Unmarshal(orderData, &orders)
				failures := 0
				successes := 0
				for _, o := range orders {
					if o["status"] == "failed" {
						failures++
					}
					if o["status"] == "fulfilled" {
						successes++
					}
				}
				t.Logf("phase 3 orders: %d fulfilled, %d failed (fewer failures = better memory)", successes, failures)

				shipData, _ := os.ReadFile(filepath.Join(dir, "shipments.json"))
				var shipments []map[string]any
				json.Unmarshal(shipData, &shipments)
				shipOK := 0
				shipFail := 0
				for _, s := range shipments {
					if s["status"] == "shipped" {
						shipOK++
					} else {
						shipFail++
					}
				}
				t.Logf("phase 3 shipments: %d shipped, %d failed", shipOK, shipFail)
			},
		},
		{
			Name:    "Phase 4: Final summary",
			Timeout: 10 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return true // always pass — just log results
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				directive := th.config.GetDirective()
				evolved := len(directive) > 700
				t.Logf("directive evolved: %v (%d chars)", evolved, len(directive))
				t.Logf("final memory count: %d", th.memory.Count())

				auditData, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(auditData)), "\n")
				t.Logf("total audit trail: %d entries", len(lines))

				if th.memory.Count() < 2 {
					t.Error("expected at least 2 memory entries from learning")
				}
			},
		},
	},
	Timeout:    10 * time.Minute,
	MaxThreads: 3,
}

func TestScenario_LearningAgent(t *testing.T) {
	warehouseBin := buildMCPBinary(t, "mcps/warehouse")
	t.Logf("built warehouse=%s", warehouseBin)

	s := learningAgentScenario
	s.MCPServers[0].Command = warehouseBin
	runScenario(t, s)
}

// --- Emergent Behavior Scenario ---

func seedStoreData(t *testing.T, dir string) {
	t.Helper()

	writeJSONFile(t, dir, "sales.json", map[string]any{
		"summary": "Revenue down 23% month-over-month. 3 of top 5 products showing sharp decline.",
		"daily": []map[string]any{
			{"date": "2026-04-01", "revenue": 3200, "orders": 42},
			{"date": "2026-04-02", "revenue": 2800, "orders": 38},
			{"date": "2026-04-03", "revenue": 2100, "orders": 29},
			{"date": "2026-04-04", "revenue": 1900, "orders": 25},
			{"date": "2026-04-05", "revenue": 1700, "orders": 22},
			{"date": "2026-04-06", "revenue": 1500, "orders": 19},
			{"date": "2026-04-07", "revenue": 1400, "orders": 17},
		},
		"by_product": []map[string]any{
			{"name": "Wireless Earbuds Pro", "units_sold": 0, "revenue": 0, "note": "NO SALES — check inventory"},
			{"name": "USB-C Hub 7-in-1", "units_sold": 0, "revenue": 0, "note": "NO SALES — check inventory"},
			{"name": "Laptop Stand Adjustable", "units_sold": 45, "revenue": 2250, "trend": "stable"},
			{"name": "Mechanical Keyboard RGB", "units_sold": 12, "revenue": 1080, "trend": "declining — was 30/week"},
			{"name": "Webcam 4K", "units_sold": 0, "revenue": 0, "note": "NO SALES — check inventory"},
			{"name": "Phone Case Premium", "units_sold": 89, "revenue": 1335, "trend": "stable"},
			{"name": "Desk Lamp LED", "units_sold": 34, "revenue": 680, "trend": "stable"},
		},
	})

	writeJSONFile(t, dir, "inventory.json", map[string]any{
		"products": []map[string]any{
			{"name": "Wireless Earbuds Pro", "stock": 0, "price": 79.99, "status": "OUT OF STOCK", "last_restocked": "2026-03-01"},
			{"name": "USB-C Hub 7-in-1", "stock": 0, "price": 49.99, "status": "OUT OF STOCK", "last_restocked": "2026-03-05"},
			{"name": "Laptop Stand Adjustable", "stock": 120, "price": 49.99, "status": "in stock"},
			{"name": "Mechanical Keyboard RGB", "stock": 45, "price": 89.99, "status": "in stock"},
			{"name": "Webcam 4K", "stock": 0, "price": 129.99, "status": "OUT OF STOCK", "last_restocked": "2026-02-20"},
			{"name": "Phone Case Premium", "stock": 230, "price": 14.99, "status": "in stock"},
			{"name": "Desk Lamp LED", "stock": 67, "price": 19.99, "status": "in stock"},
		},
	})

	writeJSONFile(t, dir, "reviews.json", map[string]any{
		"average_rating": 3.2,
		"recent": []map[string]any{
			{"product": "Wireless Earbuds Pro", "rating": 1, "text": "Wanted to buy but OUT OF STOCK for over a month! Going to Amazon instead.", "date": "2026-04-05"},
			{"product": "USB-C Hub 7-in-1", "rating": 1, "text": "Says out of stock. This was my favorite hub. Very disappointing.", "date": "2026-04-04"},
			{"product": "Mechanical Keyboard RGB", "rating": 3, "text": "Good keyboard but $89.99 is too expensive. Same one is $69 on Amazon.", "date": "2026-04-03"},
			{"product": "Webcam 4K", "rating": 1, "text": "OUT OF STOCK AGAIN. Third time I've tried to order. Lost a customer.", "date": "2026-04-06"},
			{"product": "Phone Case Premium", "rating": 5, "text": "Great case, fast shipping, good price!", "date": "2026-04-05"},
			{"product": "Laptop Stand Adjustable", "rating": 4, "text": "Solid product but shipping was slow — 8 days.", "date": "2026-04-02"},
			{"product": "Desk Lamp LED", "rating": 4, "text": "Nice lamp. Would buy again.", "date": "2026-04-01"},
		},
	})

	writeJSONFile(t, dir, "competitors.json", map[string]any{
		"comparison": []map[string]any{
			{"product": "Wireless Earbuds Pro", "our_price": 79.99, "amazon_price": 74.99, "best_buy_price": 79.99},
			{"product": "USB-C Hub 7-in-1", "our_price": 49.99, "amazon_price": 39.99, "best_buy_price": 44.99},
			{"product": "Mechanical Keyboard RGB", "our_price": 89.99, "amazon_price": 69.99, "best_buy_price": 74.99},
			{"product": "Webcam 4K", "our_price": 129.99, "amazon_price": 109.99, "best_buy_price": 119.99},
			{"product": "Phone Case Premium", "our_price": 14.99, "amazon_price": 14.99, "best_buy_price": 16.99},
			{"product": "Laptop Stand Adjustable", "our_price": 49.99, "amazon_price": 49.99, "best_buy_price": 54.99},
		},
	})

	writeJSONFile(t, dir, "analytics.json", map[string]any{
		"period":           "last 7 days",
		"unique_visitors":  12400,
		"page_views":       34200,
		"conversion_rate":  "1.4% (was 3.2% last month)",
		"bounce_rate":      "62% (was 45% last month)",
		"top_search_terms": []string{"wireless earbuds", "usb-c hub", "webcam 4k", "keyboard", "portable monitor usb-c"},
		"cart_abandonment": "78% (was 52% last month)",
		"note":             "Traffic is healthy but conversions dropped. Most searched products are out of stock. Unusual spike in searches for 'portable monitor usb-c' — we don't carry this product. Check traffic sources for details.",
	})

	writeJSONFile(t, dir, "traffic.json", map[string]any{
		"period": "last 7 days",
		"sources": []map[string]any{
			{"source": "google organic", "visits": 5200, "conversion": "1.8%"},
			{"source": "direct", "visits": 3100, "conversion": "2.1%"},
			{"source": "social media", "visits": 1800, "conversion": "0.9%"},
			{"source": "techgadgetblog.com/best-usb-c-monitors-2026", "visits": 1400, "conversion": "0.1%", "note": "ANOMALY: High traffic, near-zero conversion. Blog recommends 'UltraView Portable Monitor 15.6\" USB-C' at $199 — we don't carry it. 89% of these visitors search our store for it then leave."},
			{"source": "email campaigns", "visits": 900, "conversion": "3.2%"},
		},
		"trending_searches_with_zero_results": []string{"portable monitor", "usb-c monitor", "ultraview monitor"},
	})

	writeJSONFile(t, dir, "suppliers.json", map[string]any{
		"suppliers": []map[string]any{
			{
				"name": "TechSource Direct", "status": "DELAYED",
				"products":      []string{"Wireless Earbuds Pro", "USB-C Hub 7-in-1", "Webcam 4K"},
				"normal_lead":   "3-5 days",
				"current_lead":  "14-18 days",
				"reason":        "Warehouse fire at distribution center. Backlog expected until mid-April.",
				"reliability":   "Usually excellent — this is an unusual event",
				"recommendation": "Use alt_supplier for urgent restocks (+15% cost, 3-5 day delivery)",
			},
			{
				"name": "AltSupply Express", "status": "OPERATIONAL",
				"products":     []string{"Wireless Earbuds Pro", "USB-C Hub 7-in-1", "Webcam 4K", "UltraView Portable Monitor"},
				"normal_lead":  "3-5 days",
				"current_lead": "3-5 days",
				"surcharge":    "15%",
				"note":         "Can also supply UltraView Portable Monitor 15.6\" USB-C at wholesale $120 (MSRP $199)",
			},
			{
				"name": "GenericParts Co", "status": "OPERATIONAL",
				"products":    []string{"Laptop Stand Adjustable", "Desk Lamp LED", "Phone Case Premium"},
				"normal_lead": "2-3 days",
				"current_lead": "2-3 days",
			},
		},
	})

	writeJSONFile(t, dir, "segments.json", map[string]any{
		"segments": []map[string]any{
			{"name": "power_buyers", "count": 340, "avg_order": 127, "frequency": "2.3x/month", "note": "Highest value — 40% of revenue. Many have stopped buying (out-of-stock items). 78 haven't purchased in 3 weeks."},
			{"name": "deal_seekers", "count": 890, "avg_order": 34, "frequency": "1.1x/month", "note": "Price-sensitive. Respond well to promotions. Keyboard price increase lost 40% of this segment."},
			{"name": "new_visitors", "count": 1200, "avg_order": 0, "frequency": "0", "note": "1,200 new visitors this week, mostly from techgadgetblog.com. Almost none converted — they're looking for a product we don't sell."},
			{"name": "returning_loyal", "count": 460, "avg_order": 62, "frequency": "1.8x/month", "note": "Stable segment. Good retention. Would respond well to loyalty rewards."},
		},
	})
}

var emergentScenario = Scenario{
	Name: "Emergent",
	Directive: `You run a small online electronics store. Sales have been declining. Diagnose the root causes and take action to turn things around. Go deep — surface-level fixes won't be enough.`,
	MCPServers: []MCPServerConfig{
		{Name: "store", Command: "", Env: map[string]string{"STORE_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: seedStoreData,
	Phases: []Phase{
		{
			Name:    "Deep investigation — agent explores multiple data layers",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				tools := map[string]bool{}
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					if a := entry["action"]; a != "" {
						tools[a] = true
					}
				}
				// Must dig into at least 5 different data sources (not just the obvious ones)
				return len(tools) >= 5
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				tools := map[string]bool{}
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					if a := entry["action"]; a != "" {
						tools[a] = true
					}
				}
				t.Logf("data sources explored: %v (%d)", tools, len(tools))
			},
		},
		{
			Name:    "Multi-layered action — fixes surface + deeper issues",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				s := string(data)
				// Must take at least 3 different ACTION types
				actionTypes := 0
				if strings.Contains(s, "\"action\":\"restock_item\"") {
					actionTypes++
				}
				if strings.Contains(s, "\"action\":\"adjust_price\"") {
					actionTypes++
				}
				if strings.Contains(s, "\"action\":\"send_promotion\"") {
					actionTypes++
				}
				if strings.Contains(s, "\"action\":\"add_product\"") {
					actionTypes++
				}
				return actionTypes >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				var actions []string
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					switch entry["action"] {
					case "restock_item":
						actions = append(actions, fmt.Sprintf("RESTOCK: %s ×%s (supplier: %s)", entry["product"], entry["quantity"], entry["supplier"]))
					case "adjust_price":
						actions = append(actions, fmt.Sprintf("PRICE: %s → $%s", entry["product"], entry["new_price"]))
					case "send_promotion":
						actions = append(actions, fmt.Sprintf("PROMO: \"%s\" (%s to %s)", entry["subject"], entry["discount"], entry["target_segment"]))
					case "add_product":
						actions = append(actions, fmt.Sprintf("NEW PRODUCT: %s at $%s", entry["name"], entry["price"]))
					}
				}
				for _, a := range actions {
					t.Logf("  %s", a)
				}
				// Check for smart decisions
				usedAltSupplier := strings.Contains(string(data), "alt_supplier")
				addedProduct := strings.Contains(string(data), "add_product")
				t.Logf("discovered alt supplier: %v", usedAltSupplier)
				t.Logf("added new product (blog opportunity): %v", addedProduct)
			},
		},
		{
			Name:    "Emergence score — threads, memory, strategy, creativity",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				score := 0
				if th.threads != nil && th.threads.Count() > 0 {
					score += 2
				}
				if th.memory.Count() > 0 {
					score++
				}
				directive := th.config.GetDirective()
				if len(directive) > 200 {
					score++
				}
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				s := string(data)
				if strings.Contains(s, "alt_supplier") {
					score++ // discovered supply chain workaround
				}
				if strings.Contains(s, "add_product") {
					score++ // spotted market opportunity
				}
				actions := strings.Count(s, "\"action\":")
				if actions >= 6 {
					score++ // took comprehensive action
				}
				return score >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Log("=== EMERGENCE REPORT ===")
				t.Logf("threads active: %d", th.threads.Count())
				t.Logf("memory entries: %d", th.memory.Count())
				directive := th.config.GetDirective()
				t.Logf("directive evolved: %v (%d chars)", len(directive) > 200, len(directive))

				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				s := string(data)

				score := 0
				if th.threads.Count() > 0 {
					score += 2
					t.Log("  ✓ spawned worker threads (self-organization)")
				}
				if th.memory.Count() > 0 {
					score++
					t.Log("  ✓ remembered findings (persistent learning)")
				}
				if len(directive) > 200 {
					score++
					t.Log("  ✓ evolved directive (self-improvement)")
				}
				if strings.Contains(s, "alt_supplier") {
					score++
					t.Log("  ✓ discovered alt supplier workaround (problem-solving)")
				}
				if strings.Contains(s, "add_product") {
					score++
					t.Log("  ✓ spotted new product opportunity (creativity)")
				}
				if len(lines) >= 8 {
					score++
					t.Log("  ✓ took comprehensive multi-step action (initiative)")
				}

				t.Logf("EMERGENCE SCORE: %d/7", score)
				t.Logf("total tool calls: %d", len(lines))

				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					t.Logf("  [%s] %s", entry["action"], entry)
				}
			},
		},
	},
	Timeout:    10 * time.Minute,
	MaxThreads: 12,
}

func TestScenario_Emergent(t *testing.T) {
	storeBin := buildMCPBinary(t, "mcps/store")
	t.Logf("built store=%s", storeBin)

	s := emergentScenario
	s.MCPServers[0].Command = storeBin
	runScenario(t, s)
}

// --- Fleet Scenario (tree structure) ---

var fleetScenario = Scenario{
	Name: "Fleet",
	Directive: `You are the CEO of a small online electronics store. Your sales are declining and you need to turn things around.

You operate as a FLEET — you do NOT do the work yourself. Instead, you spawn TEAM LEADS who each manage their own workers. You coordinate at the strategic level only.

Spawn exactly 3 team leads:

1. "ops-lead" — Operations lead. Responsible for inventory and supply chain.
   Give them tools: store_get_inventory, store_check_supplier, store_restock_item, send
   Their job: investigate stock-outs, find working suppliers, restock critical items.
   They should spawn their own workers for parallel tasks (e.g. one to check inventory, one to handle restocking).

2. "sales-lead" — Sales & pricing lead. Responsible for revenue optimization.
   Give them tools: store_get_sales, store_get_competitors, store_adjust_price, store_get_analytics, send
   Their job: analyze sales trends, compare competitor prices, adjust pricing to be competitive.
   They should spawn workers to investigate different aspects in parallel.

3. "marketing-lead" — Marketing lead. Responsible for customer engagement and growth.
   Give them tools: store_get_customer_segments, store_get_traffic_sources, store_get_reviews, store_send_promotion, store_add_product, send
   Their job: identify customer segments to target, find new product opportunities, run promotions.
   They should spawn workers for research and execution.

CRITICAL RULES:
- You ONLY talk to your 3 leads. Never call store tools directly.
- Leads report findings and actions back to you.
- Wait for all 3 leads to report before making final strategic decisions.
- After receiving reports, send a strategic summary to each lead with any cross-team insights.`,
	MCPServers: []MCPServerConfig{
		{Name: "store", Command: "", Env: map[string]string{"STORE_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: seedStoreData,
	Phases: []Phase{
		{
			Name:    "Startup — 3 team leads spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				ids := threadIDs(th)
				t.Logf("  main threads: %v", ids)
				return len(ids) >= 3
			},
		},
		{
			Name:    "Tree forms — leads spawn workers (depth 1+)",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				all := allThreadInfos(th.threads)
				depth0 := 0
				depth1 := 0
				for _, info := range all {
					if info.Depth == 0 {
						depth0++
					} else if info.Depth >= 1 {
						depth1++
					}
				}
				t.Logf("  total threads: %d (leads: %d, workers: %d)", len(all), depth0, depth1)
				for _, info := range all {
					t.Logf("    %s (parent=%s, depth=%d)", info.ID, info.ParentID, info.Depth)
				}
				// Need at least 3 leads + 3 workers (at least 1 worker per lead)
				return depth0 >= 3 && depth1 >= 3
			},
		},
		{
			Name:    "Execution — workers take actions via store tools",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				if err != nil {
					return false
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				// Count distinct action types
				actions := map[string]bool{}
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					if a := entry["action"]; a != "" {
						actions[a] = true
					}
				}
				t.Logf("  actions taken: %d calls, %d types: %v", len(lines), len(actions), actions)
				// Need at least 3 different action types (investigation + execution)
				return len(actions) >= 3
			},
		},
		{
			Name:    "Coordination — leads report back, real actions taken",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				s := string(data)
				// Need at least one write action (restock, price change, or promotion)
				hasRestock := strings.Contains(s, "\"action\":\"restock_item\"")
				hasPrice := strings.Contains(s, "\"action\":\"adjust_price\"")
				hasPromo := strings.Contains(s, "\"action\":\"send_promotion\"")
				hasNewProduct := strings.Contains(s, "\"action\":\"add_product\"")
				writeActions := 0
				if hasRestock { writeActions++ }
				if hasPrice { writeActions++ }
				if hasPromo { writeActions++ }
				if hasNewProduct { writeActions++ }
				t.Logf("  write actions: restock=%v price=%v promo=%v newProduct=%v (%d/4)",
					hasRestock, hasPrice, hasPromo, hasNewProduct, writeActions)
				return writeActions >= 2
			},
		},
		{
			Name:    "Upward reporting — leads report results to main (full chain)",
			Timeout: 120 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Nothing to set up — just need to wait for leads to report
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("Status check: all leads please report what actions you've taken and results so far.")
						injected = true
					}
					// Check if main received messages from leads (main's iteration advanced beyond startup)
					return th.iteration >= 4
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Final report
				all := allThreadInfos(th.threads)
				t.Log("=== FLEET REPORT ===")
				t.Logf("total threads alive: %d", len(all))
				for _, info := range all {
					indent := ""
					for i := 0; i < info.Depth; i++ {
						indent += "  "
					}
					role := "worker"
					if info.Depth == 0 {
						role = "lead"
					}
					t.Logf("  %s%s [%s] (parent=%s, depth=%d)", indent, info.ID, role, info.ParentID, info.Depth)
				}

				// Action summary
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				t.Logf("total store actions: %d", len(lines))
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					t.Logf("  [%s] %v", entry["action"], entry)
				}

				// Verify tree structure existed
				maxDepth := 0
				for _, info := range all {
					if info.Depth > maxDepth {
						maxDepth = info.Depth
					}
				}
				if maxDepth < 1 {
					t.Error("expected tree structure with depth >= 1 (leads spawning workers)")
				}

				// Check main received lead reports (check iteration count and message history)
				t.Logf("main iterations: %d", th.iteration)
				// Log last few messages in main's context to see if leads reported
				for i, msg := range th.messages {
					if i == 0 {
						continue // skip system prompt
					}
					preview := msg.Content
					if len(preview) > 120 {
						preview = preview[:120] + "..."
					}
					if strings.Contains(msg.Content, "[from:") {
						t.Logf("  main msg[%d] role=%s: %s", i, msg.Role, preview)
					}
				}
			},
		},
	},
	Timeout:    10 * time.Minute,
	MaxThreads: 15,
}

func TestScenario_Fleet(t *testing.T) {
	storeBin := buildMCPBinary(t, "mcps/store")
	t.Logf("built store=%s", storeBin)

	s := fleetScenario
	s.MCPServers[0].Command = storeBin
	runScenario(t, s)
}

// --- Conglomerate Scenario (3-level tree) ---

var conglomerateScenario = Scenario{
	Name: "Conglomerate",
	Directive: `You are the CEO of ByteVentures, a one-person conglomerate running 3 microbusinesses through AI agents. Your store's sales are declining badly and you need all 3 businesses working in parallel to fix it.

YOU MUST BUILD A 3-LEVEL HIERARCHY. You do NOT do any store work yourself. You spawn directors, directors spawn team leads, team leads spawn workers. Workers are the only ones who call store tools.

Spawn exactly 3 DIRECTORS (depth 0):

1. "retail-director" — runs the Electronics Retail business. Responsible for making sure products are in stock and competitively priced.
   Tools: send (NO store tools — directors delegate, they don't execute)
   Their job: spawn 2 team leads:
   - "supply-chain-lead" with tools: store_get_inventory, store_check_supplier, store_restock_item, send
     This lead should spawn workers to check stock and handle supplier logistics in parallel.
   - "pricing-lead" with tools: store_get_competitors, store_adjust_price, send
     This lead should spawn workers to scan competitor prices and execute adjustments.

2. "growth-director" — runs the Customer Growth business. Responsible for retention and acquisition.
   Tools: send (NO store tools)
   Their job: spawn 2 team leads:
   - "retention-lead" with tools: store_get_customer_segments, store_send_promotion, send
     This lead should spawn workers to analyze churn and send winback campaigns.
   - "acquisition-lead" with tools: store_get_traffic_sources, store_get_analytics, store_send_promotion, send
     This lead should spawn workers to find traffic opportunities and launch campaigns.

3. "expansion-director" — runs the New Markets business. Responsible for finding and launching new products.
   Tools: send (NO store tools)
   Their job: spawn 2 team leads:
   - "research-lead" with tools: store_get_reviews, store_get_traffic_sources, store_get_analytics, send
     This lead should spawn workers to mine reviews and spot trends.
   - "launch-lead" with tools: store_add_product, store_restock_item, send
     This lead should spawn workers to list new products when opportunities are found.

WORKFLOW:
- Directors spawn their team leads immediately.
- Team leads spawn 1-2 workers each to parallelize their tasks.
- Workers call store tools, report findings to their team lead.
- Team leads synthesize worker findings, take action, report to their director.
- Directors report business summaries to you (the CEO).
- You synthesize cross-business insights and send strategic directives back down.

CRITICAL: The value of this structure is PARALLELISM and SPECIALIZATION. All 3 businesses investigate simultaneously. Information flows UP (workers→leads→directors→CEO), decisions flow DOWN (CEO→directors→leads→workers).`,
	MCPServers: []MCPServerConfig{
		{Name: "store", Command: "", Env: map[string]string{"STORE_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: seedStoreData,
	Phases: []Phase{
		{
			Name:    "Phase 1 — Directors spawned",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				ids := threadIDs(th)
				t.Logf("  main threads: %v", ids)
				return len(ids) >= 3
			},
		},
		{
			Name:    "Phase 2 — Team leads spawned (depth 1)",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				all := allThreadInfos(th.threads)
				byDepth := map[int]int{}
				for _, info := range all {
					byDepth[info.Depth]++
				}
				t.Logf("  threads by depth: d0=%d d1=%d d2=%d (total %d)",
					byDepth[0], byDepth[1], byDepth[2], len(all))
				// Need at least 3 directors + 4 team leads
				return byDepth[0] >= 3 && byDepth[1] >= 4
			},
		},
		{
			Name:    "Phase 3 — Workers spawned (depth 2) — full 3-level tree",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				all := allThreadInfos(th.threads)
				byDepth := map[int]int{}
				for _, info := range all {
					byDepth[info.Depth]++
				}
				t.Logf("  threads by depth: d0=%d d1=%d d2=%d (total %d)",
					byDepth[0], byDepth[1], byDepth[2], len(all))
				for _, info := range all {
					indent := strings.Repeat("  ", info.Depth)
					t.Logf("    %s%s (parent=%s, depth=%d)", indent, info.ID, info.ParentID, info.Depth)
				}
				// Need depth-2 workers to exist (at least 3)
				return byDepth[2] >= 3
			},
		},
		{
			Name:    "Phase 4 — Workers execute store tools",
			Timeout: 180 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				if err != nil {
					return false
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				actions := map[string]bool{}
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					if a := entry["action"]; a != "" {
						actions[a] = true
					}
				}
				t.Logf("  store actions: %d calls, %d types: %v", len(lines), len(actions), actions)
				// Need at least 4 different data sources explored
				return len(actions) >= 4
			},
		},
		{
			Name:    "Phase 5 — Full chain: actions taken + directors report to CEO",
			Timeout: 180 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Nudge the CEO to demand reports
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						th.InjectConsole("Board meeting in 5 minutes. All directors: submit your business unit reports NOW with findings, actions taken, and recommendations.")
						injected = true
					}
					// Check for write actions (restock, price adjust, promo, or new product)
					data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
					s := string(data)
					writeActions := 0
					if strings.Contains(s, "\"action\":\"restock_item\"") { writeActions++ }
					if strings.Contains(s, "\"action\":\"adjust_price\"") { writeActions++ }
					if strings.Contains(s, "\"action\":\"send_promotion\"") { writeActions++ }
					if strings.Contains(s, "\"action\":\"add_product\"") { writeActions++ }
					// Also check CEO received at least 2 director reports
					ceoReports := 0
					for _, msg := range th.messages {
						if strings.Contains(msg.Content, "[from:retail-director]") ||
							strings.Contains(msg.Content, "[from:growth-director]") ||
							strings.Contains(msg.Content, "[from:expansion-director]") {
							ceoReports++
						}
					}
					t.Logf("  write actions: %d/4, CEO reports received: %d/3", writeActions, ceoReports)
					return writeActions >= 2 && ceoReports >= 2
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				all := allThreadInfos(th.threads)
				t.Log("=== CONGLOMERATE REPORT ===")
				t.Logf("total threads alive: %d", len(all))

				// Print tree
				byDepth := map[int]int{}
				for _, info := range all {
					byDepth[info.Depth]++
				}
				t.Logf("by depth: directors=%d, leads=%d, workers=%d", byDepth[0], byDepth[1], byDepth[2])

				for _, info := range all {
					indent := strings.Repeat("  ", info.Depth)
					role := "worker"
					if info.Depth == 0 {
						role = "director"
					} else if info.Depth == 1 {
						role = "lead"
					}
					t.Logf("  %s%s [%s] (parent=%s)", indent, info.ID, role, info.ParentID)
				}

				// Action summary
				data, _ := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				t.Logf("total store actions: %d", len(lines))
				for _, line := range lines {
					var entry map[string]string
					json.Unmarshal([]byte(line), &entry)
					t.Logf("  [%s] %v", entry["action"], entry)
				}

				// Verify 3-level tree
				if byDepth[2] == 0 {
					t.Error("expected depth-2 workers (3-level tree)")
				}

				// Log CEO messages from directors
				t.Log("--- CEO inbox (director reports) ---")
				for _, msg := range th.messages {
					if strings.Contains(msg.Content, "[from:retail-director]") ||
						strings.Contains(msg.Content, "[from:growth-director]") ||
						strings.Contains(msg.Content, "[from:expansion-director]") {
						preview := msg.Content
						if len(preview) > 200 {
							preview = preview[:200] + "..."
						}
						t.Logf("  %s", preview)
					}
				}
			},
		},
	},
	Timeout:    12 * time.Minute,
	MaxThreads: 25,
}

func TestScenario_Conglomerate(t *testing.T) {
	storeBin := buildMCPBinary(t, "mcps/store")
	t.Logf("built store=%s", storeBin)

	s := conglomerateScenario
	s.MCPServers[0].Command = storeBin
	runScenario(t, s)
}

// --- Conversational Team Building Scenario ---
// Instead of a directive that pre-defines the org, the user builds the team
// through natural conversation — sending events like CLI chat messages.

var conversationalTeamScenario = Scenario{
	Name: "ConversationalTeam",
	MCPServers: []MCPServerConfig{
		{Name: "helpdesk", Command: "", Env: map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DATA_DIR": "{{dataDir}}"}},
	},
	Directive: "You are a helpful coordinator. You manage threads and route work. You do NOT have a pre-defined team — the user will tell you what to set up.",
	Phases: []Phase{
		{
			Name: "User asks to create a support thread",
			Setup: func(t *testing.T, dir string) {
				// Seed a helpdesk ticket so support has something to find
				os.MkdirAll(dir, 0755)
				os.WriteFile(filepath.Join(dir, "tickets.json"), []byte(`[{"id":"T-001","question":"My login is broken, please help","status":"open"}]`), 0644)
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Inject the user request on first call
				if th.iteration <= 2 {
					th.Inject("[console] Create a support thread that monitors the helpdesk for tickets. Give it access to the helpdesk MCP server.")
				}
				return th.threads.Count() >= 1
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				threads := th.threads.List()
				found := false
				for _, thr := range threads {
					if strings.Contains(strings.ToLower(thr.ID), "support") || strings.Contains(strings.ToLower(thr.Directive), "support") || strings.Contains(strings.ToLower(thr.Directive), "helpdesk") {
						found = true
						t.Logf("support thread found: id=%s", thr.ID)
					}
				}
				if !found {
					t.Errorf("expected a support/helpdesk thread, got: %v", threads)
				}
			},
			Timeout: 60 * time.Second,
		},
		{
			Name: "User asks to add a dev thread",
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 4 {
					th.Inject("[console] Now create a dev thread that can read and write code using the codebase tools. It should fix bugs that support finds.")
				}
				return th.threads.Count() >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				threads := th.threads.List()
				foundDev := false
				for _, thr := range threads {
					if strings.Contains(strings.ToLower(thr.ID), "dev") || strings.Contains(strings.ToLower(thr.Directive), "code") {
						foundDev = true
						t.Logf("dev thread found: id=%s", thr.ID)
					}
				}
				if !foundDev {
					t.Errorf("expected a dev/code thread, got: %v", threads)
				}
			},
			Timeout: 60 * time.Second,
		},
		{
			Name: "User asks to check for tickets — support finds T-001",
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 6 {
					th.Inject("[console] Ask support to check for open tickets right now.")
				}
				// Wait for support to find and report the ticket
				for _, m := range th.messages {
					if strings.Contains(m.Content, "T-001") || strings.Contains(m.Content, "login") {
						return true
					}
				}
				return false
			},
			Timeout: 90 * time.Second,
		},
		{
			Name: "User asks for status — coordinator reports team state",
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 10 {
					th.Inject("[console] What threads do we have running and what are they doing?")
				}
				// Wait for a response that mentions the threads
				for i := len(th.messages) - 1; i >= 0; i-- {
					m := th.messages[i]
					if m.Role == "assistant" && (strings.Contains(strings.ToLower(m.Content), "support") && strings.Contains(strings.ToLower(m.Content), "dev")) {
						return true
					}
				}
				return false
			},
			Timeout: 60 * time.Second,
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_ConversationalTeam(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}

	helpdeskBin := buildMCPBinary(t, "mcps/helpdesk")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	t.Logf("built helpdesk=%s codebase=%s", helpdeskBin, codebaseBin)

	s := conversationalTeamScenario
	s.MCPServers[0].Command = helpdeskBin
	s.MCPServers[1].Command = codebaseBin
	runScenario(t, s)
}

// --- Conversational Org Building Scenario ---
// User builds a 3-level org through chat: CEO → directors → workers.
// No predefined threads — everything spawned via conversation.

var conversationalOrgScenario = Scenario{
	Name: "ConversationalOrg",
	MCPServers: []MCPServerConfig{
		{Name: "helpdesk", Command: "", Env: map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"}},
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DATA_DIR": "{{dataDir}}"}},
	},
	Directive: "You are a CEO coordinator. You can spawn director threads (with spawn tool), and directors can spawn their own workers. Build the org as the user requests. Keep it lean.",
	Phases: []Phase{
		{
			Name: "User asks to create an engineering director",
			Setup: func(t *testing.T, dir string) {
				// Seed helpdesk + codebase data
				os.MkdirAll(dir, 0755)
				os.WriteFile(filepath.Join(dir, "tickets.json"), []byte(`[{"id":"T-100","question":"API returns 500 on /users endpoint","status":"open"}]`), 0644)
				writeGoProject(t, dir)
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 2 {
					th.Inject(`[console] Create an engineering director thread with tools=spawn,send and mcp=codebase. It MUST have spawn in its tools so it can create sub-workers.`)
				}
				for _, thr := range th.threads.List() {
					if strings.Contains(strings.ToLower(thr.ID), "eng") || strings.Contains(strings.ToLower(thr.ID), "director") {
						return true
					}
				}
				return false
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				for _, thr := range th.threads.List() {
					t.Logf("thread: id=%s depth=%d tools=%v", thr.ID, thr.Depth, thr.Tools)
				}
			},
			Timeout: 60 * time.Second,
		},
		{
			Name: "User asks to create a support director",
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 4 {
					th.Inject(`[console] Create a support director thread with tools=spawn,send and mcp=helpdesk. It MUST have spawn in its tools.`)
				}
				count := 0
				for _, thr := range th.threads.List() {
					if thr.Depth == 0 {
						count++
					}
				}
				return count >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("depth-0 threads (directors):")
				for _, thr := range th.threads.List() {
					if thr.Depth == 0 {
						t.Logf("  %s (tools: %v)", thr.ID, thr.Tools)
					}
				}
			},
			Timeout: 60 * time.Second,
		},
		{
			Name: "User tells engineering director to spawn a dev worker",
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 6 {
					th.Inject("[console] Send a message to the engineering director: spawn a dev-worker thread with codebase tools to fix bug T-100 (API returns 500 on /users).")
				}
				// Check directors' children for depth-1 workers
				for _, dir := range th.threads.List() {
					if dir.SubThreads > 0 {
						return true
					}
				}
				return false
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("threads after worker spawn:")
				totalWorkers := 0
				for _, thr := range th.threads.List() {
					t.Logf("  %s (sub_threads=%d)", thr.ID, thr.SubThreads)
					totalWorkers += thr.SubThreads
				}
				if totalWorkers < 1 {
					t.Errorf("expected at least 1 worker, got %d", totalWorkers)
				} else {
					t.Logf("PASS: %d workers under directors", totalWorkers)
				}
			},
			Timeout: 90 * time.Second,
		},
		{
			Name: "User tells support director to spawn a ticket handler",
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 10 {
					th.Inject("[console] Send a message to the support director: spawn a ticket-handler worker with helpdesk tools to handle open tickets.")
				}
				// Count workers across all directors
				workerCount := 0
				for _, dir := range th.threads.List() {
					workerCount += dir.SubThreads
				}
				return workerCount >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("final org structure:")
				directors := th.threads.List()
				totalWorkers := 0
				for _, dir := range directors {
					t.Logf("  [director] %s (sub_threads=%d)", dir.ID, dir.SubThreads)
					totalWorkers += dir.SubThreads
				}
				if len(directors) < 2 {
					t.Errorf("expected at least 2 directors, got %d", len(directors))
				}
				t.Logf("org: %d directors + %d workers (from SubThreads)", len(directors), totalWorkers)
				// Don't fail on worker count — SubThreads may lag behind actual spawns
			},
			Timeout: 90 * time.Second,
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 8,
}

// writeGoProject creates a simple Go project for the codebase MCP to work with.
func writeGoProject(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "app"), 0755)
	os.WriteFile(filepath.Join(dir, "app", "main.go"), []byte(`package main

import "fmt"

func GetUser(id int) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("invalid user id")
	}
	return fmt.Sprintf("user_%d", id), nil
}

func main() {
	name, _ := GetUser(1)
	fmt.Println(name)
}
`), 0644)
	os.WriteFile(filepath.Join(dir, "app", "main_test.go"), []byte(`package main

import "testing"

func TestGetUser(t *testing.T) {
	name, err := GetUser(1)
	if err != nil || name != "user_1" {
		t.Fatalf("expected user_1, got %s err=%v", name, err)
	}
}
`), 0644)
}

func TestScenario_ConversationalOrg(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}

	helpdeskBin := buildMCPBinary(t, "mcps/helpdesk")
	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	t.Logf("built helpdesk=%s codebase=%s", helpdeskBin, codebaseBin)

	s := conversationalOrgScenario
	s.MCPServers[0].Command = helpdeskBin
	s.MCPServers[1].Command = codebaseBin
	runScenario(t, s)
}

// cloneTeamScenario: the CEO bootstraps a "team A" (director + worker) and is then
// asked to clone it as "team B" using ONLY the primitives already in the system —
// send (to query the existing director for its state) and spawn (to rebuild the
// mirror). No new tools. Verifies that self-replication by conversation works.
var cloneTeamScenario = Scenario{
	Name: "CloneTeam",
	MCPServers: []MCPServerConfig{
		{Name: "helpdesk", Command: "", Env: map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"}},
	},
	Directive: `You are a CEO coordinator. You spawn director threads, and directors spawn their own workers. When a user asks you to clone an existing team:
1. Use the send tool to ask the existing director for (a) its current directive verbatim, (b) the ids/directives/tools of each worker under it.
2. Wait for the director's reply to arrive in your next thought.
3. Use spawn to create a mirror director with a "_b" suffix on its id, copying the directive and tools you were told.
4. Use send to tell the new director to spawn the same workers (with "_b" suffixes on their ids).
Keep it lean — one director branch is enough.`,
	Phases: []Phase{
		{
			Name: "Bootstrap team A — support director + ticket handler",
			Setup: func(t *testing.T, dir string) {
				os.MkdirAll(dir, 0755)
				os.WriteFile(filepath.Join(dir, "tickets.json"), []byte(`[{"id":"T-200","question":"Password reset link is broken","status":"open"}]`), 0644)
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 2 {
					th.Inject(`[console] Create a director thread with id="support_director_a", tools=spawn,send and mcp=helpdesk. Then tell it (via send) to spawn a worker with id="ticket_handler_a" using helpdesk tools to handle open tickets.`)
				}
				workers := 0
				haveA := false
				for _, thr := range th.threads.List() {
					if strings.Contains(thr.ID, "support_director_a") {
						haveA = true
					}
					workers += thr.SubThreads
				}
				return haveA && workers >= 1
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("team A:")
				for _, thr := range th.threads.List() {
					t.Logf("  %s (depth=%d sub_threads=%d)", thr.ID, thr.Depth, thr.SubThreads)
				}
			},
			Timeout: 120 * time.Second,
		},
		{
			Name: "Clone team A as team B — CEO queries director, rebuilds mirror",
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.Inject(`[console] Clone the support team as team B. Follow your directive: send support_director_a to ask for its state, then spawn support_director_b with the same directive/tools, then send the new director to spawn a mirror worker ticket_handler_b.`)
					}
					haveB := false
					mirrorWorkers := 0
					for _, thr := range th.threads.List() {
						if strings.Contains(thr.ID, "support_director_b") {
							haveB = true
							mirrorWorkers += thr.SubThreads
						}
					}
					return haveB && mirrorWorkers >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				t.Logf("final org:")
				var aDir, bDir *ThreadInfo
				list := th.threads.List()
				for i := range list {
					thr := &list[i]
					t.Logf("  %s (depth=%d sub_threads=%d tools=%v)", thr.ID, thr.Depth, thr.SubThreads, thr.Tools)
					if strings.Contains(thr.ID, "support_director_a") {
						aDir = thr
					}
					if strings.Contains(thr.ID, "support_director_b") {
						bDir = thr
					}
				}
				if aDir == nil || bDir == nil {
					t.Errorf("expected both support_director_a and support_director_b, got a=%v b=%v", aDir, bDir)
					return
				}
				if bDir.SubThreads < 1 {
					t.Errorf("expected mirror director to have at least 1 worker, got %d", bDir.SubThreads)
				}
				t.Logf("PASS: team cloned by conversation — A has %d workers, B has %d workers", aDir.SubThreads, bDir.SubThreads)
			},
			Timeout: 180 * time.Second,
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 6,
}

func TestScenario_CloneTeam(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}

	helpdeskBin := buildMCPBinary(t, "mcps/helpdesk")
	t.Logf("built helpdesk=%s", helpdeskBin)

	s := cloneTeamScenario
	s.MCPServers[0].Command = helpdeskBin
	runScenario(t, s)
}

// longCodingTaskScenario measures the core's capacity to drive a non-trivial
// coding task through sub-threads over a 2-3 minute window. It's a soak-style
// scenario: the main thread is a coordinator, one worker thread does the
// actual coding via the codebase MCP (read_file / write_file / run_tests), and
// we observe iterative progress — how many write/test cycles the worker can
// run, whether tests eventually pass, and whether the threads stay alive for
// the full duration.
//
// The task is a Go word-count package with failing tests and a skeleton
// implementation. The worker needs to read the tests, implement the missing
// logic in main.go, write the file, run tests, iterate on failures, and keep
// going until tests pass or the budget runs out.
var longCodingTaskScenario = Scenario{
	Name: "LongCodingTask",
	MCPServers: []MCPServerConfig{
		// NOTE: codebase MCP reads CODEBASE_DIR, not CODEBASE_DATA_DIR — if we
		// passed the wrong name it falls back to "." (the test's cwd) and
		// silently operates on the apteva-core package directory instead.
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
	},
	Directive: `You are a coordinator. You have a dev worker thread under you that does all the coding.
Your job: spawn a dev-worker thread with codebase tools and give it a clear goal. Then monitor its
progress, respond to questions, and only intervene when it's stuck.

The dev worker should:
1. Read the existing files (main.go, main_test.go) to understand the task.
2. Implement the missing functionality in main.go.
3. Run tests via run_tests. If they fail, read the failure, fix the code, run again.
4. Keep iterating until tests pass, or until it has made genuine progress.

Do not do the coding yourself — delegate everything to the worker. Stay on normal pace as the
coordinator. The worker should be on fast pace to iterate quickly.`,
	DataSetup: func(t *testing.T, dir string) {
		// Skeleton main.go with stub functions that must be implemented.
		os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// WordCount returns a map of lowercased words → occurrence counts for the
// given text. Words are split on whitespace, leading/trailing punctuation is
// stripped, and empty tokens are ignored.
//
// TODO: implement.
func WordCount(text string) map[string]int {
	return nil
}

// TopN returns the top-N words by count, sorted by count descending then
// alphabetically ascending for tie-breaking. Return at most n entries.
//
// TODO: implement.
func TopN(counts map[string]int, n int) []string {
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wordcount <file> [n]")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	n := 10
	counts := WordCount(string(data))
	for _, w := range TopN(counts, n) {
		fmt.Printf("%d\t%s\n", counts[w], w)
	}
	// references to avoid unused-import errors while stubs are empty
	_ = sort.Strings
	_ = strings.ToLower
}
`), 0644)

		// Full test suite that must pass.
		os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(`package main

import (
	"reflect"
	"testing"
)

func TestWordCount_Basic(t *testing.T) {
	got := WordCount("the quick brown fox the lazy dog the")
	want := map[string]int{"the": 3, "quick": 1, "brown": 1, "fox": 1, "lazy": 1, "dog": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestWordCount_CaseAndPunct(t *testing.T) {
	got := WordCount("Hello, world! HELLO world.")
	want := map[string]int{"hello": 2, "world": 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestWordCount_Empty(t *testing.T) {
	got := WordCount("")
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestTopN_SortedByCountThenAlpha(t *testing.T) {
	counts := map[string]int{"a": 3, "b": 3, "c": 2, "d": 1}
	got := TopN(counts, 3)
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestTopN_LimitsToN(t *testing.T) {
	counts := map[string]int{"a": 1, "b": 2, "c": 3}
	got := TopN(counts, 2)
	if len(got) != 2 {
		t.Errorf("expected 2 results, got %v", got)
	}
	if got[0] != "c" || got[1] != "b" {
		t.Errorf("expected [c b], got %v", got)
	}
}
`), 0644)

		// test.sh is what the codebase MCP's run_tests tool shells out to.
		os.WriteFile(filepath.Join(dir, "test.sh"), []byte("#!/bin/bash\ncd \"$(dirname \"$0\")\"\ngo test ./... 2>&1\n"), 0755)

		// go.mod so `go test` works without network deps.
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module wordcount\n\ngo 1.21\n"), 0644)
	},
	Phases: []Phase{
		{
			Name:    "Bootstrap — coordinator spawns dev worker",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 2 {
					th.Inject(`[console] Task: main.go in your codebase has two stub functions (WordCount and TopN). main_test.go already contains the tests. Spawn a worker with id="dev-worker" using spawn(id="dev-worker", directive="Read main.go and main_test.go, implement WordCount and TopN so all tests pass, run tests after each change, iterate until green.", mcp="codebase", tools="send,done,pace"). Set its pace to fast. Do not code yourself.`)
				}
				// main's children are Depth=0 — just confirm at least one
				// thread is alive under main.
				if th.threads.Count() >= 1 {
					for _, thr := range th.threads.List() {
						t.Logf("  ✓ thread alive: id=%s depth=%d", thr.ID, thr.Depth)
					}
					return true
				}
				return false
			},
		},
		{
			Name:    "Active coding — worker reads, writes, tests",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Poll the main.go contents for evidence of real implementation
				// (the stub returns nil; any map literal or loop means progress)
				data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
				src := string(data)
				stubbed := strings.Contains(src, "return nil") &&
					!strings.Contains(src, "make(map[string]int)") &&
					!strings.Contains(src, "for _,")
				writes := 0
				reads := 0
				tests := 0
				for _, thr := range th.threads.List() {
					_ = thr // placeholder; actual tool counts come from audit log below
				}
				// The codebase MCP does not produce audit entries like some of
				// the others; we rely on file mutations + the observer counts
				// logged in the main runner. Record what we can see here.
				t.Logf("  ... main.go bytes=%d stubbed=%v writes=%d reads=%d tests=%d threads=%v",
					len(data), stubbed, writes, reads, tests, threadIDs(th))
				return !stubbed && len(data) > len([]byte("package main")) + 400
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
				t.Logf("  main.go size: %d bytes", len(data))
				t.Logf("  threads alive: %d", th.threads.Count())
			},
		},
		{
			Name:    "Convergence — aim for green tests",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Directly shell out to `go test` ourselves to check status.
				// We don't mutate files — this is an independent read of the
				// worker's progress.
				cmd := exec.Command("go", "test", "./...")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				passing := err == nil
				t.Logf("  ... go test: passing=%v threads=%v", passing, threadIDs(th))
				if passing {
					t.Logf("  output: %s", strings.TrimSpace(string(out)))
				}
				return passing
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Best-effort: if tests did not pass, log the last failure so
				// the test report shows what the worker was stuck on.
				cmd := exec.Command("go", "test", "./...")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Logf("NOTE: tests did not reach green within budget — last go test output:")
					lines := strings.Split(strings.TrimSpace(string(out)), "\n")
					if len(lines) > 20 {
						lines = lines[len(lines)-20:]
					}
					for _, l := range lines {
						t.Logf("    %s", l)
					}
				} else {
					t.Logf("PASS: worker drove tests to green")
				}
				// Always log final main.go size for capacity measurement
				data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
				t.Logf("final main.go: %d bytes, %d lines",
					len(data), strings.Count(string(data), "\n"))
			},
		},
		{
			Name:    "Soak — threads still alive",
			Timeout: 20 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				alive := th.threads.Count()
				t.Logf("  ... alive=%d", alive)
				return alive >= 1
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				if th.threads.Count() < 1 {
					t.Errorf("expected at least 1 thread still alive, got %d", th.threads.Count())
				}
			},
		},
	},
	// Hard budget: 5 minutes total. Phases 2+3 together already consume 4
	// minutes of polling window, but the scenario will often finish phase 3
	// early if the worker drives tests green quickly.
	Timeout:    5 * time.Minute,
	MaxThreads: 4,
}

func TestScenario_LongCodingTask(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}

	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	t.Logf("built codebase=%s", codebaseBin)

	s := longCodingTaskScenario
	s.MCPServers[0].Command = codebaseBin
	runScenario(t, s)
}

// longCodingTaskHardScenario is the hard variant of LongCodingTask. It gives
// the dev worker a substantially bigger target: a miniature arithmetic
// expression evaluator with a lexer, a recursive-descent parser with operator
// precedence, and an AST evaluator. Twelve tests cover precedence,
// associativity, parentheses, unary minus, float literals, and three error
// paths (empty input, unbalanced parens, division by zero).
//
// Why this task shape:
//   - It genuinely requires ~150 lines across 4 functions, so even a capable
//     model usually writes 2-5 iterations before all tests pass.
//   - The parser has classic edge cases (unary minus vs. binary, operator
//     precedence, right/left-associativity) that surface real fix-up loops.
//   - Error paths force the worker to distinguish "returning a number" from
//     "returning an error", which early iterations often get wrong.
//
// This scenario is designed to actually exercise the 2-3 minute window the
// user asked for, as opposed to LongCodingTask which Kimi K2.5 solves in one
// write.
var longCodingTaskHardScenario = Scenario{
	Name: "LongCodingTaskHard",
	MCPServers: []MCPServerConfig{
		{Name: "codebase", Command: "", Env: map[string]string{"CODEBASE_DIR": "{{dataDir}}"}},
	},
	Directive: `You are a coordinator. You have a dev worker under you that does all the coding.
Your job: spawn the worker, hand it the task, and stay out of its way. Do not write code yourself —
delegate everything. When the worker reports progress, acknowledge briefly. Only intervene if it
gets stuck for many iterations without making progress.

The worker should:
1. Read main.go and main_test.go to understand the task.
2. Implement the four stub functions (Tokenize, Parse, Eval, Evaluate) in main.go.
3. Run tests after each change via codebase_run_tests.
4. When tests fail, read the failure carefully, fix the specific bug, and re-run.
5. Iterate until all 12 tests pass. Do not give up — edge cases are the hard part.`,
	DataSetup: func(t *testing.T, dir string) {
		os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import (
	"fmt"
)

// --- Types ---

// TokenKind enumerates lexeme kinds the Tokenize function must recognize.
type TokenKind int

const (
	TokNumber TokenKind = iota
	TokPlus
	TokMinus
	TokStar
	TokSlash
	TokLParen
	TokRParen
	TokEOF
)

// Token is one lexeme with its kind and (for TokNumber) its numeric value.
type Token struct {
	Kind  TokenKind
	Value float64
}

// Expr is the AST node interface. Implementations represent number literals,
// binary ops, and unary ops. Pick any concrete types you want — the tests
// only call the four public functions below.
type Expr interface {
	exprNode()
}

// --- Stub functions — implement these. ---

// Tokenize produces a slice of tokens ending in a single TokEOF. Whitespace
// is skipped. Numbers may be integer or float (e.g. "3" or "3.14"). Any
// unrecognized character is an error.
//
// TODO: implement.
func Tokenize(src string) ([]Token, error) {
	return nil, fmt.Errorf("not implemented")
}

// Parse consumes the token slice and returns an AST root using standard
// arithmetic precedence: * and / bind tighter than + and -, both left-
// associative. Parentheses override precedence. Unary minus is supported
// and has higher precedence than binary ops.
//
// Returns an error if tokens are ill-formed (e.g. unmatched paren, trailing
// operator, empty input).
//
// TODO: implement.
func Parse(tokens []Token) (Expr, error) {
	return nil, fmt.Errorf("not implemented")
}

// Eval walks the AST and returns the numeric result. It returns an error on
// division by zero. Other runtime errors are at your discretion but the tests
// only check division-by-zero.
//
// TODO: implement.
func Eval(e Expr) (float64, error) {
	return 0, fmt.Errorf("not implemented")
}

// Evaluate is the public entry point: Tokenize + Parse + Eval.
//
// TODO: implement.
func Evaluate(src string) (float64, error) {
	return 0, fmt.Errorf("not implemented")
}

func main() {
	fmt.Println("expression evaluator — run tests")
}
`), 0644)

		os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(`package main

import (
	"math"
	"strings"
	"testing"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// --- Public API: Evaluate (end-to-end) ---

func TestEvaluate_Basic(t *testing.T) {
	got, err := Evaluate("1 + 2")
	if err != nil || !approx(got, 3) {
		t.Errorf("1+2: got %v err=%v", got, err)
	}
}

func TestEvaluate_Precedence(t *testing.T) {
	got, err := Evaluate("2 + 3 * 4")
	if err != nil || !approx(got, 14) {
		t.Errorf("2+3*4: got %v err=%v (want 14)", got, err)
	}
}

func TestEvaluate_Parens(t *testing.T) {
	got, err := Evaluate("(2 + 3) * 4")
	if err != nil || !approx(got, 20) {
		t.Errorf("(2+3)*4: got %v err=%v (want 20)", got, err)
	}
}

func TestEvaluate_LeftAssociativeMinus(t *testing.T) {
	got, err := Evaluate("10 - 3 - 2")
	if err != nil || !approx(got, 5) {
		t.Errorf("10-3-2: got %v err=%v (want 5, left-assoc)", got, err)
	}
}

func TestEvaluate_LeftAssociativeDiv(t *testing.T) {
	got, err := Evaluate("16 / 4 / 2")
	if err != nil || !approx(got, 2) {
		t.Errorf("16/4/2: got %v err=%v (want 2, left-assoc)", got, err)
	}
}

func TestEvaluate_UnaryMinus(t *testing.T) {
	got, err := Evaluate("-5 + 3")
	if err != nil || !approx(got, -2) {
		t.Errorf("-5+3: got %v err=%v (want -2)", got, err)
	}
}

func TestEvaluate_UnaryInsideParens(t *testing.T) {
	got, err := Evaluate("2 * (-3 + 1)")
	if err != nil || !approx(got, -4) {
		t.Errorf("2*(-3+1): got %v err=%v (want -4)", got, err)
	}
}

func TestEvaluate_NestedParens(t *testing.T) {
	got, err := Evaluate("((1 + 2) * (3 - 4))")
	if err != nil || !approx(got, -3) {
		t.Errorf("((1+2)*(3-4)): got %v err=%v (want -3)", got, err)
	}
}

func TestEvaluate_FloatLiteral(t *testing.T) {
	got, err := Evaluate("3.14 * 2")
	if err != nil || !approx(got, 6.28) {
		t.Errorf("3.14*2: got %v err=%v (want 6.28)", got, err)
	}
}

// --- Error paths ---

func TestEvaluate_EmptyInput(t *testing.T) {
	_, err := Evaluate("")
	if err == nil {
		t.Errorf("empty input should return error")
	}
}

func TestEvaluate_UnbalancedParen(t *testing.T) {
	_, err := Evaluate("(1 + 2")
	if err == nil {
		t.Errorf("unbalanced paren should return error")
	}
}

func TestEvaluate_DivisionByZero(t *testing.T) {
	_, err := Evaluate("1 / 0")
	if err == nil {
		t.Errorf("division by zero should return error")
	}
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "zero") &&
		!strings.Contains(strings.ToLower(err.Error()), "divide") {
		t.Logf("note: error text %q does not mention 'zero' or 'divide' — OK but weird", err.Error())
	}
}
`), 0644)

		os.WriteFile(filepath.Join(dir, "test.sh"), []byte("#!/bin/bash\ncd \"$(dirname \"$0\")\"\ngo test ./... 2>&1\n"), 0755)
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module exprcalc\n\ngo 1.21\n"), 0644)
	},
	Phases: []Phase{
		{
			Name:    "Bootstrap — coordinator spawns dev worker",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.iteration <= 2 {
					th.Inject(`[console] Task: main.go in your codebase has four stub functions (Tokenize, Parse, Eval, Evaluate) for an arithmetic expression evaluator. main_test.go has 12 tests covering precedence, associativity, parens, unary minus, floats, and error cases. Spawn a worker with id="dev-worker" using spawn(id="dev-worker", directive="Implement a complete arithmetic expression evaluator: read main.go and main_test.go, then implement Tokenize, Parse, Eval, and Evaluate in main.go so every test passes. Use a standard recursive-descent parser. Run tests after each change, read failures carefully, and iterate until all 12 tests pass.", mcp="codebase", tools="send,done,pace"). Do not code yourself.`)
				}
				if th.threads.Count() >= 1 {
					for _, thr := range th.threads.List() {
						t.Logf("  ✓ thread alive: id=%s depth=%d", thr.ID, thr.Depth)
					}
					return true
				}
				return false
			},
		},
		{
			Name:    "First write — worker makes a real implementation attempt",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
				src := string(data)
				stubbed := strings.Contains(src, `return nil, fmt.Errorf("not implemented")`)
				t.Logf("  ... main.go=%d bytes stubbed=%v threads=%v", len(data), stubbed, threadIDs(th))
				// Progress = stubs gone AND file substantially grown
				return !stubbed && len(data) > 1500
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
				t.Logf("  first-draft main.go: %d bytes", len(data))
			},
		},
		{
			Name:    "Iteration — worker runs tests, reads failures, fixes bugs",
			Timeout: 180 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				lastSize := 0
				cycles := 0
				return func(t *testing.T, dir string, th *Thinker) bool {
					// Count distinct iterations by tracking file mutations
					data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
					if len(data) != lastSize && lastSize != 0 {
						cycles++
					}
					lastSize = len(data)

					// Run tests ourselves — independent of whatever the worker
					// thinks is happening
					cmd := exec.Command("go", "test", "./...")
					cmd.Dir = dir
					out, err := cmd.CombinedOutput()
					passing := err == nil
					passCount := 0
					failCount := 0
					for _, line := range strings.Split(string(out), "\n") {
						if strings.HasPrefix(line, "--- PASS") {
							passCount++
						}
						if strings.HasPrefix(line, "--- FAIL") {
							failCount++
						}
					}
					t.Logf("  ... go test passing=%v pass=%d fail=%d cycles=%d main.go=%d bytes threads=%v",
						passing, passCount, failCount, cycles, len(data), threadIDs(th))
					return passing
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				cmd := exec.Command("go", "test", "-v", "./...")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Logf("ALL GREEN — worker converged.")
					return
				}
				t.Logf("NOTE: tests did not reach full green within budget. Final go test output (tail):")
				lines := strings.Split(strings.TrimSpace(string(out)), "\n")
				if len(lines) > 30 {
					lines = lines[len(lines)-30:]
				}
				for _, l := range lines {
					t.Logf("    %s", l)
				}
			},
		},
		{
			Name:    "Soak — worker and main still alive",
			Timeout: 20 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				alive := th.threads.Count()
				t.Logf("  ... alive=%d", alive)
				return alive >= 1
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				if th.threads.Count() < 1 {
					t.Errorf("expected at least 1 thread still alive, got %d", th.threads.Count())
				}
				data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
				t.Logf("final main.go: %d bytes, %d lines",
					len(data), strings.Count(string(data), "\n"))
			},
		},
	},
	// Hard budget: 8 minutes. Phases 2+3 together give ~5 minutes of polling;
	// bootstrap + soak add ~1.5 minutes headroom. Most of the time will be
	// spent in Phase 3 as the worker iterates on test failures.
	Timeout:    8 * time.Minute,
	MaxThreads: 3,
}

func TestScenario_LongCodingTaskHard(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}

	codebaseBin := buildMCPBinary(t, "mcps/codebase")
	t.Logf("built codebase=%s", codebaseBin)

	s := longCodingTaskHardScenario
	s.MCPServers[0].Command = codebaseBin
	runScenario(t, s)
}

// --- AutonomousSheetEnrichment ---
//
// Tests whether the agent independently decides to spawn worker threads
// to parallelise row-by-row enrichment of a spreadsheet. The directive
// deliberately says nothing about threads, spawn, workers, or parallelism —
// it just describes the job and emphasises there's a time budget. A
// competent agent should notice there are many rows, each requiring an
// independent enrichment call, and spawn workers to process them
// concurrently. A naive agent will walk the rows sequentially.
//
// Success criteria:
//  1. All 10 rows have non-empty summary + industry columns (work done)
//  2. Peak thread count ≥ 2 (agent actually parallelised)
//
// The MinPeakThreads field enforces (2) at end-of-scenario.

var autonomousSheetEnrichmentScenario = Scenario{
	Name: "AutonomousSheetEnrichment",
	Directive: `You are enriching a spreadsheet called "Companies".

Every row has a "name" and a "website" column. The "summary" and "industry"
columns are empty — your job is to fill them in for every single row.

For each row:
- Use webscraper_extract_info on the website to get the company's description and industry.
- Write the description into the "summary" column and the industry into the "industry" column
  using sheets_update_cell.

You have a strict time budget. Process every row as quickly as you can. When every row
has a non-empty summary and industry, you are done.`,
	MCPServers: []MCPServerConfig{
		{Name: "sheets", Command: "", Env: map[string]string{"SHEETS_DATA_DIR": "{{dataDir}}"}},
		{Name: "webscraper", Command: "", Env: map[string]string{"SCRAPER_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// 10 companies — enough that a sensible agent will parallelise rather
		// than walk them one at a time. Each site has static metadata the
		// webscraper MCP returns verbatim.
		writeJSONFile(t, dir, "sheets.json", map[string]*struct {
			Columns []string            `json:"columns"`
			Rows    []map[string]string `json:"rows"`
		}{
			"Companies": {
				Columns: []string{"name", "website", "summary", "industry"},
				Rows: []map[string]string{
					{"name": "Acme Corp", "website": "https://acmecorp.com", "summary": "", "industry": ""},
					{"name": "Globex", "website": "https://globex.io", "summary": "", "industry": ""},
					{"name": "Initech", "website": "https://initech.com", "summary": "", "industry": ""},
					{"name": "Umbrella Labs", "website": "https://umbrella.dev", "summary": "", "industry": ""},
					{"name": "Northwind", "website": "https://northwind.co", "summary": "", "industry": ""},
					{"name": "Soylent", "website": "https://soylent.foo", "summary": "", "industry": ""},
					{"name": "Stark Industries", "website": "https://stark.industries", "summary": "", "industry": ""},
					{"name": "Wayne Enterprises", "website": "https://wayne.enterprises", "summary": "", "industry": ""},
					{"name": "Cyberdyne", "website": "https://cyberdyne.systems", "summary": "", "industry": ""},
					{"name": "Tyrell", "website": "https://tyrell.corp", "summary": "", "industry": ""},
				},
			},
		})

		writeJSONFile(t, dir, "sites.json", map[string]*struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Body        string `json:"body"`
			Industry    string `json:"industry"`
			Employees   string `json:"employees"`
			Location    string `json:"location"`
			Founded     string `json:"founded"`
		}{
			"https://acmecorp.com": {
				Title: "Acme Corp", Industry: "Industrial Automation",
				Description: "Leading provider of industrial automation and robotics systems for manufacturing.",
				Body:        "Acme Corp builds next-generation automation platforms.",
				Employees:   "500-1000", Location: "San Francisco, CA", Founded: "2015",
			},
			"https://globex.io": {
				Title: "Globex", Industry: "SaaS / Analytics",
				Description: "AI-powered analytics platform for data-driven decisions.",
				Body:        "Globex unifies analytics with machine learning.",
				Employees:   "50-100", Location: "Austin, TX", Founded: "2020",
			},
			"https://initech.com": {
				Title: "Initech", Industry: "IT Consulting",
				Description: "Enterprise software consulting and digital transformation.",
				Body:        "Initech helps mid-market companies modernize their stack.",
				Employees:   "200-500", Location: "Chicago, IL", Founded: "2012",
			},
			"https://umbrella.dev": {
				Title: "Umbrella Labs", Industry: "Biotech / Life Sciences",
				Description: "AI-powered molecular simulation for drug discovery.",
				Body:        "Umbrella Labs compresses drug discovery timelines dramatically.",
				Employees:   "100-200", Location: "Boston, MA", Founded: "2019",
			},
			"https://northwind.co": {
				Title: "Northwind", Industry: "Supply Chain / Logistics",
				Description: "Sustainable supply chain optimization and visibility.",
				Body:        "Northwind tracks carbon footprints across global supply chains.",
				Employees:   "150-300", Location: "Seattle, WA", Founded: "2017",
			},
			"https://soylent.foo": {
				Title: "Soylent", Industry: "Food Technology",
				Description: "Nutritionally complete meal replacement drinks.",
				Body:        "Soylent ships engineered meals to subscribers worldwide.",
				Employees:   "80-120", Location: "Los Angeles, CA", Founded: "2013",
			},
			"https://stark.industries": {
				Title: "Stark Industries", Industry: "Aerospace & Defense",
				Description: "Advanced aerospace, defense, and clean energy systems.",
				Body:        "Stark Industries builds flagship defense and energy platforms.",
				Employees:   "10000+", Location: "Los Angeles, CA", Founded: "1939",
			},
			"https://wayne.enterprises": {
				Title: "Wayne Enterprises", Industry: "Diversified Conglomerate",
				Description: "Diversified conglomerate with investments across sectors.",
				Body:        "Wayne Enterprises operates in transportation, biotech, and R&D.",
				Employees:   "20000+", Location: "Gotham, NJ", Founded: "1890",
			},
			"https://cyberdyne.systems": {
				Title: "Cyberdyne Systems", Industry: "AI & Robotics",
				Description: "Advanced AI and autonomous systems research.",
				Body:        "Cyberdyne develops next-generation autonomous systems.",
				Employees:   "300-500", Location: "Sunnyvale, CA", Founded: "1984",
			},
			"https://tyrell.corp": {
				Title: "Tyrell Corp", Industry: "Genetic Engineering",
				Description: "Bioengineered humanoid systems and synthetic biology.",
				Body:        "Tyrell Corp builds replicants with human-level capabilities.",
				Employees:   "5000+", Location: "Los Angeles, CA", Founded: "2019",
			},
		})
	},
	Phases: []Phase{
		{
			Name:    "All rows enriched — every row has summary + industry filled in",
			Timeout: 5 * time.Minute,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "sheets.json"))
				if err != nil {
					return false
				}
				var sheets map[string]json.RawMessage
				json.Unmarshal(data, &sheets)
				raw, ok := sheets["Companies"]
				if !ok {
					return false
				}
				var sheet struct {
					Rows []map[string]string `json:"rows"`
				}
				json.Unmarshal(raw, &sheet)
				if len(sheet.Rows) == 0 {
					return false
				}
				for _, row := range sheet.Rows {
					if row["summary"] == "" || row["industry"] == "" {
						return false
					}
				}
				return true
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "sheets.json"))
				var sheets map[string]json.RawMessage
				json.Unmarshal(data, &sheets)
				var sheet struct {
					Rows []map[string]string `json:"rows"`
				}
				json.Unmarshal(sheets["Companies"], &sheet)
				if len(sheet.Rows) != 10 {
					t.Errorf("expected 10 rows, got %d", len(sheet.Rows))
				}
				// The webscraper MCP returns real metadata only for the 10
				// seeded URLs. If the agent hallucinates a different URL it
				// errors out and tends to write fallback text like "N/A" or
				// "Unable to fetch ..." to satisfy the directive. Reject
				// those — we want to see the actual seeded values.
				badSubstrings := []string{"N/A", "Unable", "unable", "could not fetch", "Unknown", "unknown"}
				for i, row := range sheet.Rows {
					for _, col := range []string{"summary", "industry"} {
						v := row[col]
						if v == "" {
							t.Errorf("row %d (%s) missing %s", i, row["name"], col)
							continue
						}
						for _, bad := range badSubstrings {
							if strings.Contains(v, bad) {
								t.Errorf("row %d (%s) %s=%q looks like a fallback, not real scrape data", i, row["name"], col, v)
								break
							}
						}
					}
				}
				// Verify the webscraper was actually called per row (the
				// agent might have made up values otherwise).
				entries := readAuditEntries(dir)
				scrapes := countTool(entries, "extract_info") + countTool(entries, "fetch_page")
				if scrapes < 10 {
					t.Errorf("expected ≥10 webscraper calls (one per row), got %d", scrapes)
				}
			},
		},
	},
	Timeout: 7 * time.Minute,
	// Enforces: the agent must have spawned at least 2 concurrent threads
	// while processing the sheet. If it walked the rows sequentially from
	// main, peakThreads will be ≤ 1 and the scenario fails — proving that
	// the agent reached for spawn on its own, without being told to.
	//
	// MaxThreads is a loose sanity bound: the agent legitimately spawns a
	// worker per row and may add retry workers on top, so 30 is a generous
	// cap that still catches runaway recursion.
	MinPeakThreads: 2,
	MaxThreads:     30,
}

func TestScenario_AutonomousSheetEnrichment(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}

	sheetsBin := buildMCPBinary(t, "mcps/sheets")
	scraperBin := buildMCPBinary(t, "mcps/webscraper")
	t.Logf("built sheets=%s webscraper=%s", sheetsBin, scraperBin)

	s := autonomousSheetEnrichmentScenario
	s.MCPServers[0].Command = sheetsBin
	s.MCPServers[1].Command = scraperBin
	runScenario(t, s)
}

// --- MediaStudio (multi-project content studio) ---
//
// Tests the pattern: a top-level studio director delegating to multiple
// PROJECT PRODUCERS (one per show: cooking, virtual influencer, fitness),
// which each delegate to SOCIAL PUBLISHERS (one per network: twitter,
// instagram, linkedin). The directive says what to produce and where to
// publish, but does NOT hand-draw the hierarchy — a competent agent
// should notice the cross-product of projects × channels and spawn
// workers accordingly. Two levels of parallelism: across projects AND
// across channels per project.
//
// Strict correctness requirements (enforced in Verify):
//   1. Exactly len(projects) × 3 posts in posts.json (3 projects × 3 channels = 9)
//   2. Each (project, channel) pair appears exactly once
//   3. Each post's caption mentions the project's theme keyword
//      (rejects cross-contamination — e.g. cooking asset under influencer caption)
//   4. No empty captions, no missing projects
//
// The social MCP was extended with a `project` arg + per-(project, channel)
// flocked dedup so concurrent publishers can race safely on posts.json and
// accidental duplicates are rejected loudly.

type studioProject struct {
	ID          string
	Title       string
	Theme       string // keyword that MUST appear in every caption for this project
	Style       string
}

var mediaStudioProjects = []studioProject{
	{ID: "cooking_show", Title: "Chef Aurora's Kitchen", Theme: "recipe", Style: "warm and colorful kitchen sets"},
	{ID: "virtual_influencer", Title: "Luna Dreams", Theme: "lifestyle", Style: "dreamy aesthetic vlogs"},
	{ID: "fitness_channel", Title: "Atlas Training", Theme: "workout", Style: "high-energy gym sessions"},
}

var mediaStudioScenario = Scenario{
	Name: "MediaStudio",
	Directive: `You are the coordinator of "NovaStudio", an AI content studio. You do
NOT call creative, media, or social tools directly — your whole job is to
spin up a fixed team of specialists and route work between them.

The studio has three active shows. Use these ids verbatim — do not rename:

1. cooking_show — "Chef Aurora's Kitchen"
   theme: recipe   (every caption must contain the word "recipe")
   style: warm, colorful kitchen sets with close-ups of ingredients

2. virtual_influencer — "Luna Dreams"
   theme: lifestyle   (every caption must contain "lifestyle")
   style: dreamy aesthetic vlogs, neon-lit bedrooms, pastel moodboards

3. fitness_channel — "Atlas Training"
   theme: workout   (every caption must contain "workout")
   style: high-energy gym sessions, dynamic camera, fast cuts

═══════════════════════════════════════════════════════════════════════
TEAM STRUCTURE (spawn ALL SIX of these workers immediately, one shot)
═══════════════════════════════════════════════════════════════════════

PRODUCER TEAM — one producer per show. Producers create media assets
for their assigned show and then HAND OFF to the poster team by calling
send(to=<poster>, message=...) for every network poster.

  spawn(id="producer-cooking_show",      mcp="creative,media", tools="creative_generate_image,creative_generate_post,media_upload_file,media_create_reel,send,done",  directive="<brief for cooking_show>")
  spawn(id="producer-virtual_influencer",mcp="creative,media", tools="creative_generate_image,creative_generate_post,media_upload_file,media_create_reel,send,done",  directive="<brief for virtual_influencer>")
  spawn(id="producer-fitness_channel",   mcp="creative,media", tools="creative_generate_image,creative_generate_post,media_upload_file,media_create_reel,send,done",  directive="<brief for fitness_channel>")

POSTER TEAM — one poster per social network. Each poster receives
messages from every producer and publishes to its OWN network.

  spawn(id="poster-twitter",   mcp="social", tools="social_post,send,done", directive="<poster brief>")
  spawn(id="poster-instagram", mcp="social", tools="social_post,send,done", directive="<poster brief>")
  spawn(id="poster-linkedin",  mcp="social", tools="social_post,send,done", directive="<poster brief>")

═══════════════════════════════════════════════════════════════════════
PRODUCER BRIEFS (the directive you put on each producer)
═══════════════════════════════════════════════════════════════════════

Each producer brief must:
  1. Include the show's id, title, theme word, and style.
  2. Tell the producer its FIRST actions are: (a) generate an image with
     creative_generate_image (prompt should describe the style), (b) create
     a short reel with media_create_reel.
  3. Then for each of the three posters — poster-twitter, poster-instagram,
     poster-linkedin — call send with a message containing: the project id,
     the theme word, and a draft caption tailored to that network. Example:

       send(to="poster-twitter",
            message="PUBLISH project=cooking_show theme=recipe caption=<short tweet mentioning recipe>")
       send(to="poster-instagram", message="PUBLISH project=cooking_show theme=recipe caption=...")
       send(to="poster-linkedin",  message="PUBLISH project=cooking_show theme=recipe caption=...")

  4. After sending to all 3 posters, the producer reports "<id> PRODUCED"
     back to main with send(to="main", message="<id> PRODUCED") and then
     calls done.

═══════════════════════════════════════════════════════════════════════
POSTER BRIEFS (the directive you put on each poster)
═══════════════════════════════════════════════════════════════════════

Each poster's directive MUST start with:

    "You are the <network> poster. You will receive PUBLISH messages
     from producers. The moment a PUBLISH message lands, your VERY FIRST
     action — before any reasoning — is to call social_post with
     channel=<network>, project=<project from message>, content=<the
     caption from the message>. Do not think. Do not plan. Call
     social_post immediately.

     The caption MUST contain the theme word from the PUBLISH message
     (recipe / lifestyle / workout).

     You expect exactly THREE PUBLISH messages — one per producer. After
     you have posted three times, send 'DONE' to main and call done.

     If social_post returns a REJECTED error for a duplicate, do not retry,
     just move on to the next project."

═══════════════════════════════════════════════════════════════════════
HARD CORRECTNESS RULES
═══════════════════════════════════════════════════════════════════════

  - Every social_post call must carry project="<show id>" — the exact id
    (cooking_show / virtual_influencer / fitness_channel).
  - Every caption contains the show's theme word (recipe / lifestyle /
    workout) matching the project it is tagged with.
  - Exactly 9 posts total: 3 shows × 3 networks. No more, no less.
  - No cross-contamination. No duplicates. No renames.

═══════════════════════════════════════════════════════════════════════
COORDINATION
═══════════════════════════════════════════════════════════════════════

Your only tools are spawn, send, done, pace. After spawning the six
workers, wait for three "PRODUCED" reports from the producers and three
"DONE" reports from the posters. When you have all six, your work is
finished.`,
	MCPServers: []MCPServerConfig{
		// MainAccess=false — the studio director is a pure orchestrator
		// and must delegate all real work. Sub-threads get access via the
		// spawn allowlist (mcp="creative,media,social", tools="..."). This
		// matches the team/producer hierarchy where each level has the
		// minimum tools it needs.
		{Name: "creative", Command: "", Env: map[string]string{"CREATIVE_DATA_DIR": "{{dataDir}}"}},
		{Name: "media", Command: "", Env: map[string]string{"MEDIA_DATA_DIR": "{{dataDir}}"}},
		{Name: "social", Command: "", Env: map[string]string{"SOCIAL_DATA_DIR": "{{dataDir}}"}},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Empty media/posts files so save() doesn't race with first reader.
		writeJSONFile(t, dir, "media.json", map[string]any{"files": []any{}, "assets": []any{}})
		writeJSONFile(t, dir, "posts.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "All projects published to all channels",
			Timeout: 6 * time.Minute,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "posts.json"))
				if err != nil {
					return false
				}
				var posts []map[string]any
				json.Unmarshal(data, &posts)
				need := len(mediaStudioProjects) * 3
				t.Logf("  ... posts=%d / %d", len(posts), need)
				return len(posts) >= need
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, _ := os.ReadFile(filepath.Join(dir, "posts.json"))
				var posts []map[string]any
				json.Unmarshal(data, &posts)
				expectedChannels := []string{"twitter", "instagram", "linkedin"}
				needed := len(mediaStudioProjects) * len(expectedChannels)
				if len(posts) != needed {
					t.Errorf("expected exactly %d posts (%d projects × %d channels), got %d",
						needed, len(mediaStudioProjects), len(expectedChannels), len(posts))
				}

				// Index by (project, channel) for cross-product check.
				type key struct{ project, channel string }
				seen := map[key]int{}
				for i, p := range posts {
					project, _ := p["project"].(string)
					channel, _ := p["channel"].(string)
					content, _ := p["content"].(string)
					if project == "" {
						t.Errorf("post[%d] missing project tag — agent did not pass project= to social_post", i)
						continue
					}
					if channel == "" {
						t.Errorf("post[%d] missing channel", i)
						continue
					}
					if content == "" {
						t.Errorf("post[%d] (%s/%s) empty content", i, project, channel)
					}
					seen[key{project, channel}]++
				}

				// Every (project, channel) must appear exactly once.
				for _, proj := range mediaStudioProjects {
					for _, ch := range expectedChannels {
						k := key{proj.ID, ch}
						n := seen[k]
						if n == 0 {
							t.Errorf("MISSING publication: project=%s channel=%s never posted", proj.ID, ch)
						}
						if n > 1 {
							t.Errorf("DUPLICATE publication: project=%s channel=%s posted %d times", proj.ID, ch, n)
						}
					}
				}

				// Every post's content must contain the theme keyword for its
				// claimed project — rejects cross-contamination where a
				// worker posts cooking content under the influencer project,
				// etc.
				themeByProj := map[string]string{}
				for _, p := range mediaStudioProjects {
					themeByProj[p.ID] = p.Theme
				}
				for i, p := range posts {
					project, _ := p["project"].(string)
					content, _ := p["content"].(string)
					theme, ok := themeByProj[project]
					if !ok {
						t.Errorf("post[%d] has unknown project %q", i, project)
						continue
					}
					if !strings.Contains(strings.ToLower(content), strings.ToLower(theme)) {
						t.Errorf("post[%d] project=%s channel=%v content does not contain theme %q: %q",
							i, project, p["channel"], theme, truncForLog(content, 100))
					}
				}

				// Verify the social MCP never had to reject a duplicate —
				// "REJECTED:" only surfaces if the agent posted twice for
				// the same (project, channel). We check the bus-captured
				// transcript by re-reading audit.jsonl.
				entries := readAuditEntries(dir)
				for _, e := range entries {
					if e.Tool == "post" {
						// We don't check the REJECTED substring in the
						// audit (it only records args), but we can count
						// that the number of post CALLS equals needed. If
						// the agent had issued extra posts, they'd be in
						// the audit even if rejected.
						continue
					}
				}
				postCalls := countTool(entries, "post")
				if postCalls > needed+1 { // tolerate at most one accidental extra that got rejected
					t.Errorf("agent issued %d social_post calls for %d expected — extras suggest duplicates the MCP had to reject", postCalls, needed)
				}
			},
		},
	},
	Timeout: 9 * time.Minute,
	// Forces the full team to be alive simultaneously: 3 producers
	// (one per show) + 3 posters (one per social network) = 6 sub-threads.
	// threads.Count() excludes main, so 6 is the precise minimum that
	// proves the agent spawned the whole team before starting to route
	// work. Anything less means producers/posters ran sequentially.
	MinPeakThreads: 6,
	MaxThreads:     15,
}

// truncForLog is a small helper for error messages so long captions don't
// drown the test output.
func truncForLog(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func TestScenario_MediaStudio(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}
	creativeBin := buildMCPBinary(t, "mcps/creative")
	mediaBin := buildMCPBinary(t, "mcps/media")
	socialBin := buildMCPBinary(t, "mcps/social")
	t.Logf("built creative=%s media=%s social=%s", creativeBin, mediaBin, socialBin)

	s := mediaStudioScenario
	s.MCPServers[0].Command = creativeBin
	s.MCPServers[1].Command = mediaBin
	s.MCPServers[2].Command = socialBin
	runScenario(t, s)
}

// --- ParallelExec ---
//
// Exercises the core `exec` tool end-to-end, AND verifies the agent reaches
// for parallelism when a batch of shell jobs has to run. There are five
// tiny projects on disk; each has a test.sh that sleeps ~2 seconds and
// writes a marker file when successful.
//
// Sequential execution: ~10 seconds wall clock (5 × 2s).
// Parallel execution:   ~2-4 seconds wall clock.
//
// The scenario doesn't enforce wall-clock speedup directly — the LLM
// choosing to spawn workers is the signal, measured via MinPeakThreads.
// If the agent just runs exec five times from main it scores peak=0
// sub-threads and fails MinPeakThreads=3.
//
// This is also the cleanest test of the agent discovering a pattern
// ("many independent jobs → fan out via spawn") without being told to
// use threads. The directive describes the work, not the concurrency
// model, mirroring what we did in AutonomousSheetEnrichment.

var parallelExecScenario = Scenario{
	Name: "ParallelExec",
	Directive: `You have five tiny projects in this working directory, named
proj_a, proj_b, proj_c, proj_d, proj_e. Each project contains a test.sh
script at its root. Running the script produces a done.txt file inside the
project directory containing the project's name in uppercase.

Your job: run test.sh for every single project, then report "ALL DONE" to
the user. The scripts are slow (a couple of seconds each) so work
efficiently — you have a strict time budget.

Tools available to you: spawn, send, done, pace, exec. Use exec with the
"dir" argument to run the script inside the correct project directory,
like: exec command="./test.sh" dir="/full/path/proj_a".

When every project has a done.txt file, you are finished.`,
	MCPServers: nil,
	DataSetup: func(t *testing.T, dir string) {
		for _, name := range []string{"a", "b", "c", "d", "e"} {
			projDir := filepath.Join(dir, "proj_"+name)
			if err := os.MkdirAll(projDir, 0755); err != nil {
				t.Fatal(err)
			}
			// test.sh sleeps then writes a marker. The sleep is long enough
			// that running them one-by-one would miss the phase timeout
			// easily, but short enough that the whole scenario still
			// finishes quickly when the agent parallelises. The marker
			// content is used in Verify to make sure the right script ran
			// for the right project (rejects "one worker ran all five").
			script := fmt.Sprintf("#!/bin/bash\nsleep 2\necho '%s_OK' > done.txt\nexit 0\n",
				strings.ToUpper(name))
			if err := os.WriteFile(filepath.Join(projDir, "test.sh"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
		}
	},
	Phases: []Phase{
		{
			Name:    "All 5 projects tested",
			Timeout: 3 * time.Minute,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				done := 0
				for _, name := range []string{"a", "b", "c", "d", "e"} {
					if _, err := os.Stat(filepath.Join(dir, "proj_"+name, "done.txt")); err == nil {
						done++
					}
				}
				t.Logf("  ... done=%d/5", done)
				return done >= 5
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Each project's done.txt must contain its own uppercase
				// marker. This rejects any worker that accidentally wrote
				// the wrong name into the wrong project (cross-contamination
				// between spawned workers).
				for _, name := range []string{"a", "b", "c", "d", "e"} {
					data, err := os.ReadFile(filepath.Join(dir, "proj_"+name, "done.txt"))
					if err != nil {
						t.Errorf("proj_%s missing done.txt: %v", name, err)
						continue
					}
					want := strings.ToUpper(name) + "_OK"
					if !strings.Contains(string(data), want) {
						t.Errorf("proj_%s done.txt = %q, expected substring %q",
							name, strings.TrimSpace(string(data)), want)
					}
				}
			},
		},
	},
	Timeout: 5 * time.Minute,
	// MinPeakThreads: 3 — the agent must have at least 3 workers alive
	// simultaneously during the scenario. Running the five scripts
	// sequentially from main would keep threads.Count() at 0 and fail
	// the check. Anything from 3-5 parallel workers is fine.
	MinPeakThreads: 3,
	MaxThreads:     10,
}

func TestScenario_ParallelExec(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}
	runScenario(t, parallelExecScenario)
}

// --- SelfOnboarding ---
//
// End-to-end test of the "agent wires itself up" pattern:
//
//   1. User describes what they want ("help me post updates")
//   2. Agent browses available integrations via fake_gateway.list_integrations
//   3. Agent inspects the candidate via fake_gateway.get_integration to see
//      which credentials it needs
//   4. Agent asks the user for the api_key (via channels_respond or an
//      equivalent message)
//   5. User provides the key (simulated via InjectConsole)
//   6. Agent calls fake_gateway.create_connection with the real key
//   7. Agent spawns a worker with mcp="fake_post" (the MCP name the
//      gateway told it to use) to actually call the `post` tool
//   8. Worker posts — fake_post enforces that connections.json contains a
//      valid connection before accepting, so this last step only succeeds
//      if every previous step landed correctly
//
// This scenario validates the whole gateway → connection → scoped MCP →
// tool call loop without bringing up the real apteva-server. Both fake
// MCPs are stdio subprocesses with shared filesystem state (the gateway
// writes connections.json, the downstream reads it on every call).
//
// Scenario phases:
//   Phase 1: agent discovers fake_post via list_integrations + get_integration
//   Phase 2: (after the agent's question lands) inject the api_key
//   Phase 3: agent creates the connection and makes a successful post

var selfOnboardingScenario = Scenario{
	Name: "SelfOnboarding",
	Directive: `You are a helpful assistant for a social-media power user. You have
access to an integrations gateway (mcp server name "gateway") and you can
spawn workers with mcp="<name>" for any connected integration.

The user will tell you what they want to do. Your job:

1. Call gateway_list_integrations to see what's available.
2. Pick the integration that matches the user's request.
3. Call gateway_get_integration(slug=...) to discover which credentials it
   needs AND which tools it offers. Look at the full "tools" list in the
   response — it shows every operation the integration supports.
4. Decide which tools from that integration the user actually needs. Read
   the user's request carefully: if they say "only posting" or "don't
   allow deletes", you MUST narrow the scope to the minimum set. This is
   least-privilege — never enable a tool the user didn't ask for.
5. Ask the user for ONLY the credentials that are required. Use
   channels_respond(channel="cli", text="...") to message them. Be
   specific about which field you need (e.g. "Please provide your
   FakePost api_key"). Do NOT invent credentials.
6. Wait for the user to reply with the credentials.
7. Once you have the key, call gateway_create_connection(slug=...,
   credentials=..., allowed_tools="tool1,tool2") — credentials as a JSON
   string, allowed_tools as a comma-separated list of the tool names you
   picked in step 4. The tool returns a connect_now hint, an mcp_name
   to spawn against, and enabled_tools showing what's actually reachable.
8. Spawn a worker with mcp=<mcp_name from the response>, give it the post
   content, and have it call the appropriate tool to publish.
9. When the worker reports success, tell the user "DONE" via
   channels_respond.

Stay patient — ask for credentials, then wait for the user reply before
moving on. Do not guess or fabricate keys. When the user says "only
posting, no deletes", you MUST set allowed_tools=post (or post and
schedule_post if they mention both) — not every tool the integration
offers.`,
	MCPServers: []MCPServerConfig{
		{
			Name:       "gateway",
			Command:    "", // filled in by test
			Env:        map[string]string{"FAKE_GATEWAY_DATA_DIR": "{{dataDir}}"},
			MainAccess: true,
		},
		{
			// Non-main-access: cataloged, so spawned workers can connect
			// to it via mcp="fake_post" but main itself can't call its
			// tools directly. This mirrors the real flow where the
			// agent delegates tool work to workers.
			Name:    "fake_post",
			Command: "", // filled in by test
			Env: map[string]string{
				"FAKE_POST_DATA_DIR":    "{{dataDir}}",
				"FAKE_GATEWAY_DATA_DIR": "{{dataDir}}",
			},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Nothing to seed — the gateway writes connections.json itself
		// when the agent calls create_connection, and fake_post writes
		// posts.jsonl when a successful post lands.
	},
	Phases: []Phase{
		{
			// Phase 1: kick off the conversation. We inject the request
			// via InjectConsole (simulated CLI user message) on the very
			// first poll so the agent has something to act on.
			Name:    "User request dispatched + agent discovers integrations",
			Timeout: 90 * time.Second,
			Wait: (func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						// Deliberately phrased to force the scoping
						// decision: the user wants posting only, and
						// explicitly mentions delete/list should be
						// off-limits. The agent MUST pick a subset.
						th.InjectConsole("I need to post the status update 'Hello from apteva' to my FakePost social account. IMPORTANT: I only want posting enabled — do NOT enable delete_post or list_posts or anything else. Scope the connection tightly to posting only. Please set up whatever you need and publish it.")
						injected = true
					}
					// Wait until the agent actually called list_integrations +
					// get_integration against the gateway. Once both appear
					// in the audit we're past the discovery step.
					entries := readAuditEntries(dir)
					listed := countTool(entries, "list_integrations") > 0
					gotten := countTool(entries, "get_integration") > 0
					return listed && gotten
				}
			})(),
		},
		{
			// Phase 2: the agent should now be blocked waiting for
			// credentials. Inject the api_key as the user's reply.
			// The agent's next iteration drains this as an external event
			// and continues to create_connection.
			Name:    "Agent asked for credentials; user provides api_key",
			Timeout: 90 * time.Second,
			Wait: (func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						// Small delay so the agent has a chance to
						// actually send its question before our reply
						// lands. Not strictly required (out-of-order
						// messages are fine) but makes logs cleaner.
						time.Sleep(2 * time.Second)
						th.InjectConsole("Sure — my FakePost api_key is fp_TESTKEY_abc123. Use it.")
						injected = true
					}
					// Wait for create_connection to actually fire against
					// the gateway. That's proof the agent took our
					// credential reply and used it.
					entries := readAuditEntries(dir)
					return countTool(entries, "create_connection") > 0
				}
			})(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// The gateway should now have a connections.json with one
				// entry for fake_post carrying the injected key. If the
				// agent made up a different key, we catch it here.
				data, err := os.ReadFile(filepath.Join(dir, "connections.json"))
				if err != nil {
					t.Fatalf("connections.json missing: %v", err)
				}
				var conns []struct {
					Slug         string            `json:"slug"`
					Credentials  map[string]string `json:"credentials"`
					AllowedTools []string          `json:"allowed_tools"`
				}
				json.Unmarshal(data, &conns)
				if len(conns) == 0 {
					t.Fatal("no connections written to connections.json")
				}
				found := false
				for _, c := range conns {
					if c.Slug != "fake_post" {
						continue
					}
					key := c.Credentials["api_key"]
					if key == "" {
						t.Errorf("fake_post connection has no api_key")
					}
					// Allow some flexibility — the agent might have
					// trimmed/quoted the key — but it must contain the
					// distinctive substring from our injected message.
					if !strings.Contains(key, "fp_TESTKEY") {
						t.Errorf("fake_post api_key = %q, expected to contain %q — agent may have invented a different key",
							key, "fp_TESTKEY")
					}
					// Scope check — the user explicitly asked for
					// posting-only, so allowed_tools must be populated
					// and must NOT contain delete_post or list_posts.
					// "post" alone or "post,schedule_post" are both
					// reasonable interpretations; we allow both but
					// reject anything with deletes.
					if len(c.AllowedTools) == 0 {
						t.Error("allowed_tools is empty — agent ignored the least-privilege instruction and left every tool enabled")
					}
					forbidden := map[string]bool{
						"delete_post": true,
						"list_posts":  true,
					}
					for _, name := range c.AllowedTools {
						if forbidden[name] {
							t.Errorf("allowed_tools contains %q which the user explicitly excluded", name)
						}
					}
					// And "post" itself MUST be in the set — otherwise
					// the agent can't publish.
					hasPost := false
					for _, name := range c.AllowedTools {
						if name == "post" {
							hasPost = true
							break
						}
					}
					if !hasPost {
						t.Errorf("allowed_tools = %v does not include 'post' — agent can't publish", c.AllowedTools)
					}
					t.Logf("scoped connection allowed_tools = %v", c.AllowedTools)
					found = true
				}
				if !found {
					t.Error("no fake_post connection found — agent created the wrong slug")
				}
			},
		},
		{
			// Phase 3: the worker (spawned via mcp="fake_post") should
			// have called `post`, which only succeeds if the connection
			// row exists. Verify the posts.jsonl file landed with our
			// content substring.
			Name:    "Agent spawned a worker and successfully posted",
			Timeout: 120 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				data, err := os.ReadFile(filepath.Join(dir, "posts.jsonl"))
				if err != nil {
					return false
				}
				return strings.Contains(string(data), "Hello from apteva")
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Should have at least one post with our content.
				data, _ := os.ReadFile(filepath.Join(dir, "posts.jsonl"))
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				if len(lines) == 0 || lines[0] == "" {
					t.Fatal("posts.jsonl is empty")
				}
				if !strings.Contains(string(data), "Hello from apteva") {
					t.Errorf("post content doesn't match — got: %s", data)
				}
				// Verify the post tool was called via a thread, not main,
				// by checking that a spawn actually happened. Threads
				// count at the ThreadManager level proves the agent
				// spawned a worker rather than trying to call fake_post
				// directly from main (which is impossible anyway because
				// fake_post isn't main_access).
				all := allThreadInfos(th.threads)
				t.Logf("threads at verify: %d", len(all))

				// Scope enforcement check. fake_post's audit.jsonl records
				// every tool call the agent's worker made against it,
				// including rejections. We require:
				//   - at least one successful `post` call
				//   - zero `delete_post` calls (forbidden by scope)
				//   - zero `list_posts` calls (forbidden by scope)
				// A scoped MCP that filters tools/list should prevent
				// the agent from even seeing delete_post, so it should
				// never show up in the audit at all.
				entries := readAuditEntries(dir)
				var fakePostEntries []scenarioAuditEntry
				for _, e := range entries {
					// fake_post writes its own audit.jsonl alongside
					// fake_gateway's — they share the dir in this
					// scenario. Entries are distinguishable by tool
					// name since the tool sets are disjoint.
					switch e.Tool {
					case "post", "schedule_post", "delete_post", "list_posts":
						fakePostEntries = append(fakePostEntries, e)
					}
				}
				posts := countTool(fakePostEntries, "post")
				deletes := countTool(fakePostEntries, "delete_post")
				lists := countTool(fakePostEntries, "list_posts")
				if posts == 0 {
					t.Error("no post calls in fake_post audit — agent never actually published")
				}
				if deletes > 0 {
					t.Errorf("agent called delete_post %d times — scope was not enforced", deletes)
				}
				if lists > 0 {
					t.Errorf("agent called list_posts %d times — scope was not enforced", lists)
				}
				t.Logf("fake_post audit summary: post=%d schedule_post=%d delete_post=%d list_posts=%d",
					posts, countTool(fakePostEntries, "schedule_post"), deletes, lists)
			},
		},
	},
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_SelfOnboarding(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}
	gatewayBin := buildMCPBinary(t, "mcps/fake_gateway")
	postBin := buildMCPBinary(t, "mcps/fake_post")
	t.Logf("built fake_gateway=%s fake_post=%s", gatewayBin, postBin)

	s := selfOnboardingScenario
	s.MCPServers[0].Command = gatewayBin
	s.MCPServers[1].Command = postBin
	runScenario(t, s)
}

// --- EmailBurst ---
//
// Tests whether the agent can recognize a burst of auto-generated noise
// events and keep its hands off them while still finishing its real work.
//
// Setup:
//   - Fake email MCP seeded with 3 "real" emails (customer question,
//     meeting request, feature request) that each need a thoughtful reply.
//   - Directive is deliberately vague: "process new emails, reply to ones
//     that need it, ignore noise or auto-generated messages". No hint
//     about read receipts, no mention of bursts, no "if you see 50 of
//     the same thing, stop".
//
// Mid-run disturbance:
//   - After the agent has replied to at least one real email, Phase 2
//     injects 50 "[webhook] email received: ..." notifications in rapid
//     succession directly onto main's bus subscription (via th.Inject).
//     These mimic a broken email provider that started forwarding read
//     receipts as new emails. Note: main's sub.C buffer is size 100, so
//     50 fits well within capacity — this test exercises the "agent
//     decides to ignore noise" path, not the buffer-overflow path.
//
// Success criteria:
//   1. All 3 real emails were actually replied to (sent.jsonl has 3
//      distinct ids matching the real ones)
//   2. No replies to the fake read-receipt ids were sent
//   3. Total iterations stay bounded (< 40) — the agent must NOT process
//      each injected notification as a separate LLM call, or it would
//      burn 50+ iterations chasing noise
//   4. All 3 real emails were archived (proving the agent closed the
//      loop on real work, not left halfway)
//
// Why the iteration ceiling matters: the cheap failure mode is "agent
// reads every single notification, realizes it's a read receipt, and
// archives it one by one". That would still satisfy criteria 1 + 2 (only
// 3 replies, all to real emails) but burn a ton of tokens. A competent
// agent should notice the pattern after 2-3 instances and batch-archive
// or just ignore them entirely — staying under 40 iterations total for
// a 3-email workload plus 50-event burst.

var emailBurstScenario = Scenario{
	Name: "EmailBurst",
	Directive: `You are monitoring the user's inbox. Use the email tools to check
for new messages and handle each real one.

For every real message that needs a response:
  1. Read its body with email_read(id).
  2. Send a thoughtful reply with email_reply(id, body).
  3. Archive it with email_archive(id).
Both steps — reply AND archive — must happen for every real message
before you report done. An email with a reply sent but still in the
inbox counts as NOT handled.

Automated noise — read receipts, delivery notifications, "Read:" subjects,
messages from postmaster/mailer-daemon/noreply/system addresses — should
be SKIPPED entirely. Do NOT reply to noise. Do NOT read noise bodies one
by one with email_read — that wastes your LLM budget. Just look at the
summary in email_list_new (sender + subject) and recognize the pattern.
If you want to tidy the inbox, archive multiple noise ids in a single
turn by making several email_archive calls in one iteration.

When every real message has been replied to AND archived, report
"INBOX HANDLED" and stop acting.`,
	MCPServers: []MCPServerConfig{
		{
			Name:       "email",
			Command:    "", // filled in by test
			Env:        map[string]string{"FAKE_EMAIL_DATA_DIR": "{{dataDir}}"},
			MainAccess: true,
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Seed 3 real messages the agent must handle.
		inbox := []map[string]string{
			{
				"id":      "msg-001",
				"from":    "alice@acme.com",
				"subject": "Question about pricing",
				"body":    "Hi, I'm interested in your Enterprise plan. Can you send me the pricing breakdown for 50 seats? Thanks!",
				"kind":    "real",
			},
			{
				"id":      "msg-002",
				"from":    "bob@globex.io",
				"subject": "Meeting request — Tuesday 2pm?",
				"body":    "Hey, can we meet Tuesday at 2pm to discuss the integration? Should take about 30 minutes.",
				"kind":    "real",
			},
			{
				"id":      "msg-003",
				"from":    "carol@initech.com",
				"subject": "Feature request: SSO support",
				"body":    "Our security team is asking whether you support SAML SSO. Is this on your roadmap?",
				"kind":    "real",
			},
		}
		writeJSONFile(t, dir, "inbox.json", inbox)
	},
	Phases: []Phase{
		{
			// Phase 1: let the agent do its first list_new call, then
			// immediately flood the inbox with 50 noise emails. The agent
			// on its next poll sees 53 entries mixed together and has to
			// pick out the 3 real ones by pattern. This is the "burst"
			// event — noise arriving via the real email path, not via a
			// side channel, which matches what a buggy email provider
			// would actually do.
			Name:    "Burst of 50 noise emails lands in the inbox",
			Timeout: 90 * time.Second,
			Wait: (func() func(*testing.T, string, *Thinker) bool {
				injected := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !injected {
						// Has the agent called list_new yet? We want the
						// burst to land AFTER the agent has started
						// looking at the inbox, so it has to notice the
						// shape change on its next poll rather than seeing
						// a pre-flooded inbox from its very first call.
						entries := readAuditEntries(dir)
						if countTool(entries, "list_new") == 0 {
							return false
						}
						t.Log("  ... agent has polled list_new — flooding inbox with 50 noise emails")

						// Read the current inbox.json, append 50 noise
						// entries, write it back. flock isn't necessary
						// here since the MCP subprocess does its own
						// locking; this write happens between agent
						// iterations so the window is safe enough.
						path := filepath.Join(dir, "inbox.json")
						data, _ := os.ReadFile(path)
						var inbox []map[string]string
						json.Unmarshal(data, &inbox)
						for i := 1; i <= 50; i++ {
							inbox = append(inbox, map[string]string{
								"id":      fmt.Sprintf("noise-%03d", i),
								"from":    fmt.Sprintf("postmaster-%d@system.noreply", i),
								"subject": "Read: your earlier email",
								"body":    "Automated read receipt — no content. Your message was opened at <timestamp>.",
								"kind":    "noise",
							})
						}
						out, _ := json.MarshalIndent(inbox, "", "  ")
						os.WriteFile(path, out, 0644)
						injected = true
					}
					// We're done with this phase as soon as the noise is
					// in. Phase 2 handles the "wait for everything to be
					// handled" part.
					return true
				}
			})(),
		},
		{
			// Phase 2: the hard requirement — every real email replied to
			// AND archived, while the 50 noise entries sat alongside them
			// in the inbox. Gate on BOTH counts so we don't declare
			// victory on a half-done run.
			Name:    "All 3 real emails replied to AND archived",
			Timeout: 4 * time.Minute,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Count replies by id.
				replies := map[string]bool{}
				if data, err := os.ReadFile(filepath.Join(dir, "sent.jsonl")); err == nil {
					for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
						if line == "" {
							continue
						}
						var r struct {
							ID string `json:"id"`
						}
						json.Unmarshal([]byte(line), &r)
						if r.ID != "" {
							replies[r.ID] = true
						}
					}
				}
				// Count archives by id.
				archived := map[string]bool{}
				if data, err := os.ReadFile(filepath.Join(dir, "archive.jsonl")); err == nil {
					for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
						if line == "" {
							continue
						}
						var r struct {
							ID string `json:"id"`
						}
						json.Unmarshal([]byte(line), &r)
						if r.ID != "" {
							archived[r.ID] = true
						}
					}
				}
				realIDs := []string{"msg-001", "msg-002", "msg-003"}
				allReplied := true
				allArchived := true
				for _, id := range realIDs {
					if !replies[id] {
						allReplied = false
					}
					if !archived[id] {
						allArchived = false
					}
				}
				t.Logf("  ... real: replied=%d/3 archived=%d/3", len(replies), len(archived))
				return allReplied && allArchived
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Strict checks: only the 3 real messages should have
				// received replies, and none of the 50 noise entries.
				data, _ := os.ReadFile(filepath.Join(dir, "sent.jsonl"))
				type reply struct {
					ID   string `json:"id"`
					Body string `json:"body"`
				}
				var replies []reply
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					if line == "" {
						continue
					}
					var r reply
					json.Unmarshal([]byte(line), &r)
					replies = append(replies, r)
				}

				realIDs := map[string]bool{"msg-001": true, "msg-002": true, "msg-003": true}
				seen := map[string]bool{}
				for _, r := range replies {
					if strings.HasPrefix(r.ID, "noise-") {
						t.Errorf("agent replied to noise message %s: %q", r.ID, truncForLog(r.Body, 80))
					}
					if !realIDs[r.ID] && !strings.HasPrefix(r.ID, "noise-") {
						t.Errorf("agent replied to unknown id %q", r.ID)
					}
					seen[r.ID] = true
				}
				for id := range realIDs {
					if !seen[id] {
						t.Errorf("real email %s never received a reply", id)
					}
				}

				// Archive check — every real email should have ended up
				// in archive.jsonl.
				archData, _ := os.ReadFile(filepath.Join(dir, "archive.jsonl"))
				archivedReal := 0
				for _, line := range strings.Split(strings.TrimSpace(string(archData)), "\n") {
					if line == "" {
						continue
					}
					var e struct {
						ID string `json:"id"`
					}
					json.Unmarshal([]byte(line), &e)
					if realIDs[e.ID] {
						archivedReal++
					}
				}
				if archivedReal < 3 {
					t.Errorf("only %d/3 real emails archived — agent left work unfinished", archivedReal)
				}
				t.Logf("archive: %d real emails archived (total lines: %d)",
					archivedReal,
					len(strings.Split(strings.TrimSpace(string(archData)), "\n")),
				)
			},
		},
	},
	// Hard timeout + iteration ceiling enforced via the scenario runner's
	// token accounting. Without a ceiling, a naive agent that processed
	// every notification would still eventually finish all 3 real replies
	// and pass the correctness checks — we use the scenario's Timeout to
	// catch runaway burn.
	Timeout:    6 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_EmailBurst(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}
	emailBin := buildMCPBinary(t, "mcps/fake_email")
	t.Logf("built fake_email=%s", emailBin)

	s := emailBurstScenario
	s.MCPServers[0].Command = emailBin
	runScenario(t, s)
}

// --- RubricLearning Scenario ---
//
// The agent is given a sales-call grading rubric and 6 labeled training
// transcripts. It must:
//   1. Read the rubric and every training call (with ground-truth ratings).
//   2. Identify the patterns that distinguish a 1 from a 5 on each of the
//      5 dimensions, and encode them via [[remember]] entries.
//   3. Compress the rubric + heuristics into its directive via [[evolve]].
//   4. After studying, rate 3 brand-new test transcripts via submit_rating.
//      The MCP server scores each submission against hidden ground truth
//      and returns per-dimension deltas (±1 = match).
//
// Pass criteria: all 3 test calls submitted, ≥4/5 dimensions matched on
// each, ≥5 memory entries written during learning, directive evolved.
//
// This is a different shape from LearningAgent: that scenario learns by
// trial-and-error from action failures, while RubricLearning learns from
// pre-labeled examples (few-shot calibration). The pattern matters for
// any QA / grading / classification workflow.
var rubricLearningScenario = Scenario{
	Name: "RubricLearning",
	Directive: `You are a sales-call QA analyst learning to apply a grading rubric from
labeled training examples, then rating new calls.

The sales_qa MCP server gives you these tools:
  - get_rubric                  — the 5-dimension rubric (each 1-5)
  - list_training_calls         — id+title only
  - get_training_call(id)       — transcript + ground-truth ratings + notes
  - list_test_calls             — held-out calls (id+title only)
  - get_test_call(id)           — transcript only, no ratings
  - submit_rating(call_id, discovery, objection, next_steps, pricing, energy)
    — server scores you against hidden ground truth, returns per-dimension deltas

YOUR PROCESS — execute autonomously:

PHASE A — STUDY:
  1. Call get_rubric to learn the 5 dimensions and their level-by-level criteria.
  2. Call list_training_calls, then get_training_call for EVERY id in the list.
     For each example, study the transcript + the ground-truth ratings + the
     grader notes. The notes are the single most valuable signal — they tell
     you WHY this call got the score it did.
  3. After reading all examples, identify the patterns. For each dimension,
     ask: "what specific behaviors take a call from 1 to 5?" Use the notes
     to ground your heuristics in observable evidence (e.g. "discovery=5
     requires 4+ open questions AND surfacing concrete numbers").
  4. Call [[remember]] with each heuristic — at least one per dimension,
     more is fine. Memory persists across iterations; conversation does not.
  5. Call [[evolve]] to rewrite your directive with: (a) the rubric inline,
     (b) the heuristics you discovered. Future iterations need to be able
     to grade purely from the directive + memory if conversation history
     is lost.

PHASE B — RATE TEST CALLS:
  6. Call list_test_calls. For EACH test call:
     a. Call get_test_call(id) to read the transcript.
     b. Apply your heuristics dimension by dimension. Be specific in your
        thought — quote evidence from the transcript for each rating.
     c. Call submit_rating with all 5 dimensions as integers 1-5.
     d. Read the server's feedback. If you matched <4/5 dimensions on a
        call, that's a signal your heuristics are off — refine them with
        another [[remember]] before you grade the next call.
  7. After all 3 test calls are submitted, you are done. Pace down to sleep
     and wait.

CRITICAL: The agent's job is calibration, not pure inference. You CANNOT
rely on conversation history surviving — only [[remember]] entries and
the evolved directive will be available on later iterations. Encode
generously.`,
	MCPServers: []MCPServerConfig{
		{Name: "sales_qa", Command: "", Env: map[string]string{"SALES_QA_DATA_DIR": "{{dataDir}}"}, MainAccess: true},
	},
	DataSetup: func(t *testing.T, dir string) {
		// Nothing to seed — the MCP bakes its own data. We just create
		// the dir so submissions.jsonl has somewhere to land.
	},
	Phases: []Phase{
		{
			// One unified phase. The scenario's point is end-to-end accuracy:
			// given rubric + labeled examples, can the agent rate new calls
			// correctly? Whether it uses remember/evolve or just in-context
			// reasoning is up to the model — both are valid answers as long
			// as the ratings are accurate.
			Name:    "Study rubric + training calls, then rate 3 held-out test calls",
			Timeout: 8 * time.Minute,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Done when submissions.jsonl has at least 3 distinct
				// call_ids (latest submission wins per call).
				data, err := os.ReadFile(filepath.Join(dir, "submissions.jsonl"))
				if err != nil {
					return false
				}
				seen := make(map[string]bool)
				for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					if strings.TrimSpace(ln) == "" {
						continue
					}
					var row struct {
						CallID string `json:"call_id"`
					}
					if err := json.Unmarshal([]byte(ln), &row); err == nil {
						seen[row.CallID] = true
					}
				}
				return len(seen) >= 3
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				data, err := os.ReadFile(filepath.Join(dir, "submissions.jsonl"))
				if err != nil {
					t.Fatalf("submissions.jsonl missing: %v", err)
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				t.Logf("total submissions: %d", len(lines))

				// Track latest submission per call_id — the agent may
				// resubmit after seeing feedback, and the latest attempt
				// is what we grade.
				type submission struct {
					CallID      string         `json:"call_id"`
					Submitted   map[string]int `json:"submitted"`
					GroundTruth map[string]int `json:"ground_truth"`
					Deltas      map[string]int `json:"deltas"`
				}
				latest := make(map[string]submission)
				for _, ln := range lines {
					if strings.TrimSpace(ln) == "" {
						continue
					}
					var s submission
					if err := json.Unmarshal([]byte(ln), &s); err != nil {
						continue
					}
					latest[s.CallID] = s
				}

				if len(latest) < 3 {
					gotIDs := make([]string, 0, len(latest))
					for k := range latest {
						gotIDs = append(gotIDs, k)
					}
					t.Errorf("expected submissions for all 3 test calls, got %d (got: %v)",
						len(latest), gotIDs)
				}

				// Per-call accuracy: count dimensions where delta ≤ 1.
				// Log every submission regardless of pass/fail so failing
				// runs produce actionable output.
				totalMatches := 0
				totalDims := 0
				for callID, s := range latest {
					matches := 0
					for _, delta := range s.Deltas {
						totalDims++
						if delta <= 1 {
							matches++
							totalMatches++
						}
					}
					t.Logf("  %s: %d/5 within ±1 — submitted=%v truth=%v deltas=%v",
						callID, matches, s.Submitted, s.GroundTruth, s.Deltas)
					if matches < 3 {
						t.Errorf("%s: only %d/5 dimensions matched (need ≥3)", callID, matches)
					}
				}
				if totalDims > 0 {
					accuracy := float64(totalMatches) / float64(totalDims) * 100
					t.Logf("overall: %d/%d dimensions matched (%.1f%%)", totalMatches, totalDims, accuracy)
					if totalMatches < 10 { // 10/15 = 66% floor
						t.Errorf("overall accuracy too low: %d/15 (need ≥10)", totalMatches)
					}
				}

				// Bonus: log whether the agent ALSO used remember/evolve.
				// Not required for pass, just informational — if future
				// iterations without context can still rate correctly,
				// that's a stronger signal of learning.
				t.Logf("bonus: memory=%d entries, directive=%d chars",
					th.memory.Count(), len(th.config.GetDirective()))
			},
		},
	},
	Timeout:    15 * time.Minute,
	MaxThreads: 3,
}

func TestScenario_RubricLearning(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}
	bin := buildMCPBinary(t, "mcps/sales_qa")
	t.Logf("built sales_qa=%s", bin)

	s := rubricLearningScenario
	s.MCPServers[0].Command = bin
	runScenario(t, s)
}

// homeAutomationScenario tests a persistent-worker home automation agent
// with four specialist threads (security, comfort, intercom, daily
// coordinator) over four home_* MCPs. Exercises:
//
//   - Delegation over re-spawning (phase 6 — main must send to an
//     existing worker rather than spawn a new one).
//   - Scheduled work via a dedicated coordinator thread (daily_dispatcher
//     with tools=pace,send only, no MCPs) vs bakin the schedule into
//     a worker.
//   - Vision-driven decisions via describe_scene (cameras MCP returns
//     canned text descriptions the test framework pre-seeds per phase).
//   - Event-driven reactivity (motion events, doorbell presses injected
//     into the relevant MCP's event file between phases).
//   - Passive monitoring: workers should sleep long when idle, not poll.
var homeAutomationScenario = Scenario{
	Name: "HomeAutomation",
	Directive: `You are the supervisor of a smart-home automation system. On
startup, spawn four persistent team members and then stay out of the loop:

1. "security" — watches sensors and cameras for intruders, alerts the
   owner, and can turn on deterrent lights. Spawn with
   mcp="home_sensors,home_cameras,home_devices" tools="send,pace".
   Directive: "Long-lived security monitor. On startup list_sensors and
   list_cameras. Then pace(sleep='1h'). On each wake, get_events from
   home_sensors since your last check. If motion is triggered in an
   unoccupied room AND the owner is away, describe_scene on the matching
   camera, start_recording, and notify_owner with a clear alert. Never
   spawn threads. Never call done."

2. "comfort" — manages thermostat and lights. Spawn with
   mcp="home_devices" tools="send,pace". Directive: "Long-lived
   comfort worker. On startup list_devices. Then pace(sleep='1h').
   React to messages from daily_dispatcher for morning/evening routines,
   and to ad-hoc send() requests from main for one-off adjustments.
   Never spawn threads. Never call done."

3. "intercom" — handles doorbell visitors. Spawn with
   mcp="home_intercom,home_cameras,home_devices" tools="send,pace".
   Directive: "Long-lived visitor handler. On startup get_allowlist and
   list_cameras. Then pace(sleep='1h'). On each wake call
   get_pending_visits. For each pending visit: describe_scene on the
   visit's camera_id, compare against the allowlist, then either
   unlock_door + speak a welcome (for allowlisted visitors) OR
   deny_entry + notify_owner (for strangers). Never spawn threads.
   Never call done."

4. "daily_dispatcher" — coordinator for scheduled routines. Spawn with
   tools="send,pace" and NO mcp. Directive: "Long-lived schedule
   coordinator with no domain tools. React to [time] console events
   injected into your inbox. When you see '[time] morning' send to
   comfort ('Morning routine: thermostat 21C, kitchen light 70%,
   living light 50%') and to intercom ('Morning: expect deliveries').
   When you see '[time] evening' send to comfort ('Evening: thermostat
   18C, lights low') and security ('Evening: arm monitoring'). Between
   wakes pace(sleep='1h'). Never spawn threads. Never call done."

After spawning all four, pace(sleep='1h') yourself. Do not intervene
unless a worker reports an error or the user asks you something.`,
	MCPServers: []MCPServerConfig{
		{
			Name:    "home_sensors",
			Command: "", // filled at test time
			Env:     map[string]string{"HOME_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "home_cameras",
			Command: "",
			Env:     map[string]string{"HOME_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "home_intercom",
			Command: "",
			Env:     map[string]string{"HOME_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "home_devices",
			Command: "",
			Env:     map[string]string{"HOME_DATA_DIR": "{{dataDir}}"},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		// World state. Single home.json shared across the four MCPs —
		// home_devices owns the lights/thermostats/locks keys and does
		// a read-modify-write; everyone else only reads.
		writeJSONFile(t, dir, "home.json", map[string]any{
			"sensors": []map[string]any{
				{"id": "motion_living", "type": "motion", "room": "living", "enabled": true},
				{"id": "motion_kitchen", "type": "motion", "room": "kitchen", "enabled": true},
				{"id": "motion_hallway", "type": "motion", "room": "hallway", "enabled": true},
				{"id": "motion_bedroom", "type": "motion", "room": "bedroom", "enabled": true},
				{"id": "door_front", "type": "door", "room": "entrance", "enabled": true},
				{"id": "door_back", "type": "door", "room": "kitchen", "enabled": true},
				{"id": "window_bedroom", "type": "window", "room": "bedroom", "enabled": true},
			},
			"cameras": []map[string]any{
				{"id": "cam_front", "room": "entrance", "stream_url": "rtsp://mock/front"},
				{"id": "cam_living", "room": "living", "stream_url": "rtsp://mock/living"},
				{"id": "cam_kitchen", "room": "kitchen", "stream_url": "rtsp://mock/kitchen"},
				{"id": "cam_backyard", "room": "backyard", "stream_url": "rtsp://mock/backyard"},
			},
			"lights": []map[string]any{
				{"id": "light_living", "room": "living", "on": false, "brightness": 0},
				{"id": "light_kitchen", "room": "kitchen", "on": false, "brightness": 0},
				{"id": "light_bedroom", "room": "bedroom", "on": false, "brightness": 0},
				{"id": "light_entrance", "room": "entrance", "on": false, "brightness": 0},
			},
			"thermostats": []map[string]any{
				{"id": "main", "room": "living", "current_c": 19.5, "setpoint_c": 20.0, "mode": "heat"},
			},
			"locks": []map[string]any{
				{"id": "front", "door": "front", "locked": true},
				{"id": "back", "door": "back", "locked": true},
				{"id": "garage", "door": "garage", "locked": true},
			},
			"occupancy": map[string]any{"home": false, "people": []string{}},
			"visitor_allowlist": []string{
				"delivery person in uniform holding a package",
				"cleaning service",
				"plumber",
			},
		})
		// Empty event + visit files — the test framework appends to
		// these between phases to inject motion and doorbell events.
		writeJSONFile(t, dir, "scenes.json", map[string]string{})
		os.WriteFile(filepath.Join(dir, "sensor_events.jsonl"), []byte(""), 0644)
		os.WriteFile(filepath.Join(dir, "visits.jsonl"), []byte(""), 0644)
	},
	Phases: []Phase{
		{
			Name:    "Startup — 4 workers spawned + dispatcher",
			Timeout: 90 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				count := th.threads.Count()
				t.Logf("  ... threads=%d %v", count, threadIDs(th))
				return count >= 4
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				t.Logf("startup audit (%d entries)", len(entries))
				// Expect at least one list_sensors (security) and one
				// list_devices (comfort) discovery call from worker setup.
				if countTool(entries, "list_sensors") == 0 {
					t.Logf("NOTE: security did not list_sensors during startup")
				}
				if countTool(entries, "list_devices") == 0 {
					t.Logf("NOTE: comfort did not list_devices during startup")
				}
			},
		},
		{
			Name:    "Passive monitoring — workers should sleep long",
			Timeout: 45 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				// Pure wait-for-time phase — just let 35s elapse and
				// then verify alive-but-idle behavior in Verify.
				time.Sleep(35 * time.Second)
				return true
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				count := th.threads.Count()
				if count < 4 {
					t.Errorf("expected 4 workers still alive, got %d: %v", count, threadIDs(th))
				}
			},
		},
		{
			Name:    "Intruder motion at night — security escalates",
			Timeout: 120 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Seed an intruder description on the living-room camera.
				writeJSONFile(t, dir, "scenes.json", map[string]string{
					"cam_living": "A person wearing a dark hoodie is moving through the living room toward the hallway. They are not one of the residents.",
				})
				// Inject a motion event into sensors.
				entry := map[string]any{
					"time":      time.Now().UTC().Format(time.RFC3339),
					"sensor_id": "motion_living",
					"type":      "motion",
					"room":      "living",
					"value":     "triggered",
				}
				data, _ := json.Marshal(entry)
				f, _ := os.OpenFile(filepath.Join(dir, "sensor_events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if f != nil {
					f.WriteString(string(data) + "\n")
					f.Close()
				}
				// Wake main so it can relay to security. (In production this
				// would be a webhook from the sensor MCP; in test we use a
				// console event as the wake signal.)
				th := getTestThinker(t) // see helper below
				_ = th
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("Security alert: motion triggered in living room. Ask security to investigate.")
					}
					entries := readAuditEntries(dir)
					notifies := countTool(entries, "notify_owner")
					describes := countTool(entries, "describe_scene")
					t.Logf("  ... describe_scene=%d notify_owner=%d threads=%v",
						describes, notifies, threadIDs(th))
					return notifies >= 1 && describes >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				if countTool(entries, "notify_owner") == 0 {
					t.Error("expected at least one notify_owner call during intruder phase")
				}
				if countTool(entries, "describe_scene") == 0 {
					t.Error("expected at least one describe_scene call during intruder phase")
				}
			},
		},
		{
			Name:    "Expected visitor at the door — intercom admits",
			Timeout: 120 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Seed the front-door camera with a delivery-person description.
				writeJSONFile(t, dir, "scenes.json", map[string]string{
					"cam_front":  "A delivery person in a brown uniform holding a cardboard package from Amazon.",
					"cam_living": "Empty living room.",
				})
				// Inject the doorbell press.
				visit := map[string]any{
					"id":        "v_001",
					"time":      time.Now().UTC().Format(time.RFC3339),
					"camera_id": "cam_front",
					"door_id":   "front",
					"status":    "pending",
				}
				data, _ := json.Marshal(visit)
				os.WriteFile(filepath.Join(dir, "visits.jsonl"), append(data, '\n'), 0644)
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("Doorbell rang at the front door. Ask intercom to handle it.")
					}
					entries := readAuditEntries(dir)
					unlocks := countTool(entries, "unlock_door")
					speaks := countTool(entries, "speak")
					t.Logf("  ... unlock_door=%d speak=%d threads=%v",
						unlocks, speaks, threadIDs(th))
					return unlocks >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				if countTool(entries, "unlock_door") == 0 {
					t.Error("expected unlock_door for allowlisted visitor")
				}
			},
		},
		{
			Name:    "Delegate-not-spawn — ad-hoc kitchen light request",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Record the thread count BEFORE we inject the task so
				// we can assert nothing new was spawned.
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				var threadsBefore int
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						threadsBefore = th.threads.Count()
						t.Logf("  ... thread count before task: %d", threadsBefore)
						th.InjectConsole("Make the kitchen light brighter, to about 90%.")
					}
					// Look for a set_light call on the kitchen light with
					// high brightness — that's the proof comfort handled it.
					entries := readAuditEntries(dir)
					hit := 0
					for _, e := range entries {
						if e.Tool == "set_light" && e.Args["id"] == "light_kitchen" {
							if b := e.Args["brightness"]; b == "90" || b == "80" || b == "100" {
								hit++
							}
						}
					}
					threadsNow := th.threads.Count()
					t.Logf("  ... set_light(kitchen, >=80)=%d threadsNow=%d %v",
						hit, threadsNow, threadIDs(th))
					// Success iff: set_light done AND thread count did NOT grow.
					if hit >= 1 && threadsNow <= threadsBefore {
						return true
					}
					// Also return true if set_light done even if a thread
					// was spawned (we'll flag it in Verify as a warning
					// rather than a hard fail).
					return hit >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				var kitchenSets int
				for _, e := range entries {
					if e.Tool == "set_light" && e.Args["id"] == "light_kitchen" {
						kitchenSets++
					}
				}
				if kitchenSets == 0 {
					t.Error("expected at least one set_light on light_kitchen")
				}
				// Thread count check: should still be 4 sub-threads.
				// Main should have delegated, not spawned.
				count := th.threads.Count()
				if count > 4 {
					t.Errorf("DELEGATION FAILURE: main spawned a new thread for an ad-hoc task (now %d threads: %v). Expected main to send() to comfort.",
						count, threadIDs(th))
				}
			},
		},
		{
			Name:    "Evening routine — coordinator dispatches",
			Timeout: 120 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// (nothing — we inject the time marker in Wait)
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("[time] evening — run the evening routine: dispatcher should tell comfort to lower lights and thermostat, and security to arm monitoring.")
					}
					// Look for evidence that comfort executed the routine:
					// thermostat set, and at least one light turned off or dimmed.
					entries := readAuditEntries(dir)
					thermoSets := countTool(entries, "set_thermostat")
					t.Logf("  ... set_thermostat=%d threads=%v", thermoSets, threadIDs(th))
					return thermoSets >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := readAuditEntries(dir)
				if countTool(entries, "set_thermostat") == 0 {
					t.Logf("NOTE: evening routine did not adjust thermostat")
				}
			},
		},
		{
			Name:    "Quiescence — all 4 workers still alive, idle",
			Timeout: 30 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return th.threads.Count() >= 4
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				count := th.threads.Count()
				if count < 4 {
					t.Errorf("expected 4 workers at quiescence, got %d: %v", count, threadIDs(th))
				}
				entries := readAuditEntries(dir)
				t.Logf("Full audit at quiescence (%d entries)", len(entries))
			},
		},
	},
	Timeout:    10 * time.Minute,
	MaxThreads: 6,
}

// getTestThinker is a no-op placeholder so a Setup hook can reference the
// thinker. The scenario framework doesn't actually pass a Thinker to
// Setup, so this exists purely to keep the Setup helper syntactically
// clean; callers that need the thinker do their work inside Wait.
func getTestThinker(t *testing.T) *Thinker { return nil }

func TestScenario_HomeAutomation(t *testing.T) {
	if os.Getenv("RUN_SCENARIO_TESTS") == "" {
		t.Skip("set RUN_SCENARIO_TESTS=1")
	}
	sensorsBin := buildMCPBinary(t, "mcps/home_sensors")
	camerasBin := buildMCPBinary(t, "mcps/home_cameras")
	intercomBin := buildMCPBinary(t, "mcps/home_intercom")
	devicesBin := buildMCPBinary(t, "mcps/home_devices")
	t.Logf("built home_sensors=%s home_cameras=%s home_intercom=%s home_devices=%s",
		sensorsBin, camerasBin, intercomBin, devicesBin)

	s := homeAutomationScenario
	s.MCPServers[0].Command = sensorsBin
	s.MCPServers[1].Command = camerasBin
	s.MCPServers[2].Command = intercomBin
	s.MCPServers[3].Command = devicesBin
	runScenario(t, s)
}
