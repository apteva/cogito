// fake_email is a minimal inbox MCP used by scenario tests. It's
// deliberately simple: list_new reads inbox.json, read returns one email
// by id, reply appends to sent.jsonl, archive moves an id from inbox to
// archive.jsonl. The scenario seeds inbox.json upfront with whatever mix
// of real-work and noise messages it needs.
//
// State (all relative to FAKE_EMAIL_DATA_DIR):
//   inbox.json      — [{id, from, subject, body, kind}] — seeded, mutated
//                     by archive. `kind` is scenario-only metadata used
//                     for verification ("real" or "noise") but the agent
//                     doesn't see it.
//   sent.jsonl      — one line per outgoing reply
//   archive.jsonl   — one line per archived message
//   audit.jsonl     — one line per tool call (even rejections)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

// Email is the on-disk record the scenario seeds. `Kind` is scenario-side
// metadata ("real" / "noise") that the agent never sees — list_new and
// read strip it before returning.
type Email struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Kind    string `json:"kind,omitempty"`
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

// withInboxLock runs fn with exclusive access to inbox.json. Lets the agent
// safely parallelise archive calls across several worker threads without
// last-writer-wins races clobbering each other — same flock pattern we use
// in the sheets and social MCPs.
func withInboxLock(fn func(inbox []Email) []Email) {
	path := filepath.Join(dataDir, "inbox.json")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err == nil {
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	var inbox []Email
	data, _ := os.ReadFile(path)
	if len(data) > 0 {
		json.Unmarshal(data, &inbox)
	}
	next := fn(inbox)
	if next == nil {
		return
	}
	out, _ := json.MarshalIndent(next, "", "  ")
	f.Truncate(0)
	f.Seek(0, 0)
	f.Write(out)
}

// readInbox is a shared-lock snapshot used by read-only tools (list_new,
// read). Callers should not mutate the returned slice.
func readInbox() []Email {
	path := filepath.Join(dataDir, "inbox.json")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		return nil
	}
	var inbox []Email
	json.Unmarshal(data, &inbox)
	return inbox
}

// stripKind returns an email without the scenario-side Kind field so the
// agent never sees it. Marshaling an Email with an empty Kind skips the
// field thanks to the omitempty tag.
func stripKind(e Email) Email {
	e.Kind = ""
	return e
}

func handleToolCall(id int64, name string, args map[string]any) {
	audit(name, args)

	switch name {
	case "list_new":
		inbox := readInbox()
		// Return a lightweight summary (no body) so the agent has to
		// `read` each one it wants to act on — mirrors how a real IMAP
		// client works and gives the scenario a second decision point
		// per email.
		type summary struct {
			ID      string `json:"id"`
			From    string `json:"from"`
			Subject string `json:"subject"`
		}
		out := make([]summary, 0, len(inbox))
		for _, e := range inbox {
			out = append(out, summary{ID: e.ID, From: e.From, Subject: e.Subject})
		}
		data, _ := json.Marshal(out)
		textResult(id, string(data))

	case "read":
		emailID, _ := args["id"].(string)
		if emailID == "" {
			respondError(id, -32602, "id is required")
			return
		}
		for _, e := range readInbox() {
			if e.ID == emailID {
				data, _ := json.Marshal(stripKind(e))
				textResult(id, string(data))
				return
			}
		}
		textResult(id, fmt.Sprintf("email %q not found", emailID))

	case "reply":
		emailID, _ := args["id"].(string)
		body, _ := args["body"].(string)
		if emailID == "" || body == "" {
			respondError(id, -32602, "id and body are required")
			return
		}
		entry, _ := json.Marshal(map[string]any{
			"time": time.Now().UTC().Format(time.RFC3339Nano),
			"id":   emailID,
			"body": body,
		})
		f, err := os.OpenFile(filepath.Join(dataDir, "sent.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			respondError(id, -32603, fmt.Sprintf("persist: %v", err))
			return
		}
		f.WriteString(string(entry) + "\n")
		f.Close()
		textResult(id, fmt.Sprintf("Reply sent to %s", emailID))

	case "archive":
		emailID, _ := args["id"].(string)
		if emailID == "" {
			respondError(id, -32602, "id is required")
			return
		}
		var archived *Email
		withInboxLock(func(inbox []Email) []Email {
			next := make([]Email, 0, len(inbox))
			for i := range inbox {
				if inbox[i].ID == emailID {
					copy := inbox[i]
					archived = &copy
					continue
				}
				next = append(next, inbox[i])
			}
			return next
		})
		if archived == nil {
			textResult(id, fmt.Sprintf("email %q not in inbox", emailID))
			return
		}
		// Append to archive.jsonl so the scenario can assert which
		// messages actually got filed.
		entry, _ := json.Marshal(*archived)
		f, _ := os.OpenFile(filepath.Join(dataDir, "archive.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if f != nil {
			f.WriteString(string(entry) + "\n")
			f.Close()
		}
		textResult(id, fmt.Sprintf("Archived %s", emailID))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("FAKE_EMAIL_DATA_DIR")
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
				"serverInfo":      map[string]string{"name": "fake_email", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_new",
						"description": "List new emails in the inbox. Returns id, from, and subject for each — call read(id) to fetch the body.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "read",
						"description": "Fetch the full body of one email by id.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Email ID from list_new"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "reply",
						"description": "Send a reply to an email.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":   map[string]string{"type": "string", "description": "Email ID"},
								"body": map[string]string{"type": "string", "description": "Reply body"},
							},
							"required": []string{"id", "body"},
						},
					},
					{
						"name":        "archive",
						"description": "Move an email out of the inbox into the archive. Use this for messages that don't need a reply (auto-generated notifications, read receipts, duplicates, noise).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Email ID"},
							},
							"required": []string{"id"},
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
