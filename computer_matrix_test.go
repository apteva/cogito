package core

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aptcomputer "github.com/apteva/computer"
	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// TestComputerUse_LocalClickMatrix iterates through (width, height,
// quality) configurations to empirically find which screenshot
// settings let Kimi click a visible link on a real page. Each
// config runs a fresh Chrome, fresh thinker, fresh directive — so
// differences are isolated to viewport geometry + JPEG quality.
//
// Each subtest:
//   1. Launches Chrome at (width, height)
//   2. Probes the real bounding box of the "More information..." link
//      via JS (ground truth)
//   3. Runs the agent; saves every screenshot it takes to
//      /tmp/probe_WxH_qN_<iter>.jpg for visual inspection
//   4. Times out fast (90s) so bad configs don't drag the matrix
//   5. Records: did click land? what coords did Kimi pick? distance
//      from ground-truth center?
//
// Tweaking the matrix: edit the `configs` slice below. Each entry is
// one subtest. You can focus a single config with:
//
//   go test -v -run 'TestComputerUse_LocalClickMatrix/1600x800_q60'
//
// Running:
//
//   RUN_COMPUTER_TESTS=1 RUN_MATRIX=1 FIREWORKS_API_KEY=fw_... \
//   APTEVA_HEADLESS_BROWSER=1 \
//   go test -v -run TestComputerUse_LocalClickMatrix -timeout 30m ./
func TestComputerUse_LocalClickMatrix(t *testing.T) {
	if os.Getenv("RUN_COMPUTER_TESTS") == "" {
		t.Skip("set RUN_COMPUTER_TESTS=1")
	}
	if os.Getenv("RUN_MATRIX") == "" {
		t.Skip("set RUN_MATRIX=1 (this test runs many variants and is slow)")
	}
	tp := getTestProvider(t)
	apiKey := tp.APIKey
	// This matrix is the raw-pixel-click diagnostic — it intentionally
	// opts out of the package's SoM default so each config measures
	// Kimi's coordinate accuracy on unlabeled screenshots.
	t.Setenv("APTEVA_SOM", "")

	// THE MATRIX — edit freely. Order: (width, height, jpeg_quality).
	// Aspect ratios and resolutions to exercise.
	configs := []struct {
		W, H, Q int
	}{
		{1600, 800, 60}, // 2:1 production default, q60
		{1600, 800, 90}, // 2:1 production default, higher quality
		{1280, 800, 60}, // 16:10 laptop, q60
		{1024, 768, 60}, // 4:3 Anthropic default, q60
		{1024, 768, 90}, // 4:3 Anthropic default, high q
		{1024, 512, 60}, // 2:1 smaller
		{800, 600, 60},  // 4:3 small
		{1920, 1080, 60}, // 16:9 full HD
	}

	// Results table accumulator.
	type row struct {
		Config   string
		Pass     bool
		Clicked  bool
		Coord    string
		TrueBox  string
		DistPx   int
		Thoughts int
		SecElap  int
		Note     string
	}
	var rows []row

	for _, c := range configs {
		name := fmt.Sprintf("%dx%d_q%d", c.W, c.H, c.Q)
		t.Run(name, func(t *testing.T) {
			r := runClickProbe(t, apiKey, c.W, c.H, c.Q, name)
			rows = append(rows, r)
		})
	}

	// Print summary table — every variant in one place.
	fmt.Fprintln(os.Stderr, "\n=== CLICK MATRIX SUMMARY ===")
	fmt.Fprintf(os.Stderr, "%-16s %-6s %-8s %-10s %-12s %-7s %-8s %s\n",
		"CONFIG", "PASS", "CLICKED", "COORD", "TRUE_BOX", "DIST_PX", "THOUGHTS", "NOTE")
	for _, r := range rows {
		pass := "FAIL"
		if r.Pass {
			pass = "PASS"
		}
		fmt.Fprintf(os.Stderr, "%-16s %-6s %-8v %-10s %-12s %-7d %-8d %s\n",
			r.Config, pass, r.Clicked, r.Coord, r.TrueBox, r.DistPx, r.Thoughts, r.Note)
	}
}

func runClickProbe(t *testing.T, apiKey string, width, height, quality int, label string) (r struct {
	Config   string
	Pass     bool
	Clicked  bool
	Coord    string
	TrueBox  string
	DistPx   int
	Thoughts int
	SecElap  int
	Note     string
}) {
	r.Config = label
	started := time.Now()

	// Drive quality through the env var our Screenshot() reads.
	os.Setenv("APTEVA_SCREENSHOT_QUALITY", fmt.Sprintf("%d", quality))

	comp, err := aptcomputer.New(aptcomputer.Config{
		Type:   "local",
		Width:  width,
		Height: height,
	})
	if err != nil {
		r.Note = "comp create failed: " + err.Error()
		return
	}
	defer comp.Close()

	// Navigate so we can probe link position before handing control
	// to the agent.
	if _, err := comp.Execute(computer.Action{Type: "navigate", URL: "https://example.com"}); err != nil {
		r.Note = "nav failed: " + err.Error()
		return
	}

	// Ground-truth link position at THIS viewport. Uses the chromedp
	// context from the computer via a small test-only helper.
	linkX, linkY, linkW, linkH, err := probeLinkBox(comp)
	if err != nil {
		r.Note = "probe link: " + err.Error()
		return
	}
	cx, cy := linkX+linkW/2, linkY+linkH/2
	r.TrueBox = fmt.Sprintf("%d,%d/%dx%d", linkX, linkY, linkW, linkH)

	provider, err := selectProvider(&Config{})
	if err != nil {
		r.Note = "provider: " + err.Error()
		return
	}
	cfg := &Config{
		Directive: "You have a local browser. Follow user instructions.",
		Mode:      ModeAutonomous,
	}
	thinker := NewThinker(apiKey, provider, cfg)
	thinker.SetComputer(comp)

	thinker.InjectConsole(fmt.Sprintf(strings.Join([]string{
		`Do this and stop:`,
		`1) computer_use(action=screenshot) to see the page (already at example.com, viewport %dx%d).`,
		`2) Find the "More information..." link. Estimate its pixel coordinates from the screenshot.`,
		`3) computer_use(action=click, coordinate="x,y") exactly once at those coordinates.`,
		`4) If URL contains "iana" after that click, reply RESULT: clicked.`,
		`ONE click attempt only. Do not retry, do not use browser_session(open) to navigate. Do not call pace.`,
	}, "\n"), width, height))

	obs := thinker.bus.SubscribeAll("probe-"+label, 500)
	var buf strings.Builder
	var thoughts atomic.Int64
	var sawClick, cheated bool
	var clickCoord string
	done := make(chan struct{})
	closed := false

	// Save each screenshot to disk for visual inspection. We intercept
	// via a tick on the chunks channel — when we see `← computer_use: screenshot`
	// the returned bytes have already been attached to the LLM context,
	// but we can't extract them from here. So instead we tap the
	// Computer directly before handing to the agent: we already took
	// one above. For subsequent screenshots the agent takes, we'd need
	// a hook. For now, save the initial one — that's what Kimi actually
	// sees on its click decision anyway.
	if buf2, err := comp.Execute(computer.Action{Type: "screenshot"}); err == nil {
		path := fmt.Sprintf("/tmp/probe_%s.jpg", label)
		os.WriteFile(path, buf2, 0644)
		if img, _, derr := image.Decode(bytes.NewReader(buf2)); derr == nil {
			b := img.Bounds()
			fmt.Fprintf(os.Stderr, "[matrix] %s: screenshot %dx%d, %d bytes → %s\n",
				label, b.Dx(), b.Dy(), len(buf2), path)
		}
	}

	go func() {
		for {
			select {
			case ev := <-obs.C:
				if ev.Type == EventThinkDone {
					thoughts.Add(1)
				}
				if ev.Type == EventChunk {
					buf.WriteString(ev.Text)
					s := buf.String()
					if strings.Contains(s, "action=click") && !sawClick {
						sawClick = true
						// Pull the first coordinate= value. Accept
						// quoted ("x,y") or unquoted (x,y) forms.
						if i := strings.Index(s, "coordinate="); i >= 0 {
							rest := s[i+len("coordinate="):]
							if strings.HasPrefix(rest, `"`) {
								rest = rest[1:]
								if end := strings.IndexByte(rest, '"'); end >= 0 {
									clickCoord = rest[:end]
								}
							} else {
								// Unquoted — stop at whitespace or close-paren.
								if end := strings.IndexAny(rest, " )\n\t"); end >= 0 {
									clickCoord = rest[:end]
								}
							}
							clickCoord = strings.TrimRight(clickCoord, ",")
						}
					}
					if sawClick && strings.Contains(s, "action=open") &&
						(strings.Contains(s, "iana") || strings.Contains(s, "url=https://iana")) {
						cheated = true
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
			case <-time.After(90 * time.Second):
				return
			}
		}
	}()

	go thinker.Run()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
	}
	finalURL := currentURL(comp)
	thinker.Stop()
	time.Sleep(200 * time.Millisecond)

	r.Clicked = sawClick
	r.Coord = clickCoord
	r.Thoughts = int(thoughts.Load())
	r.SecElap = int(time.Since(started).Seconds())

	if r.Coord != "" {
		var cxPicked, cyPicked int
		if _, err := fmt.Sscanf(r.Coord, "%d,%d", &cxPicked, &cyPicked); err == nil {
			r.DistPx = manhattan(cxPicked, cyPicked, cx, cy)
		}
	}

	switch {
	case cheated:
		r.Note = "cheated via open()"
	case !sawClick:
		r.Note = "no click emitted"
	case !strings.Contains(finalURL, "iana"):
		r.Note = fmt.Sprintf("click missed (final %s)", finalURL)
	default:
		r.Pass = true
		r.Note = "ok"
	}
	t.Logf("%s → pass=%v clicked=%v coord=%s true=(%d,%d) distPx=%d thoughts=%d %ds (%s)",
		label, r.Pass, r.Clicked, r.Coord, cx, cy, r.DistPx, r.Thoughts, r.SecElap, r.Note)
	return
}

func probeLinkBox(c computer.Computer) (x, y, w, h int, err error) {
	type ctxholder interface {
		Context() context.Context
	}
	// Access chromedp ctx via the underlying local.Computer. We
	// reach in through a type assertion on an unexported method —
	// cheapest way to run ad-hoc JS without adding a public API.
	type jsRunner interface {
		RunJS(js string, out any) error
	}
	if r, ok := c.(jsRunner); ok {
		var rect struct{ X, Y, W, H float64 }
		err = r.RunJS(`(function(){var a=document.querySelector('a');var r=a.getBoundingClientRect();return {X:r.x,Y:r.y,W:r.width,H:r.height};})()`, &rect)
		if err != nil {
			return
		}
		return int(rect.X), int(rect.Y), int(rect.W), int(rect.H), nil
	}
	// Fallback: spin up a throwaway chromedp context — but that loses
	// the real session state (cookies, current URL), so we need the
	// Computer to expose its chromedp.Context. For now, use
	// aptcomputer's Config-based session isn't possible in-process;
	// best effort: re-navigate in a side context, probe there.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
	)
	ac, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelA()
	ctx, cancel := chromedp.NewContext(ac)
	defer cancel()
	d := c.DisplaySize()
	if err = chromedp.Run(ctx); err != nil {
		return
	}
	if err = chromedp.Run(ctx, emulation.SetDeviceMetricsOverride(int64(d.Width), int64(d.Height), 1, false)); err != nil {
		return
	}
	if err = chromedp.Run(ctx, chromedp.Navigate("https://example.com")); err != nil {
		return
	}
	var rect struct{ X, Y, W, H float64 }
	if err = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var a=document.querySelector('a');var r=a.getBoundingClientRect();return {X:r.x,Y:r.y,W:r.width,H:r.height};})()`, &rect)); err != nil {
		return
	}
	return int(rect.X), int(rect.Y), int(rect.W), int(rect.H), nil
}

func manhattan(x1, y1, x2, y2 int) int {
	d := 0
	if x1 > x2 {
		d += x1 - x2
	} else {
		d += x2 - x1
	}
	if y1 > y2 {
		d += y1 - y2
	} else {
		d += y2 - y1
	}
	return d
}
