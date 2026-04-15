// MCP server for home intercom / doorbell / visitor management.
// Reads pending visits from HOME_DATA_DIR/visits.jsonl (the scenario
// framework appends a line when the doorbell is "pressed"), and writes
// announcement + unlock decisions back to visit_history.jsonl +
// audit.jsonl.
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

type Visit struct {
	ID       string `json:"id"`
	Time     string `json:"time"`
	CameraID string `json:"camera_id"`
	DoorID   string `json:"door_id"`
	Status   string `json:"status"` // "pending" | "admitted" | "denied"
	Reason   string `json:"reason,omitempty"`
}

type HomeState struct {
	Allowlist []string `json:"visitor_allowlist"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
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

func audit(tool string, args map[string]string) {
	entry := AuditEntry{Time: time.Now().UTC().Format(time.RFC3339), Tool: tool, Args: args}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func loadVisits() []Visit {
	f, err := os.Open(filepath.Join(dataDir, "visits.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var visits []Visit
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var v Visit
		if err := json.Unmarshal(sc.Bytes(), &v); err == nil {
			visits = append(visits, v)
		}
	}
	return visits
}

func saveVisits(visits []Visit) {
	f, err := os.Create(filepath.Join(dataDir, "visits.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	for _, v := range visits {
		data, _ := json.Marshal(v)
		f.WriteString(string(data) + "\n")
	}
}

func loadAllowlist() []string {
	data, err := os.ReadFile(filepath.Join(dataDir, "home.json"))
	if err != nil {
		return nil
	}
	var s HomeState
	json.Unmarshal(data, &s)
	return s.Allowlist
}

func appendHistory(v Visit) {
	data, _ := json.Marshal(v)
	f, _ := os.OpenFile(filepath.Join(dataDir, "visit_history.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "get_pending_visits":
		visits := loadVisits()
		var pending []Visit
		for _, v := range visits {
			if v.Status == "pending" {
				pending = append(pending, v)
			}
		}
		data, _ := json.Marshal(pending)
		textResult(id, string(data))

	case "list_history":
		visits := loadVisits()
		data, _ := json.Marshal(visits)
		textResult(id, string(data))

	case "get_allowlist":
		list := loadAllowlist()
		data, _ := json.Marshal(map[string]any{"allowlist": list})
		textResult(id, string(data))

	case "speak":
		msg := args["message"]
		room := args["room"]
		if msg == "" {
			respondError(id, -32602, "message required")
			return
		}
		if room == "" {
			room = "entrance"
		}
		textResult(id, fmt.Sprintf("Announced in %s: %q", room, msg))

	case "unlock_door":
		doorID := args["door_id"]
		visitID := args["visit_id"]
		reason := args["reason"]
		if doorID == "" {
			doorID = "front"
		}
		// Mark the matching visit as admitted if provided.
		if visitID != "" {
			visits := loadVisits()
			for i := range visits {
				if visits[i].ID == visitID {
					visits[i].Status = "admitted"
					visits[i].Reason = reason
					appendHistory(visits[i])
					break
				}
			}
			saveVisits(visits)
		}
		textResult(id, fmt.Sprintf("Door %s unlocked (visit=%s)", doorID, visitID))

	case "deny_entry":
		visitID := args["visit_id"]
		reason := args["reason"]
		if visitID == "" {
			respondError(id, -32602, "visit_id required")
			return
		}
		visits := loadVisits()
		for i := range visits {
			if visits[i].ID == visitID {
				visits[i].Status = "denied"
				visits[i].Reason = reason
				appendHistory(visits[i])
				break
			}
		}
		saveVisits(visits)
		textResult(id, fmt.Sprintf("Visit %s denied: %s", visitID, strings.TrimSpace(reason)))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("HOME_DATA_DIR")
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
				"serverInfo":      map[string]string{"name": "home_intercom", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "get_pending_visits",
						"description": "Return every unanswered doorbell press. Each visit has id, time, camera_id (for calling describe_scene), door_id, and status=pending. Call this on each wake to see what needs handling.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "list_history",
						"description": "Return the full visit log including admitted and denied entries. Useful for 'who came to the door today' queries.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_allowlist",
						"description": "Return the list of expected visitor descriptors (e.g. 'Amazon delivery', 'cleaning service'). Match a visitor's describe_scene output against this list before deciding whether to admit them.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "speak",
						"description": "Play a text announcement through the intercom speakers. Use this to greet visitors, relay instructions ('please leave the package at the door'), or speak to the owner in a room.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"message": map[string]string{"type": "string", "description": "Text to speak"},
								"room":    map[string]string{"type": "string", "description": "Room to announce in (defaults to entrance)"},
							},
							"required": []string{"message"},
						},
					},
					{
						"name":        "unlock_door",
						"description": "Unlock the front door for an admitted visitor. Pass visit_id to close out the pending visit and log it. Only call this AFTER checking describe_scene and the allowlist.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"door_id":  map[string]string{"type": "string", "description": "front | back | garage"},
								"visit_id": map[string]string{"type": "string", "description": "Visit id from get_pending_visits"},
								"reason":   map[string]string{"type": "string", "description": "Why this visit was admitted (for audit)"},
							},
							"required": []string{"door_id"},
						},
					},
					{
						"name":        "deny_entry",
						"description": "Reject a pending visit. Marks it denied in history. Use for visitors who don't match the allowlist. NEVER call unlock_door in the same thought as deny_entry on the same visit.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"visit_id": map[string]string{"type": "string", "description": "Visit id from get_pending_visits"},
								"reason":   map[string]string{"type": "string", "description": "Why denied"},
							},
							"required": []string{"visit_id"},
						},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
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
