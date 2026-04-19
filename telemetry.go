package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TelemetryEvent is the unified event format — matches server schema.
type TelemetryEvent struct {
	ID         string          `json:"id"`
	InstanceID int64           `json:"instance_id,omitempty"`
	ThreadID   string          `json:"thread_id"`
	Type       string          `json:"type"`
	Time       time.Time       `json:"time"`
	Data       json.RawMessage `json:"data"`
}

// Telemetry collects events and forwards them to the server.
type Telemetry struct {
	mu             sync.Mutex
	log            []TelemetryEvent // stored events (forwarded to server)
	liveLog        []TelemetryEvent // all events including live-only (for SSE)
	notify         chan struct{}
	forwardCh      chan TelemetryEvent // serialized queue for live event forwarding
	serverURL        string // server URL (e.g. "http://localhost:5280")
	telemetryURL     string // full URL for batched stored events
	telemetryLiveURL string // full URL for live event forwarding
	instanceSecret   string // shared secret for telemetry auth
	instanceID     int64
	seq            int64
	quit           chan struct{}
}

func NewTelemetry() *Telemetry {
	t := &Telemetry{
		notify:    make(chan struct{}, 1),
		forwardCh: make(chan TelemetryEvent, 500),
		quit:      make(chan struct{}),
	}

	// Read instance ID from env (set by server when spawning)
	if id := os.Getenv("INSTANCE_ID"); id != "" {
		fmt.Sscanf(id, "%d", &t.instanceID)
	}

	// Read instance secret from env (for telemetry auth)
	t.instanceSecret = os.Getenv("INSTANCE_SECRET")

	// Telemetry endpoints are provided by the server via env vars.
	// Core doesn't need to know the server's route layout — fire-and-forget
	// to whatever URLs it was given.
	t.telemetryURL = os.Getenv("TELEMETRY_URL")
	t.telemetryLiveURL = os.Getenv("TELEMETRY_LIVE_URL")
	t.serverURL = os.Getenv("SERVER_URL")

	if t.telemetryURL != "" || t.telemetryLiveURL != "" {
		logMsg("TELEMETRY", fmt.Sprintf("telemetry URLs configured: stored=%s live=%s instanceID=%d",
			t.telemetryURL, t.telemetryLiveURL, t.instanceID))
		go t.forwardLoop()
		go t.liveForwardLoop()
	} else {
		logMsg("TELEMETRY", "no TELEMETRY_URL/TELEMETRY_LIVE_URL set — forwarding disabled")
	}

	return t
}

func (t *Telemetry) Stop() {
	close(t.quit)
}

// generateID returns a monotonically-increasing unique event id.
// Uses atomic increment so concurrent emit calls never collide on the
// same (ms, seq) tuple — previously this was a plain `t.seq++` outside
// the mutex, which occasionally produced duplicate ids under parallel
// thread dispatch and confused the dashboard's dedup.
func (t *Telemetry) generateID() string {
	seq := atomic.AddInt64(&t.seq, 1)
	return fmt.Sprintf("%d-%d", time.Now().UnixMilli(), seq)
}

// Emit records a telemetry event (stored + forwarded to server).
func (t *Telemetry) Emit(eventType, threadID string, data any) {
	t.emit(eventType, threadID, data, true)
}

// EmitLive records a telemetry event for SSE only (not forwarded to server).
func (t *Telemetry) EmitLive(eventType, threadID string, data any) {
	t.emit(eventType, threadID, data, false)
}

func (t *Telemetry) emit(eventType, threadID string, data any, store bool) {
	dataJSON, _ := json.Marshal(data)

	ev := TelemetryEvent{
		ID:         t.generateID(),
		InstanceID: t.instanceID,
		ThreadID:   threadID,
		Type:       eventType,
		Time:       time.Now(),
		Data:       json.RawMessage(dataJSON),
	}

	t.mu.Lock()
	if store {
		t.log = append(t.log, ev)
		logMsg("TELEMETRY", fmt.Sprintf("emit STORED %s (log=%d, url=%s)", eventType, len(t.log), t.telemetryURL))
		if len(t.log) > 2000 {
			t.log = t.log[len(t.log)-1000:]
		}
	} else {
		logMsg("TELEMETRY", fmt.Sprintf("emit LIVE-ONLY %s", eventType))
	}
	t.liveLog = append(t.liveLog, ev)
	if len(t.liveLog) > 2000 {
		t.liveLog = t.liveLog[len(t.liveLog)-1000:]
	}
	t.mu.Unlock()

	// Notify SSE watchers
	select {
	case t.notify <- struct{}{}:
	default:
	}

	// Forward ALL events to server for broadcast (live display on dashboard/console)
	if t.telemetryLiveURL != "" {
		select {
		case t.forwardCh <- ev:
		default:
			logMsg("TELEMETRY", fmt.Sprintf("forwardCh FULL, dropping %s", eventType))
		}
	}
}

// liveForwardLoop drains the forwardCh sequentially — one HTTP POST at a time.
// This guarantees chunks arrive at the server in the correct order.
func (t *Telemetry) liveForwardLoop() {
	for {
		select {
		case ev := <-t.forwardCh:
			t.forwardLive(ev)
		case <-t.quit:
			return
		}
	}
}

func (t *Telemetry) forwardLive(ev TelemetryEvent) {
	body, err := json.Marshal([]TelemetryEvent{ev})
	if err != nil {
		return
	}
	req, err := http.NewRequest("POST", t.telemetryLiveURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if t.instanceSecret != "" {
		req.Header.Set("X-Instance-Secret", t.instanceSecret)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logMsg("TELEMETRY", fmt.Sprintf("forwardLive: POST error for %s: %v", ev.Type, err))
		return
	}
	resp.Body.Close()
}

// Events returns all events (including live-only) since the given index. Used by SSE.
// If the log was truncated (since > len), reset to return everything available.
func (t *Telemetry) Events(since int) ([]TelemetryEvent, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if since > len(t.liveLog) {
		since = 0
	}
	if since == len(t.liveLog) {
		return nil, len(t.liveLog)
	}
	events := make([]TelemetryEvent, len(t.liveLog)-since)
	copy(events, t.liveLog[since:])
	return events, len(t.liveLog)
}

// StoredEvents returns only stored events since the given index. Used by forwardLoop.
// If the log was truncated (since > len), reset to return everything available.
func (t *Telemetry) StoredEvents(since int) ([]TelemetryEvent, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if since > len(t.log) {
		// Log was truncated — reset to drain everything remaining
		since = 0
	}
	if since == len(t.log) {
		return nil, len(t.log)
	}
	events := make([]TelemetryEvent, len(t.log)-since)
	copy(events, t.log[since:])
	return events, len(t.log)
}

// forwardLoop batches stored events and POSTs them to the server for DB
// persistence. On transient failures (network error, non-2xx) the cursor
// does NOT advance and the same batch is re-sent after an exponential
// backoff (1s → 30s). The server dedups by event.id PRIMARY KEY (INSERT
// OR IGNORE) so retries are safe and never double-insert.
//
// Backoff resets to the base interval on any successful POST. The stored
// event log is bounded (truncated in emit() past 2000 entries); if the
// server is unreachable long enough for truncation to run, StoredEvents()
// resets the cursor to 0 so we drain whatever remains rather than stalling.
func (t *Telemetry) forwardLoop() {
	const (
		baseInterval = 1 * time.Second
		maxInterval  = 30 * time.Second
	)
	interval := baseInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()

	var lastSent int
	var consecutiveFailures int
	client := &http.Client{Timeout: 5 * time.Second}

	logMsg("TELEMETRY", fmt.Sprintf("forwardLoop started, url=%s", t.telemetryURL))

	for {
		select {
		case <-timer.C:
			// Default: reset to base cadence. Overridden below if this
			// iteration fails.
			next := baseInterval

			if t.telemetryURL == "" {
				timer.Reset(next)
				continue
			}
			events, total := t.StoredEvents(lastSent)
			if len(events) == 0 {
				timer.Reset(next)
				continue
			}

			types := make([]string, len(events))
			for i, e := range events {
				types[i] = e.Type
			}
			logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: sending %d events to %s: %v", len(events), t.telemetryURL, types))

			body, err := json.Marshal(events)
			if err != nil {
				// Marshal failure is deterministic — retrying won't help.
				// Advance past this batch so we don't spin forever.
				logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: marshal error (dropping batch): %v", err))
				lastSent = total
				timer.Reset(next)
				continue
			}

			ok := t.postBatch(client, body)
			if ok {
				lastSent = total
				if consecutiveFailures > 0 {
					logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: recovered after %d failures", consecutiveFailures))
				}
				consecutiveFailures = 0
				interval = baseInterval
			} else {
				consecutiveFailures++
				// Exponential backoff, capped. Log every failure so
				// outages are visible in the logs.
				interval *= 2
				if interval > maxInterval {
					interval = maxInterval
				}
				next = interval
				logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: retry in %s (consecutive failures=%d, queued=%d)",
					next, consecutiveFailures, len(events)))
			}
			timer.Reset(next)

		case <-t.quit:
			return
		}
	}
}

// postBatch POSTs a serialized batch of events. Returns true on 2xx.
// Any network error or non-2xx status is treated as a retryable failure —
// the caller keeps lastSent unchanged so the same batch is re-sent after
// backoff.
func (t *Telemetry) postBatch(client *http.Client, body []byte) bool {
	req, err := http.NewRequest("POST", t.telemetryURL, bytes.NewReader(body))
	if err != nil {
		logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: request error: %v", err))
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if t.instanceSecret != "" {
		req.Header.Set("X-Instance-Secret", t.instanceSecret)
	}

	resp, err := client.Do(req)
	if err != nil {
		logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: POST error: %v", err))
		return false
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logMsg("TELEMETRY", fmt.Sprintf("forwardLoop: POST %s status=%d body=%s",
			t.telemetryURL, resp.StatusCode, string(respBody)))
		return false
	}
	return true
}

// --- Convenience emitters with typed data ---

type LLMDoneData struct {
	Model        string  `json:"model"`
	TokensIn     int     `json:"tokens_in"`
	TokensCached int     `json:"tokens_cached"`
	TokensOut    int     `json:"tokens_out"`
	DurationMs   int64   `json:"duration_ms"`
	CostUSD      float64 `json:"cost_usd"`
	Iteration    int     `json:"iteration"`
	Rate         string  `json:"rate"`
	ContextMsgs  int     `json:"context_msgs"`
	ContextChars int     `json:"context_chars"`
	// MaxContextTokens is the model's advertised input-context window
	// (in tokens). Comes from a static lookup keyed on the model id —
	// see ModelContextWindow. 0 when the model isn't in the table; UI
	// should treat 0 as "unknown" and skip percentage rendering.
	MaxContextTokens int `json:"max_context_tokens,omitempty"`
	MemoryCount      int `json:"memory_count"`
	ThreadCount      int `json:"thread_count"`
	Message          string `json:"message,omitempty"`
}

type LLMChunkData struct {
	Text      string `json:"text"`
	Iteration int    `json:"iteration"`
}

type LLMErrorData struct {
	Model     string `json:"model"`
	Error     string `json:"error"`
	Iteration int    `json:"iteration"`
}

type ThreadSpawnData struct {
	ParentID  string   `json:"parent_id"`
	Directive string   `json:"directive"`
	Tools     []string `json:"tools"`
}

type ThreadDoneData struct {
	ParentID string `json:"parent_id"`
	Result   string `json:"result,omitempty"`
}

type ThreadMessageData struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Message string `json:"message"`
}

type ToolCallData struct {
	ID     string            `json:"id,omitempty"`
	Name   string            `json:"name"`
	Args   map[string]string `json:"args,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

type ToolResultData struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	DurationMs int64  `json:"duration_ms"`
	Success    bool   `json:"success"`
	Result     string `json:"result,omitempty"`
}

type DirectiveChangeData struct {
	Old string `json:"old,omitempty"`
	New string `json:"new"`
}

// --- Cost calculation (matches TUI pricing) ---

const (
	costInputPerMillion  = 0.60
	costCachedPerMillion = 0.10
	costOutputPerMillion = 3.00
)

// ModelContextWindow returns the advertised input-context window (in
// tokens) for a given model id. Pure static lookup — no network, no
// API call, ~hundreds of nanoseconds. Returns 0 if the model isn't in
// the table; the UI treats 0 as "unknown" and skips the % display.
//
// Numbers come from each provider's own model documentation. Update
// when a new model ships or a provider changes a limit. Match is by
// substring so we tolerate the various id forms providers use
// (e.g. "claude-opus-4-7", "claude-opus-4-7-20251119", "claude-opus-4-7[1m]").
//
// Order matters: longer / more specific keys checked first so
// "claude-opus-4-7[1m]" matches the 1M variant before falling through
// to the generic "claude-opus-4-7" 200K entry.
func ModelContextWindow(modelID string) int {
	if modelID == "" {
		return 0
	}
	// Order longest-prefix first to win over shorter substrings.
	table := []struct {
		match  string
		tokens int
	}{
		// --- Anthropic (Claude) ---
		// 1M-context variants are explicitly tagged.
		{"claude-opus-4-7[1m]", 1_000_000},
		{"claude-opus-4-6[1m]", 1_000_000},
		{"claude-opus-4-5[1m]", 1_000_000},
		{"claude-sonnet-4-6[1m]", 1_000_000},
		{"claude-sonnet-4-5[1m]", 1_000_000},
		// Standard 200K Claude family.
		{"claude-opus-4", 200_000},
		{"claude-sonnet-4", 200_000},
		{"claude-haiku-4", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"claude-3-5-haiku", 200_000},
		{"claude-3-opus", 200_000},
		{"claude-3-sonnet", 200_000},
		{"claude-3-haiku", 200_000},

		// --- Fireworks (Moonshot Kimi) ---
		// Kimi K2.5 turbo via Fireworks router is 256K input context.
		{"kimi-k2p5", 256_000},
		{"kimi-k2", 128_000},

		// --- OpenAI ---
		{"gpt-4.1", 1_000_000},
		{"gpt-4o-mini", 128_000},
		{"gpt-4o", 128_000},
		{"gpt-4-turbo", 128_000},
		{"gpt-4", 8_192},
		{"gpt-3.5", 16_385},
		{"o3-mini", 200_000},
		{"o3", 200_000},
		{"o1-mini", 128_000},
		{"o1", 200_000},

		// --- Google (Gemini) ---
		{"gemini-2.5-pro", 2_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-2.0-pro", 2_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},
		{"gemini-1.5-flash", 1_000_000},

		// --- Local / generic ---
		{"llama3.1", 128_000},
		{"llama-3.1", 128_000},
		{"llama3", 8_192},
	}
	low := strings.ToLower(modelID)
	for _, e := range table {
		if strings.Contains(low, strings.ToLower(e.match)) {
			return e.tokens
		}
	}
	return 0
}

func calculateCost(usage TokenUsage) float64 {
	uncached := usage.PromptTokens - usage.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*costInputPerMillion +
		float64(usage.CachedTokens)*costCachedPerMillion +
		float64(usage.CompletionTokens)*costOutputPerMillion) / 1_000_000
}
