// Package computer defines the Computer interface for screen-based environments.
// This package contains only the interface and types — no implementations.
// Implementations live in github.com/apteva/computer (browserbase, service, etc.)
package computer

// Action represents a normalized computer use action.
type Action struct {
	Type      string `json:"type"`                // "click", "double_click", "type", "key", "scroll", "screenshot", "navigate", "wait"
	X         int    `json:"x,omitempty"`         // click/scroll coordinate
	Y         int    `json:"y,omitempty"`         // click/scroll coordinate
	Text      string `json:"text,omitempty"`      // for "type" action
	Key       string `json:"key,omitempty"`       // for "key" action (e.g. "Enter", "Escape")
	Direction string `json:"direction,omitempty"` // for "scroll": "up", "down", "left", "right"
	Amount    int    `json:"amount,omitempty"`    // scroll amount
	URL       string `json:"url,omitempty"`       // for "navigate"
	Duration  int    `json:"duration,omitempty"`  // for "wait" (milliseconds)
	// Label: Set-of-Mark target. When non-zero, click/double_click
	// resolve the target via the label→bbox map populated by the
	// most recent screenshot. Takes precedence over X/Y when set.
	// Implementations that don't support SoM fall back to X/Y.
	Label int `json:"label,omitempty"`
}

// DisplaySize holds screen dimensions.
type DisplaySize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Context binds a session to a persistent state bundle (cookies,
// localStorage, IndexedDB, ServiceWorkers, Cache) that survives across
// sessions. Per-provider mapping:
//
//   - Browserbase   → browserSettings.context = {id, persist}
//   - Browser Engine → context = {id, persist}
//   - Steel         → profileId / persistProfile (Steel calls these
//     "profiles" but the lifecycle and intent match)
//
// IDs are provider-scoped: a Browserbase context id will not resolve on
// Steel, and vice versa. Concurrent attaches to the same context on the
// same provider are unsafe (Chrome can't share a user-data-dir); each
// backend serializes or 409s. Local / service backends ignore Context.
type Context struct {
	// ID is the provider-issued identifier returned at context-create time.
	ID string `json:"id"`
	// Persist controls whether changes (new cookies, storage writes) are
	// saved back to the context at session close. Default true mirrors
	// Browserbase's default; set false for one-shot read-only attaches.
	Persist bool `json:"persist"`
}

// Computer is the interface for screen-based environments.
type Computer interface {
	// Execute performs an action and returns a screenshot.
	Execute(action Action) (screenshot []byte, err error)

	// Screenshot takes a screenshot without performing any action.
	Screenshot() ([]byte, error)

	// DisplaySize returns the screen dimensions.
	DisplaySize() DisplaySize

	// Close terminates the session and releases resources.
	Close() error
}

// OpenOptions describes a session-open intent: which url to land on,
// which persistent context (if any) to bind, and whether to attach to
// an existing session id instead of creating a new one. The agent owns
// these decisions — they're tool-call arguments, not factory config.
type OpenOptions struct {
	// URL to navigate to after the session is established. Optional;
	// when empty the session is opened but no navigation is issued
	// (useful for resume to a session that's already on a page).
	URL string

	// ContextID binds the new session to a persistent context. Mutually
	// exclusive with SessionID. Provider-scoped — see Context.
	ContextID string
	// Persist controls whether changes are saved back to the context
	// at session close. Defaults true (matches Browserbase default).
	Persist bool

	// SessionID, when set, attaches to an existing session instead of
	// creating a new one. Mutually exclusive with ContextID. Provider
	// requirements vary: Browserbase needs the session to have been
	// created with KeepAlive=true; Browser Engine accepts both live
	// and snapshot-saved sessions; Steel and local backends reject it.
	SessionID string

	// Timeout sets the new session's max lifetime in seconds. Ignored
	// for SessionID attaches (the timeout was set at original create).
	// Zero leaves the provider's server-side default in place.
	Timeout int

	// Proxy, when non-nil, decides whether the new session routes
	// egress through the backend's managed residential proxy. nil
	// leaves the harness/backend default; &true forces on; &false
	// forces off. Honored by browser-engine, browserbase, steel;
	// ignored by local. Set by the agent via the browser_session
	// open tool — the agent owns the policy decision.
	Proxy *bool

	// ProxyCountry is an ISO-2 country code for the residential
	// proxy exit (e.g. "US"). Honored by browser-engine; ignored by
	// browserbase + steel (they need a custom proxy list for that).
	ProxyCountry string
}

// SessionOpener is implemented by Computers that own session lifecycle.
// One method covers create-with-context, attach-by-id, and re-bind to
// a different context — all by varying OpenOptions. Implementations
// MUST tear down the current session (if different) before establishing
// the new one. Local / service backends implement this as a thin nav.
type SessionOpener interface {
	OpenSession(opts OpenOptions) error
}
