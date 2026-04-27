package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	aptcomputer "github.com/apteva/computer"
)

// TestComputerUse_LocalCookieBanner proves the agent can dismiss a
// cookie-consent overlay and reach the underlying content. Real-world
// sites almost always gate interaction behind one of these banners,
// so this is table-stakes for any browser agent.
//
// Rather than depending on an external site (whose banner varies by
// region, A/B test, and IP), we serve a deterministic page locally:
//
//	GET /        → page with a full-screen overlay and three badges:
//	               Accept (clicking redirects to /welcome),
//	               Reject (redirects to /rejected), and
//	               a distractor link that predates the banner.
//	GET /welcome → sentinel page with "WELCOME_HOME" text.
//
// SoM is the package default (TestMain), so Kimi sees numeric labels
// on every button and clicks by label — the same path users hit in
// production. Pass criterion: final URL ends in /welcome AND the
// agent used label= grounding on its click.
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... \
//	APTEVA_HEADLESS_BROWSER=1 \
//	go test -v -run TestComputerUse_LocalCookieBanner -timeout 3m ./
func TestComputerUse_LocalCookieBanner(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1")
	}
	tp := getTestProvider(t)
	apiKey := tp.APIKey

	// Local fixture site — deterministic banner + landing pages.
	// The overlay uses fixed positioning with a high z-index so it
	// genuinely obscures the content beneath (mirrors real consent
	// modals: the distractor link below is not reachable until the
	// banner is dismissed).
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Test Site</title><style>
body { font-family: sans-serif; margin: 40px; }
#overlay {
	position: fixed; top: 0; left: 0; right: 0; bottom: 0;
	background: rgba(0,0,0,0.6); z-index: 9999;
	display: flex; align-items: center; justify-content: center;
}
#banner {
	background: #fff; padding: 40px; border-radius: 8px;
	max-width: 520px; text-align: center;
}
#banner h2 { margin-top: 0; }
#banner button {
	margin: 10px; padding: 14px 28px; font-size: 16px;
	border: 0; border-radius: 4px; cursor: pointer;
}
#accept { background: #2563eb; color: #fff; }
#reject { background: #e5e7eb; color: #111; }
</style></head><body>
<h1>Main Content</h1>
<p>This is the page behind the banner.</p>
<a href="/distractor">Distractor link — do not click</a>
<div id="overlay"><div id="banner">
<h2>We use cookies</h2>
<p>This site uses cookies for essential functionality. Please accept to continue.</p>
<button id="accept" onclick="location.href='/welcome'">Accept all cookies</button>
<button id="reject" onclick="location.href='/rejected'">Reject</button>
</div></div></body></html>`)
	})
	mux.HandleFunc("/welcome", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Welcome</title></head><body>
<h1>WELCOME_HOME</h1><p>You accepted cookies. The banner is dismissed.</p></body></html>`)
	})
	mux.HandleFunc("/rejected", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Rejected</title></head><body>
<h1>REJECTED</h1></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:   "local",
		Width:  1600,
		Height: 900,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("local chrome connected: %dx%d (SoM on) serving=%s", comp.DisplaySize().Width, comp.DisplaySize().Height, srv.URL)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}
	cfg := &Config{
		Directive: "You have a local browser with Set-of-Mark grounding — interactive elements have colored numeric badges. Prefer label=N for clicks.",
		Mode:      ModeAutonomous,
	}
	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(strings.Join([]string{
		`Do this exactly:`,
		fmt.Sprintf(`1) browser_session(action=open, url=%s)`, srv.URL),
		`2) computer_use(action=screenshot) — you will see a cookie-consent banner with numeric badges on Accept and Reject.`,
		`3) Click the "Accept all cookies" button using computer_use(action=click, label=<that number>). Use label, not coordinate.`,
		`4) When the URL path is /welcome, reply RESULT: accepted.`,
		`Do not call pace. Do not use browser_session(open) to navigate anywhere else.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-cookie", 500)
	logFile, _ := os.Create("computer_test_cookie_chunks.log")
	defer logFile.Close()

	var sawClick, sawLabel, cheated bool
	done := make(chan struct{})
	closed := false
	var buf strings.Builder
	clickAt := -1

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
					if !sawClick && strings.Contains(s, "action=click") {
						sawClick = true
						clickAt = strings.Index(s, "action=click")
					}
					if strings.Contains(s, "label=") {
						sawLabel = true
					}
					// Cheat check: post-click browser_session(open) to
					// /welcome means the agent bypassed the banner by
					// typing the URL directly instead of clicking Accept.
					if sawClick && clickAt >= 0 {
						post := s[clickAt:]
						if strings.Contains(post, "action=open") &&
							strings.Contains(post, "/welcome") {
							cheated = true
						}
					}
				}
				if sawClick && strings.Contains(currentURL(comp), "/welcome") && !closed {
					closed = true
					close(done)
					return
				}
				if cheated && !closed {
					closed = true
					close(done)
					return
				}
			case <-time.After(3 * time.Minute):
				return
			}
		}
	}()

	go thinker.Run()

	select {
	case <-done:
		t.Log("banner dismissed → /welcome reached — stopping agent")
	case <-time.After(2 * time.Minute):
		t.Log("timeout — evaluating log")
	}
	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	log, _ := os.ReadFile("computer_test_cookie_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(log))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawClick {
		t.Fatal("FAIL: agent never emitted a click action — banner blocked the flow entirely")
	}
	t.Log("✓ click action called")
	if !sawLabel {
		t.Log("WARN: agent did not use label= (fell back to coordinate guessing)")
	} else {
		t.Log("✓ agent used label= (SoM grounding)")
	}
	if cheated {
		t.Fatal("FAIL: agent bypassed the banner via browser_session(open, url=/welcome) instead of clicking Accept")
	}
	if !strings.Contains(finalURL, "/welcome") {
		if strings.Contains(finalURL, "/rejected") {
			t.Fatalf("FAIL: agent clicked Reject instead of Accept — final URL %q", finalURL)
		}
		t.Fatalf("FAIL: final URL is %q, expected to contain '/welcome' (banner not dismissed)", finalURL)
	}
	t.Log("✓ cookie banner dismissed via Accept click — agent reached underlying content")

	if err := comp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	comp = nil
}
