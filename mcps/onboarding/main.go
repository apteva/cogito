// MCP server for a simulated client onboarding pipeline.
// Tools: CRM (leads, proposals), Calendar (scheduling), Email (follow-ups).
// State in ONBOARDING_DATA_DIR: leads.json, proposals.json, calendar.json, emails.json, actions.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func logAction(tool string, args map[string]string, result string) {
	entry := map[string]string{"time": time.Now().UTC().Format(time.RFC3339), "tool": tool, "result": result}
	for k, v := range args {
		entry[k] = v
	}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "actions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func readJSON(name string, v any) error {
	data, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(name string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(filepath.Join(dataDir, name), data, 0644)
}

var nextID = 100

func initNextID() {
	// Scan all data files for highest ID
	for _, file := range []string{"leads.json", "proposals.json", "calendar.json", "emails.json"} {
		data, err := os.ReadFile(filepath.Join(dataDir, file))
		if err != nil {
			continue
		}
		var items []map[string]any
		json.Unmarshal(data, &items)
		for _, item := range items {
			if idStr, ok := item["id"].(string); ok {
				var id int
				fmt.Sscanf(idStr, "%d", &id)
				if id >= nextID {
					nextID = id + 1
				}
			}
		}
	}
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {

	// ── CRM: Leads ──

	case "get_leads":
		var leads []map[string]any
		readJSON("leads.json", &leads)
		// Filter by status if provided
		status := args["status"]
		if status != "" {
			var filtered []map[string]any
			for _, l := range leads {
				if s, _ := l["status"].(string); s == status {
					filtered = append(filtered, l)
				}
			}
			leads = filtered
		}
		data, _ := json.MarshalIndent(leads, "", "  ")
		logAction(name, args, fmt.Sprintf("%d leads", len(leads)))
		textResult(id, string(data))

	case "qualify_lead":
		leadID := args["lead_id"]
		score := args["score"]
		notes := args["notes"]
		if leadID == "" {
			respondError(id, -32602, "lead_id required")
			return
		}
		var leads []map[string]any
		readJSON("leads.json", &leads)
		for i, l := range leads {
			if lid, _ := l["id"].(string); lid == leadID {
				leads[i]["status"] = "qualified"
				leads[i]["score"] = score
				leads[i]["qualification_notes"] = notes
				leads[i]["qualified_at"] = time.Now().UTC().Format(time.RFC3339)
				writeJSON("leads.json", leads)
				logAction(name, args, "qualified")
				textResult(id, fmt.Sprintf("Lead %s qualified (score: %s). Notes: %s", leadID, score, notes))
				return
			}
		}
		logAction(name, args, "not found")
		textResult(id, fmt.Sprintf("Lead %q not found", leadID))

	case "update_lead":
		leadID := args["lead_id"]
		status := args["status"]
		if leadID == "" {
			respondError(id, -32602, "lead_id required")
			return
		}
		var leads []map[string]any
		readJSON("leads.json", &leads)
		for i, l := range leads {
			if lid, _ := l["id"].(string); lid == leadID {
				if status != "" {
					leads[i]["status"] = status
				}
				if n := args["notes"]; n != "" {
					leads[i]["notes"] = n
				}
				writeJSON("leads.json", leads)
				logAction(name, args, "updated")
				textResult(id, fmt.Sprintf("Lead %s updated to status: %s", leadID, status))
				return
			}
		}
		textResult(id, fmt.Sprintf("Lead %q not found", leadID))

	// ── Proposals ──

	case "draft_proposal":
		leadID := args["lead_id"]
		service := args["service"]
		price := args["price"]
		scope := args["scope"]
		if leadID == "" || service == "" {
			respondError(id, -32602, "lead_id and service required")
			return
		}
		if price == "" {
			price = "TBD"
		}
		proposal := map[string]any{
			"id":         fmt.Sprintf("prop-%d", nextID),
			"lead_id":    leadID,
			"service":    service,
			"price":      price,
			"scope":      scope,
			"status":     "draft",
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		nextID++
		var proposals []map[string]any
		readJSON("proposals.json", &proposals)
		proposals = append(proposals, proposal)
		writeJSON("proposals.json", proposals)
		logAction(name, args, fmt.Sprintf("drafted %s", proposal["id"]))
		textResult(id, fmt.Sprintf("Proposal %s drafted for lead %s:\n  Service: %s\n  Price: %s\n  Scope: %s\n  Status: draft — ready to send", proposal["id"], leadID, service, price, scope))

	case "send_proposal":
		proposalID := args["proposal_id"]
		if proposalID == "" {
			respondError(id, -32602, "proposal_id required")
			return
		}
		var proposals []map[string]any
		readJSON("proposals.json", &proposals)
		for i, p := range proposals {
			if pid, _ := p["id"].(string); pid == proposalID {
				proposals[i]["status"] = "sent"
				proposals[i]["sent_at"] = time.Now().UTC().Format(time.RFC3339)
				writeJSON("proposals.json", proposals)
				logAction(name, args, "sent")
				textResult(id, fmt.Sprintf("Proposal %s sent to client. Awaiting response.", proposalID))
				return
			}
		}
		textResult(id, fmt.Sprintf("Proposal %q not found", proposalID))

	// ── Calendar ──

	case "schedule_call":
		leadID := args["lead_id"]
		dateTime := args["datetime"]
		duration := args["duration"]
		subject := args["subject"]
		if leadID == "" || dateTime == "" {
			respondError(id, -32602, "lead_id and datetime required")
			return
		}
		if duration == "" {
			duration = "30min"
		}
		if subject == "" {
			subject = "Onboarding call"
		}
		event := map[string]any{
			"id":         fmt.Sprintf("cal-%d", nextID),
			"lead_id":    leadID,
			"datetime":   dateTime,
			"duration":   duration,
			"subject":    subject,
			"status":     "confirmed",
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		nextID++
		var calendar []map[string]any
		readJSON("calendar.json", &calendar)
		calendar = append(calendar, event)
		writeJSON("calendar.json", calendar)
		logAction(name, args, fmt.Sprintf("scheduled %s", event["id"]))
		textResult(id, fmt.Sprintf("Call scheduled: %s\n  Lead: %s\n  Time: %s (%s)\n  Subject: %s\n  Status: confirmed ✓", event["id"], leadID, dateTime, duration, subject))

	case "get_calendar":
		var calendar []map[string]any
		readJSON("calendar.json", &calendar)
		data, _ := json.MarshalIndent(calendar, "", "  ")
		logAction(name, args, fmt.Sprintf("%d events", len(calendar)))
		textResult(id, string(data))

	// ── Email ──

	case "send_email":
		to := args["to"]
		subject := args["subject"]
		body := args["body"]
		leadID := args["lead_id"]
		if to == "" || subject == "" {
			respondError(id, -32602, "to and subject required")
			return
		}
		email := map[string]any{
			"id":         fmt.Sprintf("email-%d", nextID),
			"to":         to,
			"lead_id":    leadID,
			"subject":    subject,
			"body":       body,
			"status":     "sent",
			"sent_at":    time.Now().UTC().Format(time.RFC3339),
		}
		nextID++
		var emails []map[string]any
		readJSON("emails.json", &emails)
		emails = append(emails, email)
		writeJSON("emails.json", emails)
		logAction(name, args, fmt.Sprintf("sent %s to %s", email["id"], to))
		textResult(id, fmt.Sprintf("Email sent to %s\n  Subject: %s\n  Status: delivered ✓", to, subject))

	case "get_emails":
		var emails []map[string]any
		readJSON("emails.json", &emails)
		leadID := args["lead_id"]
		if leadID != "" {
			var filtered []map[string]any
			for _, e := range emails {
				if lid, _ := e["lead_id"].(string); lid == leadID {
					filtered = append(filtered, e)
				}
			}
			emails = filtered
		}
		data, _ := json.MarshalIndent(emails, "", "  ")
		logAction(name, args, fmt.Sprintf("%d emails", len(emails)))
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("ONBOARDING_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	initNextID()

	// Seed data if not exists
	if _, err := os.Stat(filepath.Join(dataDir, "leads.json")); os.IsNotExist(err) {
		writeJSON("leads.json", []map[string]any{
			{
				"id": "lead-1", "name": "Sarah Chen", "email": "sarah@techstartup.io",
				"company": "TechStartup Inc", "role": "CTO",
				"status": "new", "source": "website",
				"interest": "AI agent automation for customer support",
				"budget": "$5k-10k/month", "team_size": "25 engineers",
				"notes": "Filled out contact form. Mentioned they're evaluating 3 platforms.",
				"created_at": "2026-04-09T08:00:00Z",
			},
		})
		writeJSON("proposals.json", []map[string]any{})
		writeJSON("calendar.json", []map[string]any{})
		writeJSON("emails.json", []map[string]any{})
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
		rid := *req.ID

		switch req.Method {
		case "initialize":
			respond(rid, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo":     map[string]string{"name": "onboarding", "version": "1.0.0"},
			})
		case "tools/list":
			respond(rid, map[string]any{
				"tools": []map[string]any{
					{"name": "get_leads", "description": "List leads in the CRM. Optional status filter (new, qualified, proposal_sent, scheduled, closed).", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"status": map[string]string{"type": "string", "description": "Filter by status"}}}},
					{"name": "qualify_lead", "description": "Qualify a lead with a score and notes. Marks them ready for proposal.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"lead_id": map[string]string{"type": "string"}, "score": map[string]string{"type": "string", "description": "Score 1-10"}, "notes": map[string]string{"type": "string", "description": "Qualification notes"}}, "required": []string{"lead_id"}}},
					{"name": "update_lead", "description": "Update a lead's status or notes.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"lead_id": map[string]string{"type": "string"}, "status": map[string]string{"type": "string"}, "notes": map[string]string{"type": "string"}}, "required": []string{"lead_id"}}},
					{"name": "draft_proposal", "description": "Create a proposal for a qualified lead.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"lead_id": map[string]string{"type": "string"}, "service": map[string]string{"type": "string", "description": "Service offered"}, "price": map[string]string{"type": "string", "description": "Monthly price"}, "scope": map[string]string{"type": "string", "description": "Scope of work"}}, "required": []string{"lead_id", "service"}}},
					{"name": "send_proposal", "description": "Send a drafted proposal to the client.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"proposal_id": map[string]string{"type": "string"}}, "required": []string{"proposal_id"}}},
					{"name": "schedule_call", "description": "Schedule an onboarding call with a lead.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"lead_id": map[string]string{"type": "string"}, "datetime": map[string]string{"type": "string", "description": "ISO datetime"}, "duration": map[string]string{"type": "string", "description": "e.g. 30min, 1h"}, "subject": map[string]string{"type": "string"}}, "required": []string{"lead_id", "datetime"}}},
					{"name": "get_calendar", "description": "View scheduled calls and events.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
					{"name": "send_email", "description": "Send an email to a contact.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"to": map[string]string{"type": "string", "description": "Email address"}, "subject": map[string]string{"type": "string"}, "body": map[string]string{"type": "string"}, "lead_id": map[string]string{"type": "string", "description": "Associated lead ID"}}, "required": []string{"to", "subject"}}},
					{"name": "get_emails", "description": "View sent emails. Optional lead_id filter.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"lead_id": map[string]string{"type": "string"}}}},
				},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				respondError(rid, -32602, "invalid params")
				continue
			}
			strArgs := make(map[string]string)
			for k, v := range params.Arguments {
				switch val := v.(type) {
				case string:
					strArgs[k] = val
				default:
					b, _ := json.Marshal(val)
					strArgs[k] = string(b)
				}
			}
			handleToolCall(rid, params.Name, strArgs)
		default:
			respondError(rid, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
