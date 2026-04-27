package computer

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ToolDefinition describes a tool for non-Anthropic providers.
type ToolDefinition struct {
	Name        string
	Description string
	Syntax      string
	Rules       string
	Parameters  map[string]any
}

// GetComputerToolDef returns the computer_use tool definition.
func GetComputerToolDef(display DisplaySize) ToolDefinition {
	return ToolDefinition{
		Name: "computer_use",
		Description: fmt.Sprintf(
			"Interact with the rendered browser page (%dx%d). One tool, many actions. "+
				"Every action returns a fresh screenshot — use it as your ground truth, "+
				"don't over-narrate between actions.",
			display.Width, display.Height,
		),
		Syntax: `[[computer_use action="screenshot"]]`,
		Rules: "" +
			"DEFAULT WORKFLOW (use this unless the task says otherwise):\n" +
			"  1. action=screenshot — see the current state. If Set-of-Mark is active you'll\n" +
			"     see small colored numeric badges on every interactive element:\n" +
			"       blue = link   |   green = button   |   orange = input/textarea/select   |   gray = other\n" +
			"  2. To click, read the badge number and use action=click, label=N.\n" +
			"     Never estimate pixels when a badge is visible — labels are reliable, coordinates aren't.\n" +
			"  3. To enter text, first click the input (action=click, label=N — this focuses the field),\n" +
			"     then action=type, text=\"...\" — the text goes into whichever field is focused.\n" +
			"  4. For keys like Enter / Escape / Tab / ctrl+c use action=key, key=\"Enter\".\n" +
			"  5. To reveal content below or above the viewport use action=scroll,\n" +
			"     direction=up|down|left|right, amount=3 (≈3 wheel ticks). After any scroll\n" +
			"     take a fresh screenshot — badges are re-enumerated for whatever's now visible.\n" +
			"\n" +
			"ACTIONS: screenshot | click | double_click | type | key | scroll | mouse_move | wait.\n" +
			"\n" +
			"FALLBACK: action=click + coordinate=\"x,y\" still works, but only use it when the target\n" +
			"has no badge (canvas, WebGL, custom widgets). Prefer label otherwise.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "Action: screenshot, click, double_click, type, key, scroll, mouse_move, wait",
				},
				"coordinate": map[string]any{
					"type":        "string",
					"description": "Position as \"x,y\" for click/double_click/scroll/mouse_move",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "Text to type (for type action)",
				},
				"key": map[string]any{
					"type":        "string",
					"description": "Key to press (for key action, e.g. Enter, Escape, ctrl+s)",
				},
				"direction": map[string]any{
					"type":        "string",
					"description": "Scroll direction: up, down, left, right",
				},
				"amount": map[string]any{
					"type":        "string",
					"description": "Scroll amount (default 3)",
				},
				"duration": map[string]any{
					"type":        "string",
					"description": "Wait duration in milliseconds (default 1000)",
				},
				"label": map[string]any{
					"type":        "string",
					"description": "Set-of-Mark target: integer label shown on a colored numeric badge in the screenshot. Alternative to coordinate for click/double_click. Use this when badges are visible.",
				},
			},
			"required": []string{"action"},
		},
	}
}

// GetSessionToolDef returns the browser_session tool definition.
func GetSessionToolDef() ToolDefinition {
	return ToolDefinition{
		Name:        "browser_session",
		Description: "Session lifecycle: open a URL, attach to a persistent context, resume an existing session, close. Does NOT return screenshots — take one with computer_use afterward to see the page.",
		Syntax:      `[[browser_session action="open" url="https://example.com"]]`,
		Rules: "" +
			"ACTIONS:\n" +
			"  open    — open a session and navigate. Three shapes:\n" +
			"              url=...                                fresh anonymous session, navigates.\n" +
			"              url=... context_id=ctx_abc             fresh session bound to a persistent\n" +
			"                                                     context (cookies, localStorage,\n" +
			"                                                     IndexedDB pre-loaded — usually\n" +
			"                                                     means you start already logged in).\n" +
			"                                                     Add persist=false for a read-only\n" +
			"                                                     attach (no writes saved back).\n" +
			"              session_id=sess_xyz [url=...]          attach to an existing session.\n" +
			"                                                     Skips nav when url is omitted —\n" +
			"                                                     useful when the session is already\n" +
			"                                                     on the right page.\n" +
			"            Pass timeout=N (seconds) for long tasks (e.g. login flows that wait on an\n" +
			"            emailed code). Default lease is short; if the session expires mid-task\n" +
			"            you cannot recover. When unsure, pad it.\n" +
			"            context_id and session_id are mutually exclusive.\n" +
			"  resume  — sugar for open(session_id=...). Works with Browserbase (only when the\n" +
			"            original session was keep-alive) and Browser Engine (live or snapshot\n" +
			"            replay). Steel / local cannot resume — open with the same context_id\n" +
			"            instead.\n" +
			"  close   — end the session. Persists context state (when persist=true). After close\n" +
			"            you cannot reopen the same session id, but you CAN open a new one bound\n" +
			"            to the same context_id and pick up where you left off.\n" +
			"  status  — current url + viewport + session_id + context_id + provider. Useful when\n" +
			"            you need to record a session_id for a later resume. Don't poll between\n" +
			"            every other action — the screenshot already shows the URL bar.\n" +
			"\n" +
			"WHEN TO USE A CONTEXT: any task that benefits from being already-logged-in (email,\n" +
			"social, dashboards). The operator sets contexts up; you receive their ids in your\n" +
			"directive or task brief and pass them to open. If you don't know a context id, open\n" +
			"anonymously.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "Action: open, close, resume, status",
				},
				"url": map[string]any{
					"type":        "string",
					"description": "URL to navigate to (for open). Optional when session_id is set and the existing session is already on the desired page.",
				},
				"context_id": map[string]any{
					"type":        "string",
					"description": "Persistent context id to bind the new session to (for open). Provider-scoped — use only ids the operator gave you. Mutually exclusive with session_id.",
				},
				"persist": map[string]any{
					"type":        "string",
					"description": "Whether to save context state at session close. \"true\" (default) writes back; \"false\" attaches read-only. Only meaningful with context_id.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Max session lifetime in seconds (for open). Use 1200+ for multi-step flows like email-code logins.",
				},
				"session_id": map[string]any{
					"type":        "string",
					"description": "Session id to attach to (for open / resume). Browserbase + Browser Engine only. Mutually exclusive with context_id.",
				},
			},
			"required": []string{"action"},
		},
	}
}

// AnthropicToolSpec is the native Claude computer use tool format.
type AnthropicToolSpec struct {
	Type            string `json:"type"`
	Name            string `json:"name"`
	DisplayWidthPx  int    `json:"display_width_px"`
	DisplayHeightPx int    `json:"display_height_px"`
}

// GetAnthropicToolSpec returns the native Anthropic computer use tool spec.
func GetAnthropicToolSpec(display DisplaySize, toolVersion string) AnthropicToolSpec {
	return AnthropicToolSpec{
		Type:            "computer_" + toolVersion,
		Name:            "computer",
		DisplayWidthPx:  display.Width,
		DisplayHeightPx: display.Height,
	}
}

// AnthropicBetaHeader returns the appropriate beta header for computer use.
func AnthropicBetaHeader(toolVersion string) string {
	switch toolVersion {
	case "20251124":
		return "computer-use-2025-11-24"
	default:
		return "computer-use-2025-01-24"
	}
}

// HandleComputerAction executes a screen interaction action (no navigate).
// Normalizes provider-specific action names (e.g. Claude's left_click → click).
// Retries screenshot if it fails after a click (page may be mid-navigation).
func HandleComputerAction(comp Computer, args map[string]string) (text string, screenshot []byte, err error) {
	rawAction := args["action"]
	if rawAction == "" {
		return "", nil, fmt.Errorf("missing action argument")
	}

	// Reject navigate — use browser_session for that
	if rawAction == "navigate" {
		return "", nil, fmt.Errorf("use browser_session to navigate to URLs, not computer_use")
	}

	// Normalize action name (left_click → click, etc.)
	actionType := NormalizeActionType(rawAction)

	action := Action{Type: actionType}
	parseCoordinate(args["coordinate"], &action)
	// SoM: label=N is an alternative to coordinate="x,y" for click /
	// double_click. The Computer implementation resolves it against
	// the label map populated by the most recent screenshot. Accept
	// integer, string, or JSON-numeric forms (providers differ in
	// how they stringify numeric tool-call args).
	if lbl := strings.TrimSpace(args["label"]); lbl != "" {
		// Strip surrounding quotes if provider passed "1" through JSON.
		lbl = strings.Trim(lbl, `"`)
		if n, err := strconv.Atoi(lbl); err == nil {
			action.Label = n
			fmt.Fprintf(os.Stderr, "[tooldef] parsed label=%d from args\n", n)
		} else {
			fmt.Fprintf(os.Stderr, "[tooldef] label raw=%q atoi failed: %v\n", lbl, err)
		}
	}
	action.Text = args["text"]
	action.Key = args["key"]

	// Claude sends scroll_direction + scroll_amount; we use direction + amount
	action.Direction = args["direction"]
	if action.Direction == "" {
		action.Direction = args["scroll_direction"]
	}
	if amt := args["amount"]; amt != "" {
		action.Amount, _ = strconv.Atoi(amt)
	} else if amt := args["scroll_amount"]; amt != "" {
		action.Amount, _ = strconv.Atoi(amt)
	}
	if dur := args["duration"]; dur != "" {
		action.Duration, _ = strconv.Atoi(dur)
	}

	start := time.Now()
	if actionType == "screenshot" {
		screenshot, err = comp.Screenshot()
	} else {
		screenshot, err = comp.Execute(action)
	}
	duration := time.Since(start)

	// If screenshot failed after an action (e.g. page mid-navigation), retry
	if err != nil && actionType != "screenshot" && strings.Contains(err.Error(), "screenshot") {
		// The action itself succeeded but the post-action screenshot failed.
		// Wait for page to settle and retry.
		for i := 0; i < 3; i++ {
			time.Sleep(500 * time.Millisecond)
			screenshot, err = comp.Screenshot()
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Sprintf("Action %s completed but screenshot failed: %v", rawAction, err), nil, err
		}
		duration = time.Since(start)
	} else if err != nil {
		return fmt.Sprintf("Error: %v", err), nil, err
	}

	text = fmt.Sprintf("Success: %s action completed. Screenshot attached (%d bytes, %dms).",
		rawAction, len(screenshot), duration.Milliseconds())
	return text, screenshot, nil
}

// HandleSessionAction manages browser session lifecycle.
func HandleSessionAction(comp Computer, args map[string]string) (text string, screenshot []byte, err error) {
	actionType := args["action"]
	if actionType == "" {
		return "", nil, fmt.Errorf("missing action argument")
	}

	switch actionType {
	case "open":
		url := args["url"]
		contextID := strings.TrimSpace(args["context_id"])
		sessionID := strings.TrimSpace(args["session_id"])
		if url == "" && sessionID == "" {
			return "", nil, fmt.Errorf("url or session_id required for open action")
		}
		if contextID != "" && sessionID != "" {
			return "", nil, fmt.Errorf("context_id and session_id are mutually exclusive (a context-bound session has its own id; pick one)")
		}
		// persist defaults true; explicit "false" / "0" opts out.
		persist := true
		if raw := strings.TrimSpace(args["persist"]); raw != "" {
			lower := strings.ToLower(strings.Trim(raw, `"`))
			if lower == "false" || lower == "0" || lower == "no" {
				persist = false
			}
		}
		var timeout int
		if raw := strings.TrimSpace(args["timeout"]); raw != "" {
			if secs, perr := strconv.Atoi(strings.Trim(raw, `"`)); perr == nil && secs > 0 {
				timeout = secs
			}
		}

		opts := OpenOptions{
			URL:       url,
			ContextID: contextID,
			Persist:   persist,
			SessionID: sessionID,
			Timeout:   timeout,
		}

		so, ok := comp.(SessionOpener)
		if !ok {
			// Backend doesn't own session lifecycle (legacy path). It can
			// still navigate; context_id / session_id are ignored.
			if contextID != "" || sessionID != "" {
				return "", nil, fmt.Errorf("this backend does not support context_id / session_id (only url is honored)")
			}
			start := time.Now()
			_, navErr := comp.Execute(Action{Type: "navigate", URL: url})
			if navErr != nil {
				return fmt.Sprintf("Error navigating to %s: %v", url, navErr), nil, navErr
			}
			return fmt.Sprintf("Navigated to %s (%dms). Use computer_use with action=screenshot to see the page.",
				url, time.Since(start).Milliseconds()), nil, nil
		}

		start := time.Now()
		if err := so.OpenSession(opts); err != nil {
			return fmt.Sprintf("Error opening session: %v", err), nil, err
		}
		duration := time.Since(start)
		text = describeOpenResult(opts, duration)
		return text, nil, nil

	case "close":
		if err := comp.Close(); err != nil {
			return fmt.Sprintf("Error closing session: %v", err), nil, err
		}
		return "Session closed.", nil, nil

	case "status":
		display := comp.DisplaySize()
		info := fmt.Sprintf("Browser active. Display: %dx%d.", display.Width, display.Height)
		// Check for optional session info
		if s, ok := comp.(SessionInfo); ok {
			info += fmt.Sprintf(" Type: %s.", s.SessionType())
			if id := s.SessionID(); id != "" {
				info += fmt.Sprintf(" Session ID: %s.", id)
			}
			if url := s.CurrentURL(); url != "" {
				info += fmt.Sprintf(" URL: %s.", url)
			}
		}
		if ci, ok := comp.(ContextInfo); ok {
			if cid := ci.ContextID(); cid != "" {
				info += fmt.Sprintf(" Context: %s.", cid)
			}
		}
		return info, nil, nil

	case "resume":
		sessionID := strings.TrimSpace(args["session_id"])
		if sessionID == "" {
			return "", nil, fmt.Errorf("session_id required for resume action")
		}
		// resume is sugar for open(session_id=...) — same SessionOpener path.
		if so, ok := comp.(SessionOpener); ok {
			if err := so.OpenSession(OpenOptions{SessionID: sessionID}); err != nil {
				return fmt.Sprintf("Error resuming session: %v", err), nil, err
			}
			return fmt.Sprintf("Resumed session %s. Use computer_use with action=screenshot to see the page.", sessionID), nil, nil
		}
		// Legacy fallback for backends that only implement Resumable.
		if r, ok := comp.(Resumable); ok {
			if err := r.Resume(sessionID); err != nil {
				return fmt.Sprintf("Error resuming session: %v", err), nil, err
			}
			return fmt.Sprintf("Resumed session %s. Use computer_use with action=screenshot to see the page.", sessionID), nil, nil
		}
		return "Resume not supported for this browser type.", nil, nil

	default:
		return "", nil, fmt.Errorf("unknown action: %s (use open, close, status, resume)", actionType)
	}
}

// describeOpenResult builds the human-readable response for a successful
// browser_session open. Distinguishes the three intent shapes (resume,
// context-bound create, anonymous create) so the agent gets a clear
// confirmation of which path ran.
func describeOpenResult(opts OpenOptions, duration time.Duration) string {
	var prefix string
	switch {
	case opts.SessionID != "":
		prefix = fmt.Sprintf("Resumed session %s", opts.SessionID)
	case opts.ContextID != "":
		persistNote := ""
		if !opts.Persist {
			persistNote = " (read-only)"
		}
		prefix = fmt.Sprintf("Opened session bound to context %s%s", opts.ContextID, persistNote)
	default:
		prefix = "Opened anonymous session"
	}
	if opts.URL != "" {
		return fmt.Sprintf("%s, navigated to %s (%dms). Use computer_use with action=screenshot to see the page.",
			prefix, opts.URL, duration.Milliseconds())
	}
	return fmt.Sprintf("%s (%dms). Use computer_use with action=screenshot to see the page.",
		prefix, duration.Milliseconds())
}

// SessionInfo is an optional interface for computers that can report session details.
type SessionInfo interface {
	SessionType() string // "local", "browserbase", "service"
	SessionID() string   // empty for local
	CurrentURL() string  // current page URL
}

// ContextInfo is an optional interface for computers attached to a
// persistent context. status surfaces the bound context id so the agent
// can confirm which identity it's running as. Implementations that do
// not support contexts (or aren't currently bound) should not implement
// this interface — the type assertion in status will simply skip it.
type ContextInfo interface {
	ContextID() string
}

// Resumable is an optional interface for computers that support session resumption.
type Resumable interface {
	Resume(sessionID string) error
}

// Timeoutable is an optional interface for computers whose backend
// session has a configurable max lifetime that the agent may want to
// extend mid-task. Browser Engine implements this; local Chrome and
// providers without an API-controlled lease return ErrNotSupported.
type Timeoutable interface {
	ExtendTimeout(seconds int) error
}

// geminiComputerUseActions maps Gemini native Computer Use function names.
// NormalizeActionType maps provider-specific action names to our standard names.
// Claude sends left_click, right_click, etc. — we normalize to what Computer.Execute understands.
func NormalizeActionType(action string) string {
	switch action {
	case "left_click":
		return "click"
	case "right_click":
		return "click" // TODO: right-click button support
	case "middle_click":
		return "click"
	case "triple_click":
		return "double_click" // closest approximation
	case "left_click_drag":
		return "click" // TODO: drag support
	case "left_mouse_down", "left_mouse_up":
		return "click" // TODO: fine-grained mouse support
	case "mouse_move":
		return "scroll" // move cursor — closest we have
	case "keypress":
		return "key"
	case "hold_key":
		return "key"
	}
	return action
}

var geminiComputerUseActions = map[string]bool{
	"click_at": true, "type_text_at": true, "hover_at": true,
	"scroll_at": true, "scroll_document": true, "key_combination": true,
	"drag_and_drop": true, "wait_5_seconds": true, "navigate": true,
	"go_back": true, "go_forward": true, "search": true,
	"open_web_browser": true,
}

// IsGeminiComputerAction returns true if the function name is a Gemini Computer Use predefined action.
func IsGeminiComputerAction(name string) bool {
	return geminiComputerUseActions[name]
}

// HandleGeminiComputerAction translates a Gemini Computer Use action to our Computer interface.
// Gemini uses normalized 0-999 coordinates; we denormalize to actual pixels.
func HandleGeminiComputerAction(comp Computer, name string, args map[string]string) (text string, screenshot []byte, err error) {
	display := comp.DisplaySize()

	denormX := func(s string) int {
		v, _ := strconv.Atoi(s)
		return int(float64(v) / 1000.0 * float64(display.Width))
	}
	denormY := func(s string) int {
		v, _ := strconv.Atoi(s)
		return int(float64(v) / 1000.0 * float64(display.Height))
	}

	var action Action
	switch name {
	case "click_at":
		action = Action{Type: "click", X: denormX(args["x"]), Y: denormY(args["y"])}
	case "type_text_at":
		action = Action{Type: "click", X: denormX(args["x"]), Y: denormY(args["y"])}
		// Click first, then type
		screenshot, err = comp.Execute(action)
		if err != nil {
			return fmt.Sprintf("Error clicking: %v", err), nil, err
		}
		action = Action{Type: "type", Text: args["text"]}
		screenshot, err = comp.Execute(action)
		if err != nil {
			return fmt.Sprintf("Error typing: %v", err), nil, err
		}
		if args["press_enter"] == "true" {
			action = Action{Type: "key", Key: "Enter"}
			screenshot, err = comp.Execute(action)
			if err != nil {
				return fmt.Sprintf("Error pressing enter: %v", err), nil, err
			}
		}
		return fmt.Sprintf("Typed %q at (%s,%s)", args["text"], args["x"], args["y"]), screenshot, nil
	case "hover_at":
		action = Action{Type: "mouse_move", X: denormX(args["x"]), Y: denormY(args["y"])}
	case "scroll_at":
		amt, _ := strconv.Atoi(args["magnitude"])
		if amt == 0 {
			amt = 3
		} else {
			amt = int(float64(amt) / 1000.0 * 10) // normalize magnitude
		}
		action = Action{Type: "scroll", X: denormX(args["x"]), Y: denormY(args["y"]), Direction: args["direction"], Amount: amt}
	case "scroll_document":
		action = Action{Type: "scroll", Direction: args["direction"], Amount: 3}
	case "key_combination":
		action = Action{Type: "key", Key: args["keys"]}
	case "drag_and_drop":
		// Execute as click-drag: click start, drag to end
		action = Action{Type: "click", X: denormX(args["x"]), Y: denormY(args["y"])}
		// Note: actual drag requires CDP-level implementation; this is simplified
	case "wait_5_seconds":
		action = Action{Type: "wait", Duration: 5000}
	case "navigate":
		action = Action{Type: "navigate", URL: args["url"]}
	case "go_back":
		action = Action{Type: "key", Key: "Alt+Left"}
	case "go_forward":
		action = Action{Type: "key", Key: "Alt+Right"}
	case "search":
		action = Action{Type: "navigate", URL: "https://www.google.com"}
	case "open_web_browser":
		return "Browser already open.", nil, nil
	default:
		return "", nil, fmt.Errorf("unknown Gemini action: %s", name)
	}

	start := time.Now()
	screenshot, err = comp.Execute(action)
	duration := time.Since(start)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil, err
	}
	return fmt.Sprintf("Success: %s completed (%dms)", name, duration.Milliseconds()), screenshot, nil
}

// parseCoordinate parses "x,y" or [x,y] format into action X/Y fields.
func parseCoordinate(coord string, action *Action) {
	if coord == "" {
		return
	}
	// Try JSON array [x, y]
	if strings.HasPrefix(coord, "[") {
		var arr []int
		if json.Unmarshal([]byte(coord), &arr) == nil && len(arr) == 2 {
			action.X = arr[0]
			action.Y = arr[1]
			return
		}
	}
	// Try "x,y"
	parts := strings.SplitN(coord, ",", 2)
	if len(parts) == 2 {
		action.X, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
		action.Y, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	}
}
