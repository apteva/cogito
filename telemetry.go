package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
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
	mu         sync.Mutex
	log        []TelemetryEvent // stored events (forwarded to server)
	liveLog    []TelemetryEvent // all events including live-only (for SSE)
	notify     chan struct{}
	forwardCh  chan TelemetryEvent // serialized queue for live event forwarding
	serverURL string // server URL (e.g. "http://localhost:5280")
	instanceID int64
	seq        int64
	quit       chan struct{}
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

	// Configure server URL for fire-and-forget
	if url := os.Getenv("SERVER_URL"); url != "" {
		t.serverURL = url
		go t.forwardLoop()
		go t.liveForwardLoop() // serialized live event forwarding
	}

	return t
}

func (t *Telemetry) Stop() {
	close(t.quit)
}

func (t *Telemetry) generateID() string {
	t.seq++
	return fmt.Sprintf("%d-%d", time.Now().UnixMilli(), t.seq)
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
		if len(t.log) > 2000 {
			t.log = t.log[len(t.log)-1000:]
		}
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

	// Queue for ordered forwarding (async goroutines cause chunk reordering)
	if !store && t.serverURL != "" {
		select {
		case t.forwardCh <- ev:
		default:
			// Drop if queue full (back-pressure)
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
	req, err := http.NewRequest("POST", t.serverURL+"/telemetry/live", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// Events returns all events (including live-only) since the given index. Used by SSE.
func (t *Telemetry) Events(since int) ([]TelemetryEvent, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if since >= len(t.liveLog) {
		return nil, len(t.liveLog)
	}
	events := make([]TelemetryEvent, len(t.liveLog)-since)
	copy(events, t.liveLog[since:])
	return events, len(t.liveLog)
}

// StoredEvents returns only stored events since the given index. Used by forwardLoop.
func (t *Telemetry) StoredEvents(since int) ([]TelemetryEvent, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if since >= len(t.log) {
		return nil, len(t.log)
	}
	events := make([]TelemetryEvent, len(t.log)-since)
	copy(events, t.log[since:])
	return events, len(t.log)
}

// forwardLoop batches events and POSTs them to the server every second.
func (t *Telemetry) forwardLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastSent int
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-ticker.C:
			events, total := t.StoredEvents(lastSent)
			if len(events) == 0 {
				continue
			}

			body, err := json.Marshal(events)
			if err != nil {
				continue
			}

			req, err := http.NewRequest("POST", t.serverURL+"/telemetry", bytes.NewReader(body))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")

			// Fire and forget — don't block on response
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				lastSent = total
			}
			// On error, we'll retry next tick with the same events

		case <-t.quit:
			return
		}
	}
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
	MemoryCount  int     `json:"memory_count"`
	ThreadCount  int     `json:"thread_count"`
	Message      string  `json:"message,omitempty"`
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
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type ToolResultData struct {
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

func calculateCost(usage TokenUsage) float64 {
	uncached := usage.PromptTokens - usage.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*costInputPerMillion +
		float64(usage.CachedTokens)*costCachedPerMillion +
		float64(usage.CompletionTokens)*costOutputPerMillion) / 1_000_000
}
