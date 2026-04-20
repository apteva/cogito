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
