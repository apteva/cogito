package main

import (
	"os"
	"testing"
)

// TestMain sets APTEVA_SOM=1 as the package-wide default before any
/// test runs. Rationale: every non-Anthropic model we drive in tests
// (Kimi via Fireworks, Google, etc.) cannot reliably produce pixel
// coordinates from a raw screenshot — empirically verified on
// example.com where Kimi stalls at (225, ~190) trying to hit a link
// whose true center is 60+px away. Set-of-Mark (label-badged)
// screenshots are the path we ship, so tests should default to the
// same path unless they explicitly probe raw-pixel behavior (e.g.
// the matrix test, which opts back out).
//
// Individual tests can still override via t.Setenv("APTEVA_SOM", "")
// to restore raw-pixel mode.
func TestMain(m *testing.M) {
	if os.Getenv("APTEVA_SOM") == "" {
		os.Setenv("APTEVA_SOM", "1")
	}
	os.Exit(m.Run())
}
