// MCP server for simulated hosting/deployment platform.
// State in HOSTING_DATA_DIR: sites.json, deployments.json, audit.jsonl
// Cross-reads CODEBASE_DIR to validate app files on deploy.
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

type Site struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"` // empty, building, live, failed
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
}

type Deployment struct {
	ID        string   `json:"id"`
	SiteID    string   `json:"site_id"`
	Status    string   `json:"status"`
	Files     []string `json:"files"`
	CreatedAt string   `json:"created_at"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

var (
	dataDir    string
	codebaseDir string
	sites      []Site
	deployments []Deployment
	nextSiteID int
	nextDeployID int
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
	entry := AuditEntry{Time: time.Now().UTC().Format(time.RFC3339), Tool: tool, Args: args}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func loadSites() {
	data, err := os.ReadFile(filepath.Join(dataDir, "sites.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &sites)
	for _, s := range sites {
		if len(s.ID) > 5 {
			var n int
			fmt.Sscanf(s.ID[5:], "%d", &n)
			if n >= nextSiteID {
				nextSiteID = n + 1
			}
		}
	}
}

func saveSites() {
	data, _ := json.MarshalIndent(sites, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "sites.json"), data, 0644)
}

func loadDeployments() {
	data, err := os.ReadFile(filepath.Join(dataDir, "deployments.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &deployments)
}

func saveDeployments() {
	data, _ := json.MarshalIndent(deployments, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "deployments.json"), data, 0644)
}

func findSite(id string) *Site {
	for i := range sites {
		if sites[i].ID == id {
			return &sites[i]
		}
	}
	return nil
}

// listAppFiles walks the codebase app dir and returns relative paths.
func listAppFiles(appDir string) []string {
	var files []string
	filepath.Walk(filepath.Join(codebaseDir, appDir), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(codebaseDir, appDir), path)
		files = append(files, rel)
		return nil
	})
	return files
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "create_site":
		siteName := args["name"]
		if siteName == "" {
			respondError(id, -32602, "name is required")
			return
		}
		// Check duplicate
		for _, s := range sites {
			if s.Name == siteName {
				data, _ := json.Marshal(s)
				textResult(id, string(data))
				return
			}
		}
		site := Site{
			ID:        fmt.Sprintf("site-%03d", nextSiteID),
			Name:      siteName,
			Status:    "empty",
			URL:       fmt.Sprintf("https://%s.apteva.app", siteName),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		nextSiteID++
		sites = append(sites, site)
		saveSites()
		data, _ := json.Marshal(site)
		textResult(id, string(data))

	case "deploy":
		siteID := args["site"]
		appDir := args["app_dir"]
		if siteID == "" {
			respondError(id, -32602, "site is required")
			return
		}
		if appDir == "" {
			appDir = "app"
		}
		site := findSite(siteID)
		if site == nil {
			textResult(id, fmt.Sprintf("ERROR: site %s not found", siteID))
			return
		}

		// Validate app files exist
		appPath := filepath.Join(codebaseDir, appDir)
		if _, err := os.Stat(filepath.Join(appPath, "package.json")); err != nil {
			textResult(id, "ERROR: package.json not found in "+appDir)
			return
		}
		if _, err := os.Stat(filepath.Join(appPath, "src", "index.tsx")); err != nil {
			if _, err2 := os.Stat(filepath.Join(appPath, "src", "index.jsx")); err2 != nil {
				textResult(id, "ERROR: src/index.tsx (or .jsx) not found in "+appDir)
				return
			}
		}

		files := listAppFiles(appDir)
		if len(files) == 0 {
			textResult(id, "ERROR: no files found in "+appDir)
			return
		}

		// Create deployment
		deploy := Deployment{
			ID:        fmt.Sprintf("deploy-%03d", nextDeployID),
			SiteID:    siteID,
			Status:    "live",
			Files:     files,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		nextDeployID++
		deployments = append(deployments, deploy)
		saveDeployments()

		// Update site status
		site.Status = "live"
		saveSites()

		result := map[string]any{
			"deployment_id": deploy.ID,
			"site":          site.Name,
			"url":           site.URL,
			"status":        "live",
			"files_deployed": len(files),
		}
		data, _ := json.Marshal(result)
		textResult(id, string(data))

	case "get_status":
		siteID := args["site"]
		if siteID == "" {
			respondError(id, -32602, "site is required")
			return
		}
		site := findSite(siteID)
		if site == nil {
			textResult(id, fmt.Sprintf("site %s not found", siteID))
			return
		}
		data, _ := json.Marshal(site)
		textResult(id, string(data))

	case "get_url":
		siteID := args["site"]
		if siteID == "" {
			respondError(id, -32602, "site is required")
			return
		}
		site := findSite(siteID)
		if site == nil {
			textResult(id, fmt.Sprintf("site %s not found", siteID))
			return
		}
		if site.Status != "live" {
			textResult(id, fmt.Sprintf("site %s is not live (status: %s)", site.Name, site.Status))
			return
		}
		textResult(id, site.URL)

	case "list_sites":
		if sites == nil {
			sites = []Site{}
		}
		data, _ := json.Marshal(sites)
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("HOSTING_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	codebaseDir = os.Getenv("CODEBASE_DIR")
	if codebaseDir == "" {
		codebaseDir = "."
	}
	nextSiteID = 1
	nextDeployID = 1
	loadSites()
	loadDeployments()

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
				"serverInfo":     map[string]string{"name": "hosting", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "create_site",
						"description": "Register a new site on the hosting platform. Returns site ID and URL.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]string{"type": "string", "description": "Site name (becomes subdomain: name.apteva.app)"},
							},
							"required": []string{"name"},
						},
					},
					{
						"name":        "deploy",
						"description": "Deploy an application to a site. Validates that package.json and src/index.tsx exist. Returns deployment status and live URL.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"site":    map[string]string{"type": "string", "description": "Site ID (from create_site)"},
								"app_dir": map[string]string{"type": "string", "description": "App directory relative to codebase root (default: app)"},
							},
							"required": []string{"site"},
						},
					},
					{
						"name":        "get_status",
						"description": "Check deployment status of a site (empty, building, live, failed).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"site": map[string]string{"type": "string", "description": "Site ID"},
							},
							"required": []string{"site"},
						},
					},
					{
						"name":        "get_url",
						"description": "Get the live URL for a deployed site.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"site": map[string]string{"type": "string", "description": "Site ID"},
							},
							"required": []string{"site"},
						},
					},
					{
						"name":        "list_sites",
						"description": "List all sites with their status and URLs.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
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
