package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	aptcomputer "github.com/apteva/computer"
	"github.com/apteva/core/pkg/computer"
)


func TestComputerUse_Local(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("skipping computer_use test (set RUN_COMPUTER_TESTS=1 to enable)")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}
	_ = apiKey

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:   "local",
		// 1600×800 — exact 2:1 widescreen, same default the server
		// picks for non-Anthropic providers (Fireworks/Kimi, OpenAI,
		// Google). Matching production viewport avoids chasing
		// coordinate bugs that don't exist in real runs.
		Width:  1600,
		Height: 800,
	})
	if err != nil {
		t.Fatalf("failed to create local computer: %v", err)
	}
	defer comp.Close()
	t.Logf("local computer connected: %dx%d", comp.DisplaySize().Width, comp.DisplaySize().Height)

	// Test browser_session: open URL (no screenshot — just navigates)
	text, screenshot, err := computer.HandleSessionAction(comp, map[string]string{
		"action": "open",
		"url":    "https://example.com",
	})
	if err != nil {
		t.Fatalf("browser_session open failed: %v", err)
	}
	if screenshot != nil {
		t.Fatal("browser_session open should NOT return a screenshot")
	}
	if !strings.Contains(text, "Navigated") {
		t.Errorf("expected navigated text, got: %s", text)
	}
	t.Logf("browser_session open: %s", text)

	// Test computer_use: take screenshot
	text, screenshot, err = computer.HandleComputerAction(comp, map[string]string{
		"action": "screenshot",
	})
	if err != nil {
		t.Fatalf("computer_use screenshot failed: %v", err)
	}
	if screenshot == nil || len(screenshot) == 0 {
		t.Fatal("computer_use screenshot returned no image")
	}
	t.Logf("computer_use screenshot: %s (%d bytes)", text, len(screenshot))

	// Test computer_use: reject navigate (should use browser_session)
	_, _, err = computer.HandleComputerAction(comp, map[string]string{
		"action": "navigate",
		"url":    "https://example.com",
	})
	if err == nil {
		t.Fatal("expected computer_use to reject navigate action")
	}
	t.Logf("computer_use navigate correctly rejected: %v", err)

	// Test browser_session: status
	text, _, err = computer.HandleSessionAction(comp, map[string]string{
		"action": "status",
	})
	if err != nil {
		t.Fatalf("browser_session status failed: %v", err)
	}
	if !strings.Contains(text, "local") {
		t.Errorf("expected 'local' in status, got: %s", text)
	}
	if !strings.Contains(text, "example.com") {
		t.Errorf("expected 'example.com' in status URL, got: %s", text)
	}
	t.Logf("browser_session status: %s", text)
}

// TestComputerUse_LocalThinkLoop is the local-browser twin of
// TestComputerUse_Navigate: real Fireworks LLM driving a real headed
// Chrome via chromedp, through the full Thinker loop. Local is the
// default surface for npx users, yet until now no test exercised
// LLM ↔ local-chrome end to end — only LLM ↔ Browserbase and
// local-chrome-via-direct-tool-calls in isolation. Mac/Windows
// regressions in 0.9.x slipped through partly because of that gap.
//
// Runs:
//
//	RUN_COMPUTER_TESTS=1 \
//	FIREWORKS_API_KEY=fw_... \
//	go test -v -run TestComputerUse_LocalThinkLoop -timeout 4m ./
//
// On Linux CI without a display server, set APTEVA_HEADLESS_BROWSER=1
// so chromedp launches Chrome in headless mode.
func TestComputerUse_LocalThinkLoop(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("skipping local-browser LLM test (set RUN_COMPUTER_TESTS=1 to enable)")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:   "local",
		// 1600×800 — exact 2:1 widescreen, same default the server
		// picks for non-Anthropic providers (Fireworks/Kimi, OpenAI,
		// Google). Matching production viewport avoids chasing
		// coordinate bugs that don't exist in real runs.
		Width:  1600,
		Height: 800,
	})
	if err != nil {
		t.Fatalf("failed to create local computer: %v", err)
	}
	// Explicit Close at the end to verify the shutdown path too —
	// not via defer, so the test body can assert state post-close
	// without relying on t.Cleanup ordering.
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("local chrome connected: %dx%d", comp.DisplaySize().Width, comp.DisplaySize().Height)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}

	// Kimi K2.5 treats the directive as ambient context and defaults to
	// idle on first thought regardless of "EXECUTE ON STARTUP" framing.
	// An inbox event, by contrast, reliably breaks it out of idle — so
	// we pre-queue the task on the bus BEFORE Run() starts. The first
	// iteration then consumes the event as an active instruction
	// instead of burning a thought on a warmup.
	cfg := &Config{
		Directive: "You have a local browser. Follow user instructions from the console.",
		Mode:      ModeAutonomous,
	}

	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	// Pre-queue the task. Run() below will pick it up on iteration #1.
	thinker.InjectConsole(`Use browser_session(action=open, url=https://example.com) then computer_use(action=screenshot), then reply with exactly "RESULT: " followed by the page title. Do not call pace, do not spawn threads. Stop after RESULT.`)

	obs := thinker.bus.SubscribeAll("test-local", 500)
	logFile, _ := os.Create("computer_test_local_chunks.log")
	defer logFile.Close()

	var sawScreenshot, sawNavigate, sawResult bool
	done := make(chan struct{})
	closed := false

	// Accumulate chunks into a rolling buffer before substring-matching.
	// Chunks arrive token-by-token, so "RESULT:" can split across two
	// events ("RES" + "ULT:"); checking each event in isolation misses it.
	var buf strings.Builder

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
					if strings.Contains(s, "← computer_use") {
						sawScreenshot = true
					}
					if strings.Contains(s, "→ computer_use") || strings.Contains(s, "→ browser_session") {
						sawNavigate = true
					}
					// Match on the real page title, not the "RESULT:" tag.
					// "RESULT:" only proves the LLM produced the template;
					// "Example Domain" requires vision to have actually
					// read the screenshot. The title on example.com has
					// been stable for 25+ years — safe literal.
					if strings.Contains(s, "Example Domain") {
						sawResult = true
					}
				}
				if sawScreenshot && sawNavigate && sawResult && !closed {
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

	// Budget: 90s covers ~3 Kimi thoughts (navigate+screenshot+RESULT).
	// Close the agent the moment we see all three signals — no point
	// waiting out the timer just to evaluate the same log.
	select {
	case <-done:
		t.Log("all three checks passed via stream — stopping agent")
	case <-time.After(90 * time.Second):
		t.Log("timeout waiting for all checks — evaluating from log")
	}

	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	logContent, _ := os.ReadFile("computer_test_local_chunks.log")
	fullText := string(logContent)
	t.Logf("=== Chunks log ===\n%s", fullText)

	if !sawNavigate {
		t.Fatal("FAIL: LLM never called computer_use navigate")
	}
	t.Log("✓ navigate called")

	if !sawScreenshot {
		t.Fatal("FAIL: screenshot never returned — local Chrome may have failed to load the page (check [BROWSER] logs on stderr)")
	}
	t.Log("✓ screenshot returned")

	if sawResult || strings.Contains(fullText, "Example Domain") {
		t.Log("✓ LLM read the page title (\"Example Domain\") from the screenshot — vision round-trip proven end-to-end")
	} else {
		t.Fatal("FAIL: LLM never produced the page title — screenshot delivered but either vision failed or the model hallucinated")
	}

	// Explicit Close to exercise the same shutdown path Thinker.Shutdown
	// uses in prod. If Close panics or hangs, the test surfaces it
	// instead of leaking an orphan Chrome the next run has to fight.
	if err := comp.Close(); err != nil {
		t.Errorf("comp.Close() returned error: %v", err)
	}
	comp = nil // prevent defer double-close
	t.Log("✓ local chrome closed cleanly")
}

// currentURL type-asserts against the SessionInfo interface (both
// local and browserbase implement it) to read the live page URL.
// Returns "" if the implementation doesn't expose one.
func currentURL(c computer.Computer) string {
	if s, ok := c.(interface{ CurrentURL() string }); ok {
		return s.CurrentURL()
	}
	return ""
}

// TestComputerUse_LocalSoMMultistep is a fuller end-to-end on a real
// public site. Exercises every primitive the browser tool exposes in
// one test: navigate → screenshot → click (via SoM label) → type →
// key (Enter) → scroll → click again → verify final URL.
//
// Target: Wikipedia. The flow is deterministic because:
//   - wikipedia.org is stable
//   - the article "Docker (software)" exists and its URL is fixed
//   - Wikipedia auto-redirects exact-match searches to the article
//   - every Docker article historically contains a link to another
//     Wikipedia article (we check the final URL is on wikipedia.org,
//     is not Main_Page, and is not Docker itself — so a second hop
//     happened)
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... \
//	APTEVA_HEADLESS_BROWSER=1 \
//	go test -v -run TestComputerUse_LocalSoMMultistep -timeout 6m ./
func TestComputerUse_LocalSoMMultistep(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}
	t.Setenv("APTEVA_SOM", "1")

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type: "local", Width: 1600, Height: 900,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("local chrome: %dx%d (SoM on)", comp.DisplaySize().Width, comp.DisplaySize().Height)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}
	cfg := &Config{
		Directive: "You have a local browser with Set-of-Mark grounding. Interactive elements have colored numeric badges — always prefer label=N for clicks.",
		Mode:      ModeAutonomous,
	}
	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(strings.Join([]string{
		`Multi-step task. Do EACH step, screenshot once after each to see new badges:`,
		`1) browser_session(action=open, url=https://en.wikipedia.org/wiki/Main_Page)`,
		`2) computer_use(action=screenshot)`,
		`3) Find the search input (orange badge at the top of the page). computer_use(action=click, label=N).`,
		`4) computer_use(action=type, text="Docker (software)")`,
		`5) computer_use(action=key, key="Enter") — submits the search. Wikipedia auto-redirects exact matches.`,
		`6) computer_use(action=screenshot) — you should be on the Docker (software) article now.`,
		`7) computer_use(action=scroll, direction="down", amount=5) to scroll into the article body.`,
		`8) computer_use(action=screenshot) — new badges will appear for visible links.`,
		`9) Pick ANY visible internal Wikipedia link (blue badge) that isn't a reference/footnote — e.g. a link to a related technology or concept. computer_use(action=click, label=N).`,
		`10) browser_session(action=status) to see the final URL.`,
		`When the URL starts with "https://en.wikipedia.org/wiki/" and does NOT contain "Main_Page" and does NOT contain "Docker_(software)", reply RESULT: navigated. Never use coordinate. Never call pace.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-multi", 2000)
	logFile, _ := os.Create("computer_test_multi_chunks.log")
	defer logFile.Close()

	var sawClick, sawType, sawKey, sawScroll bool
	done := make(chan struct{})
	closed := false
	var buf strings.Builder

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
					if strings.Contains(s, "action=click") {
						sawClick = true
					}
					if strings.Contains(s, "action=type") {
						sawType = true
					}
					if strings.Contains(s, "action=key") {
						sawKey = true
					}
					if strings.Contains(s, "action=scroll") {
						sawScroll = true
					}
				}
				// Success: landed on a third article (not Main_Page, not Docker).
				u := currentURL(comp)
				if sawClick && strings.HasPrefix(u, "https://en.wikipedia.org/wiki/") &&
					!strings.Contains(u, "Main_Page") &&
					!strings.Contains(u, "Docker_(software)") &&
					!strings.Contains(u, "Docker_%28software%29") && !closed {
					closed = true
					close(done)
					return
				}
			case <-time.After(5 * time.Minute):
				return
			}
		}
	}()

	go thinker.Run()

	select {
	case <-done:
		t.Log("multi-step flow completed — stopping agent")
	case <-time.After(5 * time.Minute):
		t.Log("timeout — evaluating final state")
	}
	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	log, _ := os.ReadFile("computer_test_multi_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(log))
	t.Logf("=== Final URL: %s", finalURL)
	t.Logf("=== Primitives exercised: click=%v type=%v key=%v scroll=%v",
		sawClick, sawType, sawKey, sawScroll)

	if !sawClick {
		t.Fatal("FAIL: never clicked")
	}
	if !sawType {
		t.Fatal("FAIL: never typed")
	}
	if !strings.HasPrefix(finalURL, "https://en.wikipedia.org/wiki/") {
		t.Fatalf("FAIL: final URL not a Wikipedia article: %q", finalURL)
	}
	if strings.Contains(finalURL, "Main_Page") {
		t.Fatalf("FAIL: never left the main page: %q", finalURL)
	}
	if strings.Contains(finalURL, "Docker_(software)") || strings.Contains(finalURL, "Docker_%28software%29") {
		t.Fatalf("FAIL: stopped on the intermediate Docker article (didn't click an inline link): %q", finalURL)
	}
	t.Logf("✓ landed on a second article via inline link click — multi-step SoM flow proven on Kimi")

	if err := comp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	comp = nil
}

// TestComputerUse_LocalSoMClick exercises the Set-of-Mark pipeline
// end-to-end at 1600×900 with Kimi. With SoM on the agent sees
// numeric badges on each interactive element and we expect it to
// reply with label=1 instead of guessing coordinates. Same example.com
// click-the-link task as LocalClick; the difference is the grounding.
//
// Activated via APTEVA_SOM=1 set inside the test so a one-off run
// doesn't affect neighboring tests.
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... \
//	APTEVA_HEADLESS_BROWSER=1 \
//	go test -v -run TestComputerUse_LocalSoMClick -timeout 4m ./
func TestComputerUse_LocalSoMClick(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}
	// Activate SoM for this test only; defer clears so later tests
	// keep their existing coordinate-based behavior.
	t.Setenv("APTEVA_SOM", "1")

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
	t.Logf("local chrome connected: %dx%d (SoM on)", comp.DisplaySize().Width, comp.DisplaySize().Height)

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
		`1) browser_session(action=open, url=https://example.com)`,
		`2) computer_use(action=screenshot) — you will see numeric badges on interactive elements. Find the link badge.`,
		`3) computer_use(action=click, label=<that number>) — ONE click, use label, not coordinate.`,
		`4) When URL contains "iana", reply RESULT: clicked.`,
		`Do not call pace. Do not use browser_session(open) to navigate anywhere else. Exactly one click attempt.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-som", 500)
	logFile, _ := os.Create("computer_test_som_chunks.log")
	defer logFile.Close()

	var sawClick, sawLabel, cheated bool
	done := make(chan struct{})
	closed := false
	var buf strings.Builder
	clickAt := -1 // buffer offset where the first click chunk appeared

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
					// Cheat check: only look for action=open+iana in
					// text that was emitted AFTER the first click.
					// The full buffer contains the user prompt (which
					// mentions "iana") plus the first navigate call
					// (action=open, url=example.com) — we don't want
					// either to count as a cheat.
					if sawClick && clickAt >= 0 {
						post := s[clickAt:]
						if strings.Contains(post, "action=open") &&
							strings.Contains(post, "iana") {
							cheated = true
						}
					}
				}
				if sawClick && strings.Contains(currentURL(comp), "iana") && !closed {
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
		t.Log("click caused navigation — stopping agent")
	case <-time.After(3 * time.Minute):
		t.Log("timeout — evaluating log")
	}
	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	log, _ := os.ReadFile("computer_test_som_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(log))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawClick {
		t.Fatal("FAIL: agent never emitted a click action")
	}
	t.Log("✓ click action called")
	if !sawLabel {
		t.Log("WARN: agent did not use label= (fell back to coordinate)")
	} else {
		t.Log("✓ agent used label= (SoM grounding)")
	}
	if cheated {
		t.Fatal("FAIL: agent gave up on click and navigated directly via browser_session(open)")
	}
	if !strings.Contains(finalURL, "iana") {
		t.Fatalf("FAIL: final URL is %q, expected to contain 'iana'", finalURL)
	}
	t.Log("✓ click navigated to iana — SoM click proven at 1600×900 on Kimi")

	if err := comp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	comp = nil
}

// TestComputerUse_LocalClick is the middle-tier test between the
// navigate-and-read (LocalThinkLoop) and the full form-fill
// (LocalLoginFlow). It proves the click action actually routes
// mouse-down events to the right DOM element and triggers the
// click's side-effect (navigation, in this case).
//
// Target: example.com has a single link "More information…" pointing
// at iana.org/domains/example. The agent has to:
//   1) navigate
//   2) screenshot
//   3) click the link using coordinates read from the screenshot
//   4) verify URL changed
//
// Proof is URL-based — we don't need the LLM to describe the landing
// page, just to successfully cause a navigation by clicking.
//
// Runs:
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... \
//	APTEVA_HEADLESS_BROWSER=1 \
//	go test -v -run TestComputerUse_LocalClick -timeout 5m ./
func TestComputerUse_LocalClick(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("skipping local-click test (set RUN_COMPUTER_TESTS=1 to enable)")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:   "local",
		Width:  1600,
		Height: 800,
	})
	if err != nil {
		t.Fatalf("failed to create local computer: %v", err)
	}
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("local chrome connected: %dx%d", comp.DisplaySize().Width, comp.DisplaySize().Height)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}

	cfg := &Config{
		Directive: "You have a local browser. Follow user instructions from the console.",
		Mode:      ModeAutonomous,
	}

	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(strings.Join([]string{
		`Do the following and stop:`,
		`1) browser_session(action=open, url=https://example.com)`,
		`2) computer_use(action=screenshot) to see the page.`,
		`3) The page has a single link labeled "More information..." near the bottom of the text. Click it using computer_use(action=click, coordinate="x,y") — estimate the coordinates from the screenshot.`,
		`4) Once you see a new page load (the URL will contain "iana"), reply: RESULT: clicked.`,
		`Do not call pace, do not spawn threads.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-click", 500)
	logFile, _ := os.Create("computer_test_click_chunks.log")
	defer logFile.Close()

	var sawClick, cheated bool
	done := make(chan struct{})
	closed := false
	var buf strings.Builder

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
					if strings.Contains(s, "action=click") {
						sawClick = true
					}
					// Cheat detection: after at least one click attempt,
					// any direct browser_session(action=open, url=...)
					// that targets an iana URL means the agent gave up
					// and navigated directly instead of making a click
					// actually work. A silent URL check would report
					// success; we surface the cheat so the test fails
					// loudly when click-from-vision is broken.
					if sawClick && strings.Contains(s, "action=open") &&
						(strings.Contains(s, "iana.org") || strings.Contains(s, "url=https://iana") || strings.Contains(s, "url=https://www.iana")) {
						cheated = true
					}
				}
				// Success: a click caused a navigation off example.com
				// to an iana.org page WITHOUT the agent cheating.
				if sawClick && !cheated && strings.Contains(currentURL(comp), "iana") && !closed {
					closed = true
					close(done)
					return
				}
				// If the agent cheated, stop early with a clear fail
				// signal — no point watching it burn LLM tokens on a
				// run that will fail the final assertion anyway.
				if cheated && !closed {
					closed = true
					close(done)
					return
				}
			case <-time.After(4 * time.Minute):
				return
			}
		}
	}()

	go thinker.Run()

	select {
	case <-done:
		t.Log("click caused navigation — stopping agent")
	case <-time.After(3 * time.Minute):
		t.Log("timeout — evaluating final state")
	}

	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	logContent, _ := os.ReadFile("computer_test_click_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(logContent))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawClick {
		t.Fatal("FAIL: agent never emitted a click action")
	}
	t.Log("✓ click action called")

	if cheated {
		t.Fatal("FAIL: agent gave up on click and navigated to iana via browser_session(open) instead — click-from-vision is not actually working")
	}

	if !strings.Contains(finalURL, "iana") {
		t.Fatalf("FAIL: final URL is %q, expected to contain 'iana' (click didn't navigate)", finalURL)
	}
	t.Log("✓ click navigated to iana without cheating — click action proven end-to-end")

	if err := comp.Close(); err != nil {
		t.Errorf("comp.Close() returned error: %v", err)
	}
	comp = nil
}

// TestComputerUse_BrowserbaseLoginFlow mirrors LocalLoginFlow but
// routes through Browserbase instead of local Chrome. Exercises the
// exact same SoM pipeline (enumerate → badge → click-by-label →
// insertText) over a hosted CDP connection to verify the computer
// package's Browserbase variant has feature parity with local.
//
// Distinct from the other Browserbase test (TestComputerUse_Navigate)
// which only does open + screenshot; this one does the full
// click/type/submit form flow on a real public login page.
//
// Additional requirements beyond the local tests:
//   - BROWSERBASE_API_KEY
//   - BROWSERBASE_PROJECT_ID
//
// Note: each run creates a real Browserbase session and gets
// REQUEST_RELEASE'd on Close (not leaked). Costs one session's worth
// of minutes against your Browserbase plan.
//
//	RUN_COMPUTER_TESTS=1 \
//	FIREWORKS_API_KEY=fw_... \
//	BROWSERBASE_API_KEY=bb_... \
//	BROWSERBASE_PROJECT_ID=... \
//	go test -v -run TestComputerUse_BrowserbaseLoginFlow -timeout 5m ./
func TestComputerUse_BrowserbaseLoginFlow(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}
	bbKey := os.Getenv("BROWSERBASE_API_KEY")
	bbProject := os.Getenv("BROWSERBASE_PROJECT_ID")
	if bbKey == "" || bbProject == "" {
		t.Skip("BROWSERBASE_API_KEY / BROWSERBASE_PROJECT_ID not set")
	}
	// SoM on — this is the whole point of the test.
	t.Setenv("APTEVA_SOM", "1")

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:      "browserbase",
		APIKey:    bbKey,
		ProjectID: bbProject,
		// 1600×900 — 2:1-ish widescreen, same as the local login test
		// so results are directly comparable between the two backends.
		Width:  1600,
		Height: 900,
	})
	if err != nil {
		t.Fatalf("failed to create browserbase computer: %v", err)
	}
	defer func() {
		if comp != nil {
			comp.Close() // REQUEST_RELEASEs the session
		}
	}()
	t.Logf("browserbase session connected: %dx%d (SoM on)", comp.DisplaySize().Width, comp.DisplaySize().Height)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}

	cfg := &Config{
		Directive: "You have a browser with Set-of-Mark grounding. Interactive elements have colored numeric badges — prefer label=N for clicks.",
		Mode:      ModeAutonomous,
	}

	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	// Same directive as LocalLoginFlow — keeps the cross-backend
	// comparison honest (behavioural differences are the Computer
	// implementation, not the prompting).
	thinker.InjectConsole(strings.Join([]string{
		`Complete this login flow:`,
		`1) browser_session(action=open, url=https://practicetestautomation.com/practice-test-login/)`,
		`2) computer_use(action=screenshot) — the page shows the valid username and password values as plain text. Read them from the image. Interactive elements have colored numeric badges (orange=input, green=button).`,
		`3) computer_use(action=click, label=N) on the Username input (the orange badge). Then computer_use(action=type, text="<the username you read>").`,
		`4) Same pattern for Password.`,
		`5) computer_use(action=click, label=N) on the Submit button (green badge).`,
		`6) browser_session(action=status) to check the URL.`,
		`When the URL contains "logged-in-successfully", reply: RESULT: logged in. Do not call pace. Use label= for every click — never coordinate.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-bb-login", 1000)
	logFile, _ := os.Create("computer_test_bb_login_chunks.log")
	defer logFile.Close()

	var sawType, sawClick bool
	done := make(chan struct{})
	closed := false
	var buf strings.Builder

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
					if strings.Contains(s, "action=type") {
						sawType = true
					}
					if strings.Contains(s, "action=click") {
						sawClick = true
					}
				}
				if sawType && sawClick && strings.Contains(currentURL(comp), "logged-in-successfully") && !closed {
					closed = true
					close(done)
					return
				}
			case <-time.After(4 * time.Minute):
				return
			}
		}
	}()

	go thinker.Run()

	select {
	case <-done:
		t.Log("login flow completed — stopping agent")
	case <-time.After(4 * time.Minute):
		t.Log("timeout — evaluating final state")
	}
	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	log, _ := os.ReadFile("computer_test_bb_login_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(log))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawType {
		t.Error("FAIL: agent never typed")
	} else {
		t.Log("✓ type action called")
	}
	if !sawClick {
		t.Error("FAIL: agent never clicked")
	} else {
		t.Log("✓ click action called")
	}
	if !strings.Contains(finalURL, "logged-in-successfully") {
		t.Fatalf("FAIL: final URL is %q", finalURL)
	}
	t.Log("✓ Browserbase + Kimi + SoM login flow proven end-to-end")

	if err := comp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	comp = nil
}

// TestComputerUse_LocalLoginFlow is a harder end-to-end: the agent
// has to navigate to a login form, find credentials displayed on the
// page (possibly scrolling to see them), type them into the form,
// click submit, and land on the success URL. Exercises click/type/
// scroll on real local Chrome plus the full LLM vision→plan loop.
//
// Verification is URL-based: after the flow completes we check the
// Computer's CurrentURL() directly — no need to trust a verbal
// "I logged in successfully" from the model.
//
// Runs:
//
//	RUN_COMPUTER_TESTS=1 FIREWORKS_API_KEY=fw_... \
//	APTEVA_HEADLESS_BROWSER=1 \
//	go test -v -run TestComputerUse_LocalLoginFlow -timeout 8m ./
func TestComputerUse_LocalLoginFlow(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("skipping local-login test (set RUN_COMPUTER_TESTS=1 to enable)")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}

	// SoM on — this is the whole point of the test. Kimi sees numeric
	// badges on the username/password inputs and the Submit button;
	// clicks resolve via label→bbox map, coordinates never leave our
	// code. t.Setenv unsets when the test ends so neighbouring tests
	// keep their non-SoM behaviour.
	t.Setenv("APTEVA_SOM", "1")

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type: "local",
		// 1600×900 — 2:1-ish widescreen. Fits the login form above
		// the fold so the agent doesn't need to scroll; page still
		// renders identically to real Kimi users on Fireworks.
		Width:  1600,
		Height: 900,
	})
	if err != nil {
		t.Fatalf("failed to create local computer: %v", err)
	}
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("local chrome connected: %dx%d (SoM on)", comp.DisplaySize().Width, comp.DisplaySize().Height)

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}

	cfg := &Config{
		Directive: "You have a local browser with Set-of-Mark grounding. Interactive elements have colored numeric badges — prefer label=N for clicks.",
		Mode:      ModeAutonomous,
	}

	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	// Instructions. Key changes from the pre-SoM version: clicks use
	// label=N (read the number off the orange/green badge), not
	// coordinate. The agent just has to read badges and pair them
	// with nearby text — no pixel guessing.
	thinker.InjectConsole(strings.Join([]string{
		`Complete this login flow:`,
		`1) browser_session(action=open, url=https://practicetestautomation.com/practice-test-login/)`,
		`2) computer_use(action=screenshot) — the page shows the valid username and password values as plain text. Read them from the image. Interactive elements have colored numeric badges (orange=input, green=button).`,
		`3) computer_use(action=click, label=N) on the Username input (the orange badge). Then computer_use(action=type, text="<the username you read>").`,
		`4) Same pattern for Password.`,
		`5) computer_use(action=click, label=N) on the Submit button (green badge).`,
		`6) browser_session(action=status) to check the URL.`,
		`When the URL contains "logged-in-successfully", reply: RESULT: logged in. Do not call pace. Use label= for every click — never coordinate.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-login", 1000)
	logFile, _ := os.Create("computer_test_login_chunks.log")
	defer logFile.Close()

	var sawType, sawClick bool
	done := make(chan struct{})
	closed := false
	var buf strings.Builder

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
					if strings.Contains(s, "action=type") {
						sawType = true
					}
					if strings.Contains(s, "action=click") {
						sawClick = true
					}
				}
				// Success signal: the agent has to actually reach the
				// /logged-in-successfully/ URL for us to consider this
				// a win. Checked live via the Computer's URL — cheaper
				// than parsing the screenshot text, and authoritative.
				if sawType && sawClick && strings.Contains(currentURL(comp), "logged-in-successfully") && !closed {
					closed = true
					close(done)
					return
				}
			case <-time.After(6 * time.Minute):
				return
			}
		}
	}()

	go thinker.Run()

	// Login flows are multi-step; budget more generously than the
	// single-nav test. Each Kimi thought is 20–40s on this prompt size,
	// and the flow needs ~6 thoughts minimum (nav, screenshot, click,
	// type, click, type, click, screenshot, verify).
	select {
	case <-done:
		t.Log("login flow completed — stopping agent")
	case <-time.After(5 * time.Minute):
		t.Log("timeout — evaluating final state from log + URL")
	}

	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(300 * time.Millisecond)
	logFile.Sync()

	logContent, _ := os.ReadFile("computer_test_login_chunks.log")
	fullText := string(logContent)
	t.Logf("=== Chunks log ===\n%s", fullText)
	t.Logf("=== Final URL: %s", finalURL)

	if !sawType {
		t.Error("FAIL: agent never called type — didn't read credentials or didn't interact")
	} else {
		t.Log("✓ type action called")
	}

	if !sawClick {
		t.Error("FAIL: agent never called click — form not submitted")
	} else {
		t.Log("✓ click action called")
	}

	if !strings.Contains(finalURL, "logged-in-successfully") {
		t.Fatalf("FAIL: final URL is %q, expected to contain 'logged-in-successfully'", finalURL)
	}
	t.Log("✓ landed on /logged-in-successfully/ — login flow proven end-to-end")

	if err := comp.Close(); err != nil {
		t.Errorf("comp.Close() returned error: %v", err)
	}
	comp = nil
}

func TestComputerUse_Navigate(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("skipping computer_use test (set RUN_COMPUTER_TESTS=1 to enable)")
	}
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		t.Skip("FIREWORKS_API_KEY not set")
	}
	bbKey := os.Getenv("BROWSERBASE_API_KEY")
	bbProject := os.Getenv("BROWSERBASE_PROJECT_ID")
	if bbKey == "" || bbProject == "" {
		t.Skip("BROWSERBASE_API_KEY or BROWSERBASE_PROJECT_ID not set")
	}

	// Create computer
	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:      "browserbase",
		APIKey:    bbKey,
		ProjectID: bbProject,
		// 1600×800 — 2:1 widescreen default for non-Anthropic LLMs.
		Width:     1600,
		Height:    800,
	})
	if err != nil {
		t.Fatalf("failed to create computer: %v", err)
	}
	defer comp.Close()
	t.Logf("computer connected: %dx%d", comp.DisplaySize().Width, comp.DisplaySize().Height)

	// Create thinker with computer
	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}

	cfg := &Config{
		Directive: "You have a browser. When told to navigate somewhere, use the computer_use tool to navigate and then take a screenshot. Describe what you see.",
		Mode:      ModeAutonomous,
	}

	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	// Observer
	obs := thinker.bus.SubscribeAll("test", 500)
	// Log all chunks to a file for debugging
	logFile, _ := os.Create("computer_test_chunks.log")
	defer logFile.Close()

	var sawScreenshot bool
	var sawNavigate bool
	var sawResult bool
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
					if strings.Contains(ev.Text, "← computer_use") {
						sawScreenshot = true
					}
					if strings.Contains(ev.Text, "→ computer_use") {
						sawNavigate = true
					}
					if strings.Contains(ev.Text, "RESULT:") {
						sawResult = true
					}
				}
				if sawScreenshot && sawNavigate && sawResult && !closed {
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

	// Tell it to navigate and describe
	time.Sleep(2 * time.Second) // let first idle thought pass
	thinker.InjectConsole("Navigate to https://example.com using computer_use. After you see the screenshot, respond with exactly: RESULT: followed by the page title you see.")

	select {
	case <-done:
		t.Log("all three checks passed via stream")
	case <-time.After(120 * time.Second):
		t.Log("timeout waiting for all checks — evaluating from log")
	}

	thinker.Stop()
	time.Sleep(500 * time.Millisecond)
	logFile.Sync()

	// Read full log
	logContent, _ := os.ReadFile("computer_test_chunks.log")
	fullText := string(logContent)
	t.Logf("=== Chunks log ===\n%s", fullText)

	if !sawNavigate {
		t.Fatal("FAIL: LLM never called computer_use navigate")
	}
	t.Log("✓ navigate called")

	if !sawScreenshot {
		t.Fatal("FAIL: screenshot never returned")
	}
	t.Log("✓ screenshot returned")

	if sawResult {
		t.Log("✓ LLM responded with RESULT:")
	} else if strings.Contains(fullText, "RESULT:") {
		t.Log("✓ LLM responded with RESULT: (found in log)")
	} else {
		t.Log("WARN: no RESULT: found — checking log for any page description")
		lower := strings.ToLower(fullText)
		if strings.Contains(lower, "example") || strings.Contains(lower, "screenshot") {
			t.Log("✓ LLM did process the screenshot (mentioned example/screenshot)")
		} else {
			t.Error("FAIL: LLM never described the page")
		}
	}
}
