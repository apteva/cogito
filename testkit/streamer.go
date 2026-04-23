package testkit

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Live event streamer. The apteva-server's own terminal rendering goes
// silent during long-running tool calls (e.g. a 30-second
// deepgram_listen HTTP wait) because no telemetry fires between
// tool.call and tool.result. Staring at a frozen screen tempts a panic
// Ctrl+C, so the streamer polls telemetry on a timer and prints a
// compact one-liner for every new event of interest (tool.call,
// tool.result, thread.spawn, thread.message, llm.error,
// directive.evolved). Chatty or redundant types (llm.chunk,
// llm.tool_chunk, llm.done, llm.thinking, event.received) are filtered
// out — if you need them, set TESTKIT_STREAM_ALL=1.
//
// There is NO heartbeat for outstanding tool.calls: the server
// truncates its telemetry response at 200 events, and under high event
// throughput a tool.result can fall outside the window before we poll
// again, leaving phantom "still running" entries. The Phase.WaitUntil
// nudge (every 15s) covers the same need without the false positives.
//
// Disable entirely with TESTKIT_NO_STREAM=1. The autostart log (server
// stderr) still flows to the terminal regardless.
type streamer struct {
	s      *Session
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Track seen event ids so we only print each event once — event
	// timestamps can collide at millisecond resolution when several
	// events fire from the same iteration.
	seen map[string]struct{}
}

const pollInterval = 1 * time.Second

// startStreamer kicks off the background watcher. Safe to call multiple
// times — extra calls are no-ops. Stops on session cleanup.
func (s *Session) startStreamer() {
	if os.Getenv("TESTKIT_NO_STREAM") != "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &streamer{
		s:      s,
		ctx:    ctx,
		cancel: cancel,
		seen:   make(map[string]struct{}),
	}
	st.wg.Add(1)
	go st.run()
	s.t.Cleanup(func() {
		cancel()
		st.wg.Wait()
	})
}

func (st *streamer) run() {
	defer st.wg.Done()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-st.ctx.Done():
			return
		case <-tick.C:
			st.drainOnce()
		}
	}
}

// drainOnce fetches recent telemetry and prints anything we haven't
// seen before. Also emits heartbeats for overdue tool.calls.
func (st *streamer) drainOnce() {
	// Fetch all event types at once — filtering happens client-side so
	// we don't issue N requests per tick. 200 is plenty of headroom for
	// a 1s poll window.
	events := st.s.eventsRaw("", 200)

	// Events arrive newest-first; walk oldest-first so printing order
	// matches chronological order.
	sort.SliceStable(events, func(i, j int) bool { return events[i].Time < events[j].Time })

	for _, e := range events {
		if _, ok := st.seen[e.ID]; ok {
			continue
		}
		st.seen[e.ID] = struct{}{}
		st.handleEvent(e)
	}
}

// eventsRaw is like Events but non-fatal on errors — the streamer
// running in a goroutine must never panic the test on a transient
// server blip.
func (s *Session) eventsRaw(typeFilter string, limit int) []TelemetryEvent {
	if limit <= 0 {
		limit = 200
	}
	path := fmt.Sprintf("/telemetry?instance_id=%d&limit=%d", s.instanceID, limit)
	if typeFilter != "" {
		path += "&type=" + typeFilter
	}
	var out []TelemetryEvent
	_ = s.get(path, &out)
	return out
}

func (st *streamer) handleEvent(e TelemetryEvent) {
	streamAll := os.Getenv("TESTKIT_STREAM_ALL") != ""

	switch e.Type {
	case "tool.call":
		name, _ := e.Data["name"].(string)
		args := shortArgs(e.Data["args"])
		st.print(e, fmt.Sprintf("▶ %s(%s)", name, args))

	case "tool.result":
		name, _ := e.Data["name"].(string)
		result, _ := e.Data["result"].(string)
		st.print(e, fmt.Sprintf("✓ %s → %s", name, trim(result, 120)))

	case "thread.spawn":
		st.print(e, "⚙ spawn")

	case "thread.message":
		from, _ := e.Data["from"].(string)
		to, _ := e.Data["to"].(string)
		msg, _ := e.Data["message"].(string)
		st.print(e, fmt.Sprintf("✉ %s → %s: %s", from, to, trim(msg, 120)))

	case "llm.error":
		errMsg, _ := e.Data["error"].(string)
		st.print(e, fmt.Sprintf("✗ llm.error: %s", trim(errMsg, 200)))

	case "directive.evolved":
		d, _ := e.Data["new"].(string)
		st.print(e, fmt.Sprintf("↻ directive: %s", trim(d, 100)))

	default:
		if streamAll {
			st.print(e, fmt.Sprintf("· %s", e.Type))
		}
	}
}

func (st *streamer) print(e TelemetryEvent, body string) {
	st.s.t.Logf("[stream %s] %s", e.ThreadID, body)
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// shortArgs renders a tool.call args map as a compact "k=v,k=v" preview.
// Only first-level keys, values clamped to ~40 chars each. Long arg
// bodies (big directive strings, base64 payloads) would dominate the
// line otherwise.
func shortArgs(v any) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, trim(fmt.Sprint(m[k]), 40)))
	}
	return strings.Join(parts, ", ")
}
