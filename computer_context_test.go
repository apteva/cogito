package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	aptcomputer "github.com/apteva/computer"
)

// TestComputerUse_BrowserEngineContextRoundtrip mirrors the cookie-
// banner test (computer_cookie_test.go) but exercises the persistent-
// context tool path end-to-end. The agent runs two sessions back to
// back under the same context_id; the second must see state seeded by
// the first.
//
// Why redirects, not body inspection: ground truth is server-decided.
// /me 302s to /logged-in-as-42 if the cookie is present, /no-cookie if
// not. Reading currentURL(comp) at the end of the run tells us
// definitively whether the context persisted, with no DOM evaluation
// and no agent self-report to second-guess.
//
// Why two opens (not resume): contexts are about identity across
// fresh sessions, not reattach. Resume is exercised separately by
// computer/browserengine/browserengine_test.go::TestLiveReattach.
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... BROWSER_API_KEY=be_... \
//	go test -v -run TestComputerUse_BrowserEngineContextRoundtrip -timeout 6m ./
func TestComputerUse_BrowserEngineContextRoundtrip(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1")
	}
	tp := getTestProvider(t)
	apiKey := tp.APIKey
	beKey := os.Getenv("BROWSER_API_KEY")
	if beKey == "" {
		t.Skip("BROWSER_API_KEY not set")
	}
	beURL := os.Getenv("BROWSER_API_URL")
	if beURL == "" {
		beURL = "https://api.browserengine.co"
	}

	// Local fixture site — same /seed + /me + sentinel URLs as the
	// direct test in computer/browserengine/browserengine_test.go,
	// duplicated here because Go test files don't share helpers
	// across packages and the surface is small.
	srv := makeContextSite(t)
	defer srv.Close()

	// Pre-create the context on Browser Engine. Operators do this in
	// production; the agent only knows the resulting id.
	ctxID := createBEContext(t, beKey, beURL)
	t.Logf("created context %s", ctxID)
	defer deleteBEContext(t, beKey, beURL, ctxID)

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:   "browser-engine",
		APIKey: beKey,
		URL:    beURL,
		Width:  1280,
		Height: 720,
	})
	if err != nil {
		t.Fatalf("create computer: %v", err)
	}
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("browser engine computer ready: %dx%d", comp.DisplaySize().Width, comp.DisplaySize().Height)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}
	cfg := &Config{
		Directive: "You have a remote browser via Browser Engine. Use the browser_session and computer_use tools. The operator has set up a persistent context for you — when you open with context_id, cookies and storage from prior sessions are pre-loaded.",
		Mode:      ModeAutonomous,
	}
	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(strings.Join([]string{
		`Do EXACTLY this sequence — no improvising, no extra navigation, no shortcuts:`,
		fmt.Sprintf(`1) browser_session(action=open, url=%s/seed, context_id=%s, persist=true, timeout=300)`, srv.URL, ctxID),
		`   This visits a page that sets an auth cookie inside the persistent context.`,
		`2) computer_use(action=screenshot) — confirm you see "SEEDED: 42".`,
		`3) browser_session(action=close) — this snapshots the cookie back into the context.`,
		fmt.Sprintf(`4) browser_session(action=open, url=%s/me, context_id=%s, persist=true, timeout=300)`, srv.URL, ctxID),
		`   This is a fresh session bound to the same context. /me is a redirect that lands you on /logged-in-as-42 if the cookie persisted, /no-cookie otherwise.`,
		`5) computer_use(action=screenshot) — read the URL bar.`,
		`6) Reply with exactly "RESULT: ok" if the URL contains "/logged-in-as-42", or "RESULT: fail" otherwise.`,
		`Do not call pace. Do not use browser_session(open) for any URL outside this sequence.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-context", 500)
	logFile, _ := os.Create("computer_test_context_chunks.log")
	defer logFile.Close()

	var sawSeedOpen, sawClose, sawMeOpen bool
	var buf strings.Builder
	done := make(chan struct{})
	closed := false

	go func() {
		for {
			select {
			case ev := <-obs.C:
				if ev.Type == EventThinkDone {
					fmt.Fprintf(logFile, "\n=== THOUGHT #%d DONE (tok=%d/%d) ===\n",
						ev.Iteration, ev.Usage.PromptTokens, ev.Usage.CompletionTokens)
				}
				if ev.Type == EventChunk {
					fmt.Fprintf(logFile, "%s", ev.Text)
					buf.WriteString(ev.Text)
					s := buf.String()
					// Track tool-call milestones in order. Strict ordering
					// catches an agent that skips close (which would also
					// skip context persistence) but still hits /me.
					if !sawSeedOpen && strings.Contains(s, "action=open") && strings.Contains(s, "/seed") {
						sawSeedOpen = true
					}
					if sawSeedOpen && !sawClose && strings.Contains(s, "action=close") {
						sawClose = true
					}
					if sawClose && !sawMeOpen && strings.Contains(s, "action=open") {
						// /me is the only other open URL the directive permits;
						// scope the substring check accordingly to avoid a
						// retry on /seed counting as a /me open.
						idx := strings.LastIndex(s, "action=open")
						if idx >= 0 && strings.Contains(s[idx:], "/me") {
							sawMeOpen = true
						}
					}
					if (strings.Contains(s, "RESULT: ok") || strings.Contains(s, "RESULT: fail")) && !closed {
						closed = true
						close(done)
						return
					}
				}
			case <-time.After(5 * time.Minute):
				return
			}
		}
	}()

	go thinker.Run()

	select {
	case <-done:
		t.Log("agent emitted RESULT — stopping")
	case <-time.After(4 * time.Minute):
		t.Log("timeout — evaluating log")
	}
	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	log, _ := os.ReadFile("computer_test_context_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(log))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawSeedOpen {
		t.Fatal("FAIL: agent never opened /seed with context_id — context-bound open path not exercised")
	}
	t.Log("✓ agent opened /seed with context_id")
	if !sawClose {
		t.Fatal("FAIL: agent never called browser_session(close) between the two opens — context never got a chance to persist")
	}
	t.Log("✓ agent closed the first session")
	if !sawMeOpen {
		t.Fatal("FAIL: agent never opened /me with context_id — context reuse not exercised")
	}
	t.Log("✓ agent opened /me with context_id (second session, same context)")

	// Ground truth: server's /me redirect lands on /logged-in-as-42 iff
	// the cookie persisted. /no-cookie means the context didn't carry it.
	if strings.Contains(finalURL, "/no-cookie") {
		t.Fatalf("FAIL: cookie did NOT persist across sessions — final URL %q", finalURL)
	}
	if !strings.Contains(finalURL, "/logged-in-as-42") {
		t.Fatalf("FAIL: agent did not reach the post-redirect URL — final %q (expected /logged-in-as-42)", finalURL)
	}
	t.Log("✓ context persisted: cookie set in session 1 was visible to /me in session 2")

	if err := comp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	comp = nil
}

// makeContextSite is the test fixture for context-roundtrip flows.
// Same shape as newCookieSite in computer/browserengine — see that
// file for the rationale on redirect-driven assertions.
func makeContextSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/seed", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "auth", Value: "42", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Seed</title></head><body><h1>SEEDED: 42</h1><p>The context now holds an auth cookie.</p></body></html>`)
	})
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("auth"); err == nil && c.Value == "42" {
			http.Redirect(w, r, "/logged-in-as-42", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/no-cookie", http.StatusFound)
	})
	mux.HandleFunc("/logged-in-as-42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>OK</title></head><body><h1>logged in: 42</h1></body></html>`)
	})
	mux.HandleFunc("/no-cookie", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>FAIL</title></head><body><h1>not logged in</h1></body></html>`)
	})
	return httptest.NewServer(mux)
}

func createBEContext(t *testing.T, apiKey, base string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":     fmt.Sprintf("core-context-test-%d", time.Now().UnixNano()),
		"metadata": map[string]any{"source": "core.computer_context_test"},
	})
	req, _ := http.NewRequest("POST", base+"/contexts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create context: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create context: HTTP %d: %s", resp.StatusCode, string(b))
	}
	var got struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode create context: %v", err)
	}
	if got.Data.ID == "" {
		t.Fatal("create context: no id in response")
	}
	return got.Data.ID
}

func deleteBEContext(t *testing.T, apiKey, base, id string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", base+"/contexts/"+id, nil)
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("delete context %s: %v", id, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Logf("delete context %s: HTTP %d", id, resp.StatusCode)
	}
}
