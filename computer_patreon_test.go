package core

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

)

// TestComputerUse_PatreonLogin drives a real Patreon login end-to-end.
// Patreon emails a 6-digit verification code after the email-submit
// step; the test pauses when the agent announces "AWAITING_CODE" and
// resumes once you provide the code via HTTP, file, or env var.
//
// Backends (env TEST_BROWSER, default "local"):
//
//	local        → local Chrome (often blocked by Patreon's bot
//	               detection; useful for harness debugging only)
//	browserbase  → cloud Chrome via Browserbase (recommended;
//	               requires BROWSERBASE_API_KEY + BROWSERBASE_PROJECT_ID)
//	steel        → cloud Chrome via Steel (requires STEEL_API_KEY)
//
// Provide the login code mid-test via any of:
//
//	curl -d 123456 $PATREON_CODE_URL      # printed on test stderr
//	echo 123456 > /tmp/patreon_code.txt   # file fallback
//	PATREON_CODE=123456 go test ...       # baked in at start
//
//	RUN_PATREON_TEST=1 PATREON_EMAIL=you@example.com \
//	FIREWORKS_API_KEY=fw_... APTEVA_HEADLESS_BROWSER=1 \
//	TEST_BROWSER=browserbase BROWSERBASE_API_KEY=... BROWSERBASE_PROJECT_ID=... \
//	go test -v -run TestComputerUse_PatreonLogin -timeout 15m ./
func TestComputerUse_PatreonLogin(t *testing.T) {
	if os.Getenv("RUN_PATREON_TEST") == "" {
		t.Skip("set RUN_PATREON_TEST=1 (real Patreon login — interactive, burns tokens)")
	}
	tp := getTestProvider(t)
	apiKey := tp.APIKey
	email := os.Getenv("PATREON_EMAIL")
	if email == "" {
		t.Skip("PATREON_EMAIL not set")
	}
	password := os.Getenv("PATREON_PASSWORD")
	if password == "" {
		t.Skip("PATREON_PASSWORD not set")
	}

	// Patreon gates logins behind Cloudflare + account-origin
	// fingerprinting — datacenter IPs almost always 403. Turn on the
	// managed residential proxy for the cloud backends unless the
	// operator explicitly opts out. No-op on TEST_BROWSER=local.
	if os.Getenv("PATREON_USE_PROXY") != "0" {
		t.Setenv("TEST_BROWSER_PROXY", "1")
	}
	// Patreon's email-code flow can take 4–10 min; the cloud
	// backends' default lease (300s on Browser Engine, plan-default
	// on others) often expires mid-flow. Browserbase + Steel can't
	// extend post-create, so we set a generous lease at creation
	// for every cloud backend. Override with PATREON_SESSION_TIMEOUT.
	if os.Getenv("TEST_BROWSER_SESSION_TIMEOUT") == "" {
		secs := "1200"
		if v := os.Getenv("PATREON_SESSION_TIMEOUT"); v != "" {
			secs = v
		}
		t.Setenv("TEST_BROWSER_SESSION_TIMEOUT", secs)
	}

	comp := buildComputerFromEnv(t)
	defer func() {
		if comp != nil {
			comp.Close()
		}
	}()
	t.Logf("computer connected: %dx%d backend=%s",
		comp.DisplaySize().Width, comp.DisplaySize().Height, backendName(t))

	// Code-intake server: random loopback port. The test prints the URL
	// so the operator can POST the emailed code mid-run. Also watches a
	// fallback file and accepts a pre-baked PATREON_CODE env var.
	codeCh := make(chan string, 1)
	if pre := os.Getenv("PATREON_CODE"); pre != "" {
		codeCh <- strings.TrimSpace(pre)
	}
	srvURL, stopSrv := startCodeServer(t, codeCh)
	defer stopSrv()
	codeFile := os.Getenv("PATREON_CODE_FILE")
	if codeFile == "" {
		codeFile = "/tmp/patreon_code.txt"
	}
	stopWatch := watchCodeFile(t, codeFile, codeCh)
	defer stopWatch()

	provider, err := selectProvider(&Config{})
	if err != nil {
		t.Fatalf("no provider: %v", err)
	}
	cfg := &Config{
		Directive: "You have a browser with Set-of-Mark grounding — interactive elements have colored numeric badges. Prefer label=N for clicks.",
		Mode:      ModeAutonomous,
	}
	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(strings.Join([]string{
		`Log in to Patreon.com with the credentials below. Do not invent a code.`,
		``,
		fmt.Sprintf(`Email:    %s`, email),
		fmt.Sprintf(`Password: %s`, password),
		``,
		`Steps:`,
		`1) browser_session(action=open, url=https://www.patreon.com/login, timeout=1200)`,
		`   The timeout=1200 (20 min) is critical — this flow waits on an emailed code and the default session lease is short. If the session expires mid-flow you cannot recover, so always extend it on open.`,
		`2) If a cookie/consent banner covers the page, dismiss it first (click Accept by label).`,
		`3) computer_use(action=screenshot) — find the email input (orange badge).`,
		`4) Click the email input (label=N), then computer_use(action=type, text="<the email above>").`,
		`5) Click the Continue/Log in button (green badge). The page advances to a password field.`,
		`6) Screenshot. Click the password input (label=N), then computer_use(action=type, text="<the password above>").`,
		`7) Click the Log in/Continue button. Patreon may now email a 6-digit verification code.`,
		`8) If (and only if) you see a code-entry screen, output the EXACT literal text on its own line:  AWAITING_CODE`,
		`   Then call pace(1h) and wait. A console message will arrive with "CODE: 123456". When it does, click the code input (label=N), type the 6 digits, and submit.`,
		`9) If instead you land directly on the logged-in page (home feed / avatar / /home / /c/...), skip the code step.`,
		`10) When you can see you are logged in, reply RESULT: logged_in on its own line.`,
		`Use label= for every click, never coordinate.`,
	}, "\n"))

	obs := thinker.bus.SubscribeAll("test-patreon", 2000)
	logFile, _ := os.Create("computer_test_patreon_chunks.log")
	defer logFile.Close()

	// Agent progress tracking. awaitingAt pins the offset where
	// "AWAITING_CODE" first appears in the post-prompt stream, so we
	// don't trigger on the prompt echoing it back to us.
	var sawAwaiting, codeInjected, loggedIn, cheated bool
	promptLen := 0 // snapshot len after initial prompt; set below
	done := make(chan struct{})
	closed := false
	var buf strings.Builder
	var bufMu sync.Mutex

	// One-shot code injector: drains codeCh, injects "CODE: X" into the
	// console, logs the action. Run in a goroutine so the observer
	// can fire it without blocking the event loop.
	injectCode := func() {
		select {
		case code := <-codeCh:
			code = strings.TrimSpace(code)
			if len(code) == 0 {
				return
			}
			t.Logf("[PATREON_CODE] received code (%d chars) — injecting", len(code))
			thinker.InjectConsole(fmt.Sprintf("CODE: %s\n\nThe verification code from your email is: %s. Click the code input field and type these digits, then submit.", code, code))
			codeInjected = true
		case <-time.After(10 * time.Minute):
			t.Logf("[PATREON_CODE] timed out waiting for code after 10 min")
		}
	}

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
					bufMu.Lock()
					buf.WriteString(ev.Text)
					s := buf.String()
					bufMu.Unlock()

					// AWAITING_CODE: only count occurrences past the end
					// of our own prompt — the prompt itself mentions the
					// token.
					if !sawAwaiting && strings.Contains(s[promptLen:], "AWAITING_CODE") {
						sawAwaiting = true
						t.Logf("[PATREON_CODE] agent emitted AWAITING_CODE — blocking for code")
						t.Logf("[PATREON_CODE] provide code via:")
						t.Logf("[PATREON_CODE]   curl -d 123456 %s/code", srvURL)
						t.Logf("[PATREON_CODE]   echo 123456 > %s", codeFile)
						go injectCode()
					}
					if !cheated && sawAwaiting && codeInjected &&
						strings.Contains(s[promptLen:], "AWAITING_CODE") {
						// Second AWAITING_CODE after we fed one in =
						// wrong code. Surface it.
						post := s[promptLen:]
						if strings.Count(post, "AWAITING_CODE") >= 2 {
							cheated = true // repurpose: "rejected"
						}
					}
					if strings.Contains(s[promptLen:], "RESULT: logged_in") {
						loggedIn = true
					}
				}
				if loggedIn && !closed {
					closed = true
					close(done)
					return
				}
			case <-time.After(15 * time.Minute):
				return
			}
		}
	}()

	// Snapshot prompt length once the initial thought starts streaming
	// so AWAITING_CODE detection skips the prompt echo.
	bufMu.Lock()
	promptLen = buf.Len()
	bufMu.Unlock()

	go thinker.Run()

	select {
	case <-done:
		t.Log("login completed — stopping agent")
	case <-time.After(13 * time.Minute):
		t.Log("timeout — evaluating final state")
	}

	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(500 * time.Millisecond)
	logFile.Sync()

	logContent, _ := os.ReadFile("computer_test_patreon_chunks.log")
	t.Logf("=== Chunks log ===\n%s", string(logContent))
	t.Logf("=== Final URL: %s", finalURL)

	if !sawAwaiting {
		t.Fatal("FAIL: agent never reached the code-entry step (AWAITING_CODE not emitted) — login form may have failed before the email step")
	}
	t.Log("✓ agent reached the code-entry step")
	if !codeInjected {
		t.Fatal("FAIL: no verification code was provided within the window — nothing to verify")
	}
	t.Log("✓ verification code injected")
	if cheated {
		t.Fatal("FAIL: agent emitted AWAITING_CODE again after the code was provided — Patreon rejected the code (wrong/expired) or agent mis-typed")
	}
	if !loggedIn {
		t.Fatalf("FAIL: agent never emitted RESULT: logged_in (final URL %q)", finalURL)
	}
	t.Log("✓ RESULT: logged_in observed — Patreon login flow passed end-to-end")
}

// buildComputerFromEnv + backendName are defined in computer_backend_test.go.

// startCodeServer listens on a random loopback port and pushes POSTed
// bodies into codeCh. Returns the base URL and a close function.
// Accepts both `curl -d 123456 URL/code` (raw body) and form encoding.
func startCodeServer(t *testing.T, codeCh chan<- string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/code", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		code := strings.TrimSpace(string(body))
		// Accept form encoding too: code=123456
		if eq := strings.SplitN(code, "=", 2); len(eq) == 2 && eq[0] == "code" {
			code = eq[1]
		}
		if code == "" {
			http.Error(w, "empty body; POST the code as the raw body", http.StatusBadRequest)
			return
		}
		select {
		case codeCh <- code:
			fmt.Fprintln(w, "ok")
		default:
			http.Error(w, "code channel full — already received one", http.StatusConflict)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	base := "http://" + ln.Addr().String()
	return base, func() {
		srv.Close()
	}
}

// watchCodeFile polls the file for a code, pushes it into codeCh when
// seen, and deletes the file so a second iteration doesn't re-trigger.
// No inotify — 500ms polling is fine for an interactive test.
func watchCodeFile(t *testing.T, path string, codeCh chan<- string) func() {
	t.Helper()
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				b, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				code := strings.TrimSpace(string(bytes.TrimSpace(b)))
				if code == "" {
					continue
				}
				os.Remove(path)
				select {
				case codeCh <- code:
				default:
				}
			}
		}
	}()
	return func() { close(stop) }
}
