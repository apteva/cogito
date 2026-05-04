// fake_email_verifier is a stub email-verification MCP used by scenario
// tests. It mimics the shape of NeverBounce / ZeroBounce: verify(email)
// returns "valid" / "invalid" / "unknown" based on a seeded list of
// known-good emails plus simple heuristics.
//
// State (relative to FAKE_EMAIL_VERIFIER_DATA_DIR):
//   valid_emails.json — ["alice@acmecorp.com", ...] — emails that should
//                       resolve as "valid". Anything not in this list
//                       resolves "invalid" unless caught by a heuristic.
//   audit.jsonl       — one line per verify call.
//
// Heuristics applied before the seeded list is consulted:
//   - empty / no '@'                  → invalid
//   - starts with "noreply"/"no-reply"
//     or "test@"                       → invalid
//   - domain contains "example"        → invalid
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var dataDir string

func respond(id int64, result any) {
	data, _ := json.Marshal(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
	fmt.Println(string(data))
}

func respondError(id int64, code int, msg string) {
	data, _ := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{code, msg},
	})
	fmt.Println(string(data))
}

func textResult(id int64, text string) {
	respond(id, map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

func audit(tool string, args map[string]any) {
	entry := map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano),
		"tool": tool,
		"args": args,
	}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func loadValid() map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(dataDir, "valid_emails.json"))
	if err != nil {
		return out
	}
	var list []string
	json.Unmarshal(data, &list)
	for _, e := range list {
		out[strings.ToLower(strings.TrimSpace(e))] = true
	}
	return out
}

func classify(email string) (status, reason string) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" || !strings.Contains(e, "@") {
		return "invalid", "missing @"
	}
	at := strings.Index(e, "@")
	local := e[:at]
	domain := e[at+1:]

	if strings.HasPrefix(local, "noreply") || strings.HasPrefix(local, "no-reply") || local == "test" {
		return "invalid", "role/test address"
	}
	if strings.Contains(domain, "example") {
		return "invalid", "reserved domain"
	}
	if loadValid()[e] {
		return "valid", "matched seeded list"
	}
	return "invalid", "not in deliverable list"
}

func handleToolCall(id int64, name string, args map[string]any) {
	audit(name, args)

	switch name {
	case "verify":
		email, _ := args["email"].(string)
		if email == "" {
			respondError(id, -32602, "email is required")
			return
		}
		status, reason := classify(email)
		out, _ := json.Marshal(map[string]any{
			"email":  email,
			"status": status,
			"reason": reason,
		})
		textResult(id, string(out))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("FAKE_EMAIL_VERIFIER_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		id := *req.ID

		switch req.Method {
		case "initialize":
			respond(id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "fake_email_verifier", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "verify",
						"description": "Verify deliverability of an email address. Returns {email, status, reason}. Status is one of 'valid' / 'invalid' / 'unknown'. Skip mailing addresses that come back invalid.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"email": map[string]string{"type": "string", "description": "Email address to verify"},
							},
							"required": []string{"email"},
						},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				respondError(id, -32602, "invalid params")
				continue
			}
			handleToolCall(id, params.Name, params.Arguments)
		default:
			respondError(id, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
