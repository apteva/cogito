package core

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	aptcomputer "github.com/apteva/computer"
)

// TestComputerUse_BrowserEngineContextAndProxy is a single agent-driven
// flow that demonstrates both:
//
//  1. The agent threading a context_id through browser_session.open at
//     runtime (operator pre-creates the context, agent picks it up via
//     the tool param).
//  2. The session being routed through Browser Engine's residential
//     proxy in the configured country.
//
// We point the agent at https://ifconfig.co/country-iso — a third-party
// endpoint that returns just the requesting IP's two-letter country
// code as plain text (e.g. "US\n"). With the proxy enabled and
// country=us the page text is "US"; if the proxy was silently bypassed
// we'd see the country of the BE worker host (typically not US).
//
// Plain-text-on-page over JSON: Chrome renders ipinfo.io/json as a
// JSON tree-view that Kimi cannot reliably read from a screenshot
// (verified empirically — agent saw the page but emitted an empty
// country code). A page whose entire body is the answer is much
// kinder to vision-grounded models.
//
// Why one combined test instead of two: this is a smoke test, not a
// matrix. The two capabilities are orthogonal at the wire level
// (context binds at session-create, proxy is set at instance-create)
// so passing both in a single open call is the closest analogue of
// real production agent behavior.
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... BROWSER_API_KEY=be_... \
//	go test -v -run TestComputerUse_BrowserEngineContextAndProxy -timeout 6m ./
func TestComputerUse_BrowserEngineContextAndProxy(t *testing.T) {
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

	// Pre-create the context. Operators do this in production — the
	// agent only learns the id, never the create endpoint.
	ctxID := createBEContext(t, beKey, beURL)
	t.Logf("created context %s", ctxID)
	defer deleteBEContext(t, beKey, beURL, ctxID)

	// us is broadly available on residential proxy networks. Picking a
	// less-common country ("ph", "ar", …) is a stricter test of the
	// country routing but flakier when the provider lacks coverage.
	proxyCountry := "us"
	expectedCountry := "US"

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:         "browser-engine",
		APIKey:       beKey,
		URL:          beURL,
		Width:        1280,
		Height:       720,
		ProxyEnabled: true,
		ProxyCountry: proxyCountry,
	})
	if err != nil {
		t.Fatalf("create computer: %v", err)
	}
	defer func() {
		if comp != nil {
			_ = comp.Close()
		}
	}()
	t.Logf("computer ready: type=browser-engine display=%dx%d proxy=%s", comp.DisplaySize().Width, comp.DisplaySize().Height, proxyCountry)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}
	cfg := &Config{
		Directive: "You have a remote browser via Browser Engine. Use browser_session and computer_use. The session is bound to a persistent context (id given to you below) and routed through a residential proxy in a specific country.",
		Mode:      ModeAutonomous,
	}
	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(strings.Join([]string{
		`Do EXACTLY this — no improvising, no waiting:`,
		fmt.Sprintf(`1) browser_session(action=open, url=https://ifconfig.co/country-iso, context_id=%s, persist=true, timeout=300)`, ctxID),
		`2) Reply on a single line with exactly: RESULT: opened`,
		`That's it. DO NOT call pace. DO NOT call computer_use. DO NOT navigate anywhere else. Reply RESULT immediately after step 1 returns.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-ctxproxy", 500)
	logFile, _ := os.Create("computer_test_ctxproxy_chunks.log")
	defer logFile.Close()

	var sawOpenWithCtx bool
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
					if !sawOpenWithCtx &&
						strings.Contains(s, "action=open") &&
						strings.Contains(s, "ifconfig.co") &&
						strings.Contains(s, "context_id="+ctxID) {
						sawOpenWithCtx = true
					}
					if strings.Contains(s, "RESULT: opened") && !closed {
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
		t.Log("agent emitted RESULT — proceeding to Eval")
	case <-time.After(4 * time.Minute):
		t.Log("timeout — evaluating whatever state the comp is in")
	}
	finalURL := currentURL(comp)

	// Read body via Eval BEFORE stopping the thinker. Stop() can race
	// with chromedp's ctx and cancel the connection, which makes Eval
	// fail with "context canceled" even though the page is fine. The
	// thinker is benign while we run one more CDP call.
	type evaluator interface {
		Eval(js string, dst any) error
	}
	var bodyText string
	var evalErr error
	if ev, ok := comp.(evaluator); ok {
		evalErr = ev.Eval(`document.body.innerText`, &bodyText)
		bodyText = strings.TrimSpace(bodyText)
	} else {
		evalErr = fmt.Errorf("comp does not implement Eval")
	}

	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	log, _ := os.ReadFile("computer_test_ctxproxy_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(log))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawOpenWithCtx {
		t.Fatal("FAIL: agent never opened ifconfig.co with context_id — context-tool plumbing not exercised")
	}
	t.Log("✓ agent opened ifconfig.co with context_id (context-binding threaded through tool param)")

	if !strings.Contains(finalURL, "ifconfig.co") {
		t.Fatalf("FAIL: final URL %q, expected ifconfig.co — proxy may have failed to connect", finalURL)
	}
	t.Log("✓ ifconfig.co reachable through proxy")

	// Independent provider-side check: ContextID() reflects the actual
	// binding the provider holds, not what the agent claimed to set.
	if ci, ok := comp.(interface{ ContextID() string }); ok {
		got := ci.ContextID()
		if got != ctxID {
			t.Fatalf("FAIL: comp.ContextID()=%q, want %q (agent's context_id arg didn't make it to the session)", got, ctxID)
		}
		t.Logf("✓ comp.ContextID()=%s matches the operator-supplied context", got)
	} else {
		t.Log("(comp doesn't implement ContextInfo — skipping provider-side context check)")
	}

	// Objective ground truth: the page body served by the proxy.
	if evalErr != nil {
		t.Fatalf("read body via Eval: %v", evalErr)
	}
	t.Logf("=== ifconfig.co/country-iso body: %q (configured proxy_country=%q)", bodyText, proxyCountry)
	if !strings.EqualFold(bodyText, expectedCountry) {
		t.Fatalf("FAIL: ifconfig.co reported country=%q via the proxy, expected %q — residential proxy did not honor the requested country (or wasn't applied)", bodyText, expectedCountry)
	}
	t.Logf("✓ proxy applied: ifconfig.co reported country=%s matching the configured proxy country", bodyText)
}
