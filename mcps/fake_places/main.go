// fake_places is a stub local-business discovery MCP used by scenario
// tests. It mimics the shape of Google Places / Serper Maps: search_text
// returns a list of canned businesses matching a query+location; get_place
// returns full details for one place id.
//
// State (all relative to FAKE_PLACES_DATA_DIR):
//   places.json    — [{id, name, address, city, country, phone, website,
//                      rating, reviews_count, types, employee_band}]
//                    seeded by the scenario before launch.
//   audit.jsonl    — one line per tool call (for verification).
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

// Place is the on-disk record the scenario seeds. All fields except
// employee_band correspond to what real Places APIs return; employee_band
// is a scenario convenience for downstream qualification.
type Place struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Address       string   `json:"address"`
	City          string   `json:"city"`
	Country       string   `json:"country"`
	Phone         string   `json:"phone"`
	Website       string   `json:"website"`
	Rating        float64  `json:"rating"`
	ReviewsCount  int      `json:"reviews_count"`
	Types         []string `json:"types"`
	EmployeeBand  string   `json:"employee_band"`
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

func loadPlaces() []Place {
	data, err := os.ReadFile(filepath.Join(dataDir, "places.json"))
	if err != nil {
		return nil
	}
	var places []Place
	json.Unmarshal(data, &places)
	return places
}

// matches checks if a place satisfies the query+location filter. Both
// arguments are case-insensitive substring matches across the name,
// types, address, city, country fields. Empty filters match everything.
func matches(p Place, query, location string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	l := strings.ToLower(strings.TrimSpace(location))

	if q != "" {
		hay := strings.ToLower(p.Name + " " + strings.Join(p.Types, " "))
		if !strings.Contains(hay, q) {
			return false
		}
	}
	if l != "" {
		hay := strings.ToLower(p.Address + " " + p.City + " " + p.Country)
		if !strings.Contains(hay, l) {
			return false
		}
	}
	return true
}

func handleToolCall(id int64, name string, args map[string]any) {
	audit(name, args)

	switch name {
	case "search_text":
		query, _ := args["query"].(string)
		location, _ := args["location"].(string)
		if query == "" {
			respondError(id, -32602, "query is required")
			return
		}
		limit := 20
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}

		// Return a slim summary — id, name, website, types, city, rating.
		// Force the agent to call get_place / webscraper for deeper info.
		type summary struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Website string   `json:"website"`
			Types   []string `json:"types"`
			City    string   `json:"city"`
			Rating  float64  `json:"rating"`
		}
		out := make([]summary, 0)
		for _, p := range loadPlaces() {
			if !matches(p, query, location) {
				continue
			}
			out = append(out, summary{
				ID: p.ID, Name: p.Name, Website: p.Website,
				Types: p.Types, City: p.City, Rating: p.Rating,
			})
			if len(out) >= limit {
				break
			}
		}
		data, _ := json.Marshal(out)
		textResult(id, string(data))

	case "get_place":
		placeID, _ := args["id"].(string)
		if placeID == "" {
			respondError(id, -32602, "id is required")
			return
		}
		for _, p := range loadPlaces() {
			if p.ID == placeID {
				data, _ := json.Marshal(p)
				textResult(id, string(data))
				return
			}
		}
		textResult(id, fmt.Sprintf("place %q not found", placeID))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("FAKE_PLACES_DATA_DIR")
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
				"serverInfo":      map[string]string{"name": "fake_places", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "search_text",
						"description": "Search local businesses by free-form query and (optional) location. Returns a list of {id, name, website, types, city, rating}. Call get_place(id) for full details (address, phone, reviews count, employee band).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query":    map[string]string{"type": "string", "description": "Business type or keyword, e.g. 'marketing agency', 'dentist', 'law firm'"},
								"location": map[string]string{"type": "string", "description": "Optional city/region/country, e.g. 'Lyon', 'France'"},
								"limit":    map[string]any{"type": "integer", "description": "Max results to return (default 20)"},
							},
							"required": []string{"query"},
						},
					},
					{
						"name":        "get_place",
						"description": "Get full details for a single place by id (from search_text). Returns address, phone, website, rating, reviews_count, types, employee_band.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Place id from search_text"},
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
