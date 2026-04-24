package main

import (
	"os"
	"strings"
	"testing"

	aptcomputer "github.com/apteva/computer"
	"github.com/apteva/core/pkg/computer"
)

// buildComputerFromEnv picks a Computer backend from TEST_BROWSER:
//
//	local | browserbase | steel | browser-engine   (default: local)
//
// Cloud backends skip the test when their credentials are missing so
// CI without those secrets stays green.
func buildComputerFromEnv(t *testing.T) computer.Computer {
	t.Helper()
	backend := strings.ToLower(os.Getenv("TEST_BROWSER"))
	if backend == "" {
		backend = "local"
	}

	const w, h = 1600, 900

	switch backend {
	case "local":
		c, err := aptcomputer.New(aptcomputer.Config{Type: "local", Width: w, Height: h})
		if err != nil {
			t.Fatalf("create local: %v", err)
		}
		return c
	case "browserbase":
		k := os.Getenv("BROWSERBASE_API_KEY")
		p := os.Getenv("BROWSERBASE_PROJECT_ID")
		if k == "" || p == "" {
			t.Skip("TEST_BROWSER=browserbase requires BROWSERBASE_API_KEY + BROWSERBASE_PROJECT_ID")
		}
		c, err := aptcomputer.New(aptcomputer.Config{
			Type:          "browserbase",
			APIKey:        k,
			ProjectID:     p,
			Width:         w,
			Height:        h,
			SolveCaptchas: true,
		})
		if err != nil {
			t.Fatalf("create browserbase: %v", err)
		}
		if dbg, ok := c.(interface{ DebugURL() string }); ok && dbg.DebugURL() != "" {
			t.Logf("[BROWSERBASE] live view: %s", dbg.DebugURL())
		}
		return c
	case "steel":
		k := os.Getenv("STEEL_API_KEY")
		if k == "" {
			t.Skip("TEST_BROWSER=steel requires STEEL_API_KEY")
		}
		c, err := aptcomputer.New(aptcomputer.Config{
			Type:         "steel",
			APIKey:       k,
			Width:        w,
			Height:       h,
			SolveCaptcha: true,
		})
		if err != nil {
			t.Fatalf("create steel: %v", err)
		}
		if dbg, ok := c.(interface{ DebugURL() string }); ok && dbg.DebugURL() != "" {
			t.Logf("[STEEL] viewer: %s", dbg.DebugURL())
		}
		return c
	case "browser-engine":
		k := os.Getenv("BROWSER_API_KEY")
		if k == "" {
			k = os.Getenv("NEXT_PUBLIC_BROWSER_API_KEY")
		}
		if k == "" {
			t.Skip("TEST_BROWSER=browser-engine requires BROWSER_API_KEY (or NEXT_PUBLIC_BROWSER_API_KEY)")
		}
		baseURL := os.Getenv("BROWSER_API_URL")
		if baseURL == "" {
			baseURL = os.Getenv("NEXT_PUBLIC_BROWSER_API_URL")
		}
		c, err := aptcomputer.New(aptcomputer.Config{
			Type:         "browser-engine",
			APIKey:       k,
			URL:          baseURL,
			Width:        w,
			Height:       h,
			ProxyEnabled: os.Getenv("BROWSER_PROXY_ENABLED") == "1",
			ProxyCountry: os.Getenv("BROWSER_PROXY_COUNTRY"),
		})
		if err != nil {
			t.Fatalf("create browser-engine: %v", err)
		}
		if dbg, ok := c.(interface{ DebugURL() string }); ok && dbg.DebugURL() != "" {
			t.Logf("[BROWSER_ENGINE] debug: %s", dbg.DebugURL())
		}
		if sv, ok := c.(interface{ StreamURL() string }); ok && sv.StreamURL() != "" {
			t.Logf("[BROWSER_ENGINE] stream: %s", sv.StreamURL())
		}
		return c
	}
	t.Fatalf("unknown TEST_BROWSER=%q (want local|browserbase|steel|browser-engine)", backend)
	return nil
}

func backendName(t *testing.T) string {
	t.Helper()
	b := os.Getenv("TEST_BROWSER")
	if b == "" {
		return "local"
	}
	return b
}
