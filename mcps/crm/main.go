// MCP server for CRM (Customer Relationship Management) simulation.
// State in CRM_DATA_DIR: contacts.json, audit.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

type Contact struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	Company       string `json:"company"`
	Website       string `json:"website"`
	Industry      string `json:"industry"`
	EmployeeCount string `json:"employee_count"`
	Location      string `json:"location"`
	Description   string `json:"description"`
	Status        string `json:"status"` // new, enriched, contacted
	CreatedAt     string `json:"created_at"`
	EnrichedAt    string `json:"enriched_at"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

var (
	dataDir  string
	contacts []Contact
	nextID   int
)

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
	entry := AuditEntry{
		Time: time.Now().UTC().Format(time.RFC3339),
		Tool: tool,
		Args: args,
	}
	data, _ := json.Marshal(entry)
	f, err := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(string(data) + "\n")
}

func load() {
	data, err := os.ReadFile(filepath.Join(dataDir, "contacts.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &contacts)
	// Determine nextID from existing contacts
	for _, c := range contacts {
		if len(c.ID) > 2 {
			if n, err := strconv.Atoi(c.ID[2:]); err == nil && n >= nextID {
				nextID = n + 1
			}
		}
	}
}

func save() {
	data, _ := json.MarshalIndent(contacts, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "contacts.json"), data, 0644)
}

func findContact(id string) *Contact {
	for i := range contacts {
		if contacts[i].ID == id {
			return &contacts[i]
		}
	}
	return nil
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "create_contact":
		cName := args["name"]
		email := args["email"]
		if cName == "" || email == "" {
			respondError(id, -32602, "name and email are required")
			return
		}
		// Check for duplicate email
		for _, c := range contacts {
			if strings.EqualFold(c.Email, email) {
				textResult(id, fmt.Sprintf("contact with email %s already exists (id=%s)", email, c.ID))
				return
			}
		}
		status := args["status"]
		if status == "" {
			status = "new"
		}
		contact := Contact{
			ID:            fmt.Sprintf("c-%03d", nextID),
			Name:          cName,
			Email:         email,
			Company:       args["company"],
			Website:       args["website"],
			Industry:      args["industry"],
			EmployeeCount: args["employee_count"],
			Location:      args["location"],
			Description:   args["description"],
			Status:        status,
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		if contact.Industry != "" || contact.Location != "" || contact.Description != "" {
			contact.EnrichedAt = contact.CreatedAt
		}
		nextID++
		contacts = append(contacts, contact)
		save()
		data, _ := json.Marshal(contact)
		textResult(id, string(data))

	case "get_contact":
		contactID := args["id"]
		if contactID == "" {
			respondError(id, -32602, "id is required")
			return
		}
		c := findContact(contactID)
		if c == nil {
			textResult(id, fmt.Sprintf("contact %s not found", contactID))
			return
		}
		data, _ := json.Marshal(c)
		textResult(id, string(data))

	case "update_contact":
		contactID := args["id"]
		if contactID == "" {
			respondError(id, -32602, "id is required")
			return
		}
		c := findContact(contactID)
		if c == nil {
			textResult(id, fmt.Sprintf("contact %s not found", contactID))
			return
		}
		// Update any provided fields
		if v, ok := args["name"]; ok && v != "" {
			c.Name = v
		}
		if v, ok := args["email"]; ok && v != "" {
			c.Email = v
		}
		if v, ok := args["company"]; ok && v != "" {
			c.Company = v
		}
		if v, ok := args["website"]; ok && v != "" {
			c.Website = v
		}
		if v, ok := args["industry"]; ok && v != "" {
			c.Industry = v
		}
		if v, ok := args["employee_count"]; ok && v != "" {
			c.EmployeeCount = v
		}
		if v, ok := args["location"]; ok && v != "" {
			c.Location = v
		}
		if v, ok := args["description"]; ok && v != "" {
			c.Description = v
		}
		if v, ok := args["status"]; ok && v != "" {
			c.Status = v
		}
		if c.Status == "enriched" && c.EnrichedAt == "" {
			c.EnrichedAt = time.Now().UTC().Format(time.RFC3339)
		}
		save()
		data, _ := json.Marshal(c)
		textResult(id, string(data))

	case "list_contacts":
		statusFilter := args["status"]
		var result []Contact
		for _, c := range contacts {
			if statusFilter == "" || c.Status == statusFilter {
				result = append(result, c)
			}
		}
		if result == nil {
			result = []Contact{}
		}
		data, _ := json.Marshal(result)
		textResult(id, string(data))

	case "search_contacts":
		query := strings.ToLower(args["query"])
		if query == "" {
			respondError(id, -32602, "query is required")
			return
		}
		var result []Contact
		for _, c := range contacts {
			if strings.Contains(strings.ToLower(c.Email), query) ||
				strings.Contains(strings.ToLower(c.Company), query) ||
				strings.Contains(strings.ToLower(c.Name), query) {
				result = append(result, c)
			}
		}
		if result == nil {
			result = []Contact{}
		}
		data, _ := json.Marshal(result)
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("CRM_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	nextID = 1
	load()

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
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo":     map[string]string{"name": "crm", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "create_contact",
						"description": "Create a new contact in the CRM. Accepts all enrichment fields directly — pass them on create rather than create-then-update. Returns the created contact with its ID.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":           map[string]string{"type": "string", "description": "Contact full name"},
								"email":          map[string]string{"type": "string", "description": "Email address"},
								"company":        map[string]string{"type": "string", "description": "Company name"},
								"website":        map[string]string{"type": "string", "description": "Company website URL"},
								"industry":       map[string]string{"type": "string", "description": "Industry"},
								"employee_count": map[string]string{"type": "string", "description": "Employee count band, e.g. '5-10'"},
								"location":       map[string]string{"type": "string", "description": "Location, e.g. 'Lyon, France'"},
								"description":    map[string]string{"type": "string", "description": "Short company description"},
								"status":         map[string]string{"type": "string", "description": "Contact stage, e.g. 'new', 'enriched', 'contacted'"},
							},
							"required": []string{"name", "email"},
						},
					},
					{
						"name":        "get_contact",
						"description": "Get a contact by ID.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Contact ID (e.g. c-001)"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "update_contact",
						"description": "Update fields on an existing contact. Only provided fields are changed.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":             map[string]string{"type": "string", "description": "Contact ID"},
								"name":           map[string]string{"type": "string", "description": "Full name"},
								"email":          map[string]string{"type": "string", "description": "Email"},
								"company":        map[string]string{"type": "string", "description": "Company name"},
								"website":        map[string]string{"type": "string", "description": "Website URL"},
								"industry":       map[string]string{"type": "string", "description": "Industry/sector"},
								"employee_count": map[string]string{"type": "string", "description": "Employee count or range"},
								"location":       map[string]string{"type": "string", "description": "Company location"},
								"description":    map[string]string{"type": "string", "description": "Company description"},
								"status":         map[string]string{"type": "string", "description": "Contact status: new, enriched, contacted"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "list_contacts",
						"description": "List all contacts. Optionally filter by status.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"status": map[string]string{"type": "string", "description": "Filter by status (new, enriched, contacted). Empty = all."},
							},
						},
					},
					{
						"name":        "search_contacts",
						"description": "Search contacts by name, email, or company (case-insensitive substring match).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query": map[string]string{"type": "string", "description": "Search query"},
							},
							"required": []string{"query"},
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
