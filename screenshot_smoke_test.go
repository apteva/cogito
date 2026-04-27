package core

import (
	"os"
	"strings"
	"testing"

	aptcomputer "github.com/apteva/computer"
	"github.com/apteva/core/pkg/computer"
)

// TestComputerUse_LocalScreenshotToFile is a one-shot smoke test:
// open https://example.com in a local headless Chrome, take a
// screenshot, and write the bytes to /tmp/apteva-screenshot.png so
// the operator can `open` the file and confirm visually.
//
// Gate: RUN_COMPUTER_TESTS=1. No LLM involved; this is purely the
// browser+screenshot wiring smoke. For the LLM-driven version see
// TestComputerUse_LocalThinkLoop.
func TestComputerUse_LocalScreenshotToFile(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1 to run the screenshot smoke")
	}

	url := os.Getenv("SCREENSHOT_URL")
	if url == "" {
		url = "https://example.com"
	}
	out := os.Getenv("SCREENSHOT_OUT")
	if out == "" {
		out = "/tmp/apteva-screenshot.png"
	}

	comp, err := aptcomputer.New(aptcomputer.Config{Type: "local", Width: 1600, Height: 900})
	if err != nil {
		t.Fatalf("create local: %v", err)
	}
	defer comp.Close()

	// Navigate.
	text, _, err := computer.HandleSessionAction(comp, map[string]string{
		"action": "open",
		"url":    url,
	})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Logf("navigated: %s", text)
	if !strings.Contains(text, "Navigated") {
		t.Fatalf("expected 'Navigated' in response, got %q", text)
	}

	// Screenshot.
	stext, png, err := computer.HandleComputerAction(comp, map[string]string{"action": "screenshot"})
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if len(png) == 0 {
		t.Fatal("screenshot returned 0 bytes")
	}
	t.Logf("screenshot: %s (%d bytes)", stext, len(png))

	if err := os.WriteFile(out, png, 0644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	t.Logf("✓ screenshot saved to %s — open it to view", out)
}
