// MCP server for website scraping simulation.
// State in SCRAPER_DATA_DIR: sites.json (pre-seeded website data), audit.jsonl
// Returns simulated page content and structured company info for known URLs.
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

type SiteInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Industry    string `json:"industry"`
	Employees   string `json:"employees"`
	Location    string `json:"location"`
	Founded     string `json:"founded"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

var (
	dataDir string
	sites   map[string]*SiteInfo
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
	data, err := os.ReadFile(filepath.Join(dataDir, "sites.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &sites)
}

// normalizeURL strips trailing slashes and protocol variants for matching.
func normalizeURL(url string) string {
	url = strings.TrimRight(url, "/")
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimPrefix(url, "www.")
	return strings.ToLower(url)
}

// findSite looks up a URL in the sites map, handling protocol/trailing slash variations.
func findSite(url string) *SiteInfo {
	// Direct match first
	if s, ok := sites[url]; ok {
		return s
	}
	// Normalized match
	norm := normalizeURL(url)
	for key, s := range sites {
		if normalizeURL(key) == norm {
			return s
		}
	}
	return nil
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "fetch_page":
		url := args["url"]
		if url == "" {
			respondError(id, -32602, "url is required")
			return
		}
		site := findSite(url)
		if site == nil {
			textResult(id, fmt.Sprintf("ERROR: could not fetch %s — connection refused or site not found", url))
			return
		}
		// Return simulated page content
		page := fmt.Sprintf("=== %s ===\n\n%s\n\n%s", site.Title, site.Description, site.Body)
		data, _ := json.Marshal(map[string]string{
			"url":         url,
			"title":       site.Title,
			"description": site.Description,
			"body":        page,
		})
		textResult(id, string(data))

	case "extract_info":
		url := args["url"]
		if url == "" {
			respondError(id, -32602, "url is required")
			return
		}
		site := findSite(url)
		if site == nil {
			textResult(id, fmt.Sprintf("ERROR: could not fetch %s — connection refused or site not found", url))
			return
		}
		// Return structured company info
		data, _ := json.Marshal(map[string]string{
			"url":         url,
			"industry":    site.Industry,
			"employees":   site.Employees,
			"location":    site.Location,
			"founded":     site.Founded,
			"description": site.Description,
		})
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("SCRAPER_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	sites = make(map[string]*SiteInfo)
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
				"serverInfo":     map[string]string{"name": "webscraper", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "fetch_page",
						"description": "Fetch a web page and return its content (title, meta description, body text).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"url": map[string]string{"type": "string", "description": "URL to fetch"},
							},
							"required": []string{"url"},
						},
					},
					{
						"name":        "extract_info",
						"description": "Extract structured company information from a website URL. Returns industry, employee count, location, founded year, and description.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"url": map[string]string{"type": "string", "description": "Company website URL"},
							},
							"required": []string{"url"},
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
