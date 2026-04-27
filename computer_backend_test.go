package core

import (
	"fmt"
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
//
// Two opt-in flags extend the default config uniformly across all
// cloud backends (ignored on "local"):
//
//	TEST_BROWSER_PROXY=1       → managed residential proxy
//	TEST_BROWSER_SOLVE_CAPTCHA=1 (default on) → managed CAPTCHA solver
//	TEST_BROWSER_PROXY_COUNTRY → country hint (browser-engine only)
//
// Each cloud backend maps these onto its vendor-specific field:
// Browserbase.Proxies=true, Steel.UseProxy=true,
// BrowserEngine.ProxyEnabled=true. Without a unified flag any test
// that needs proxy must know the three field names — flipping them
// here keeps caller sites simple.
func buildComputerFromEnv(t *testing.T) computer.Computer {
	t.Helper()
	backend := strings.ToLower(os.Getenv("TEST_BROWSER"))
	if backend == "" {
		backend = "local"
	}

	const w, h = 1600, 900
	useProxy := os.Getenv("TEST_BROWSER_PROXY") == "1"
	// CAPTCHA solver defaults on — disable with TEST_BROWSER_SOLVE_CAPTCHA=0.
	solveCaptcha := os.Getenv("TEST_BROWSER_SOLVE_CAPTCHA") != "0"
	proxyCountry := os.Getenv("TEST_BROWSER_PROXY_COUNTRY")
	// Session lifetime in seconds, applied at creation across every
	// cloud backend. Browserbase + Steel cannot extend post-create
	// via API (verified against their SDKs), so multi-step flows
	// must request a generous lease here. 0 = each provider's default.
	sessionTimeout := 0
	if v := os.Getenv("TEST_BROWSER_SESSION_TIMEOUT"); v != "" {
		fmt.Sscanf(v, "%d", &sessionTimeout)
	}

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
		cfg := aptcomputer.Config{
			Type:          "browserbase",
			APIKey:        k,
			ProjectID:     p,
			Width:         w,
			Height:        h,
			SolveCaptchas: solveCaptcha,
			Timeout:       sessionTimeout,
		}
		if useProxy {
			cfg.Proxies = true // Browserbase managed residential proxy
		}
		c, err := aptcomputer.New(cfg)
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
			SolveCaptcha: solveCaptcha,
			UseProxy:     useProxy,
			Timeout:      sessionTimeout, // factory converts seconds → ms for Steel
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
		// Legacy env var BROWSER_PROXY_ENABLED still supported for
		// backwards compatibility with earlier test scripts.
		proxyEnabled := useProxy || os.Getenv("BROWSER_PROXY_ENABLED") == "1"
		country := proxyCountry
		if country == "" {
			country = os.Getenv("BROWSER_PROXY_COUNTRY")
		}
		c, err := aptcomputer.New(aptcomputer.Config{
			Type:         "browser-engine",
			APIKey:       k,
			URL:          baseURL,
			Timeout:      sessionTimeout,
			Width:        w,
			Height:       h,
			ProxyEnabled: proxyEnabled,
			ProxyCountry: country,
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
