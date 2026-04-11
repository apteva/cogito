// MCP server for a simulated CMS (Content Management System).
// Supports multiple sites. Each site has articles with drafts and published states.
// State in CMS_DATA_DIR: sites.json, articles.json, actions.jsonl
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

type Site struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Categories  []string `json:"categories"`
}

type Article struct {
	ID          string `json:"id"`
	SiteID      string `json:"site_id"`
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	Content     string `json:"content"`
	Category    string `json:"category"`
	Tags        []string `json:"tags"`
	Status      string `json:"status"` // draft, published
	Author      string `json:"author"`
	CreatedAt   string `json:"created_at"`
	PublishedAt string `json:"published_at,omitempty"`
}

var nextArticleID = 100

func initNextArticleID() {
	var articles []Article
	if err := readJSON("articles.json", &articles); err == nil {
		for _, a := range articles {
			var id int
			fmt.Sscanf(a.ID, "art-%d", &id)
			if id >= nextArticleID {
				nextArticleID = id + 1
			}
		}
	}
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {
	case "list_sites":
		var sites []Site
		readJSON("sites.json", &sites)
		data, _ := json.MarshalIndent(sites, "", "  ")
		logAction(name, args, "listed sites")
		textResult(id, string(data))

	case "get_site":
		siteID := args["site_id"]
		if siteID == "" {
			respondError(id, -32602, "site_id required")
			return
		}
		var sites []Site
		readJSON("sites.json", &sites)
		for _, s := range sites {
			if s.ID == siteID {
				data, _ := json.MarshalIndent(s, "", "  ")
				logAction(name, args, "found")
				textResult(id, string(data))
				return
			}
		}
		logAction(name, args, "not found")
		textResult(id, fmt.Sprintf("Site %q not found", siteID))

	case "list_articles":
		siteID := args["site_id"]
		status := args["status"] // optional filter
		var articles []Article
		readJSON("articles.json", &articles)
		var filtered []Article
		for _, a := range articles {
			if siteID != "" && a.SiteID != siteID {
				continue
			}
			if status != "" && a.Status != status {
				continue
			}
			filtered = append(filtered, a)
		}
		// Return summary (no full content)
		type summary struct {
			ID        string `json:"id"`
			SiteID    string `json:"site_id"`
			Title     string `json:"title"`
			Category  string `json:"category"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		}
		var summaries []summary
		for _, a := range filtered {
			summaries = append(summaries, summary{ID: a.ID, SiteID: a.SiteID, Title: a.Title, Category: a.Category, Status: a.Status, CreatedAt: a.CreatedAt})
		}
		data, _ := json.MarshalIndent(summaries, "", "  ")
		logAction(name, args, fmt.Sprintf("%d articles", len(summaries)))
		textResult(id, string(data))

	case "get_article":
		articleID := args["article_id"]
		if articleID == "" {
			respondError(id, -32602, "article_id required")
			return
		}
		var articles []Article
		readJSON("articles.json", &articles)
		for _, a := range articles {
			if a.ID == articleID {
				data, _ := json.MarshalIndent(a, "", "  ")
				logAction(name, args, "found")
				textResult(id, string(data))
				return
			}
		}
		logAction(name, args, "not found")
		textResult(id, fmt.Sprintf("Article %q not found", articleID))

	case "create_draft":
		siteID := args["site_id"]
		title := args["title"]
		content := args["content"]
		category := args["category"]
		tagsStr := args["tags"]
		if siteID == "" || title == "" {
			respondError(id, -32602, "site_id and title required")
			return
		}
		if content == "" {
			content = "(draft — content pending)"
		}
		var tags []string
		if tagsStr != "" {
			for _, t := range strings.Split(tagsStr, ",") {
				tags = append(tags, strings.TrimSpace(t))
			}
		}
		slug := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(title, " ", "-"), "'", ""))
		article := Article{
			ID:        fmt.Sprintf("art-%d", nextArticleID),
			SiteID:    siteID,
			Title:     title,
			Slug:      slug,
			Content:   content,
			Category:  category,
			Tags:      tags,
			Status:    "draft",
			Author:    "ai-writer",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		nextArticleID++
		var articles []Article
		readJSON("articles.json", &articles)
		articles = append(articles, article)
		writeJSON("articles.json", articles)
		logAction(name, args, fmt.Sprintf("created %s", article.ID))
		textResult(id, fmt.Sprintf("Draft created: %s (id: %s, slug: %s). Use publish_article to make it live.", title, article.ID, slug))

	case "update_draft":
		articleID := args["article_id"]
		if articleID == "" {
			respondError(id, -32602, "article_id required")
			return
		}
		var articles []Article
		readJSON("articles.json", &articles)
		for i, a := range articles {
			if a.ID == articleID {
				if a.Status != "draft" {
					textResult(id, fmt.Sprintf("Article %s is already %s — cannot update", articleID, a.Status))
					logAction(name, args, "already published")
					return
				}
				if t := args["title"]; t != "" {
					articles[i].Title = t
				}
				if c := args["content"]; c != "" {
					articles[i].Content = c
				}
				if cat := args["category"]; cat != "" {
					articles[i].Category = cat
				}
				writeJSON("articles.json", articles)
				logAction(name, args, "updated")
				textResult(id, fmt.Sprintf("Draft %s updated.", articleID))
				return
			}
		}
		logAction(name, args, "not found")
		textResult(id, fmt.Sprintf("Article %q not found", articleID))

	case "publish_article":
		articleID := args["article_id"]
		if articleID == "" {
			respondError(id, -32602, "article_id required")
			return
		}
		var articles []Article
		readJSON("articles.json", &articles)
		for i, a := range articles {
			if a.ID == articleID {
				if a.Status == "published" {
					textResult(id, fmt.Sprintf("Article %s is already published.", articleID))
					logAction(name, args, "already published")
					return
				}
				articles[i].Status = "published"
				articles[i].PublishedAt = time.Now().UTC().Format(time.RFC3339)
				writeJSON("articles.json", articles)
				logAction(name, args, "published")
				textResult(id, fmt.Sprintf("Article \"%s\" published! Live at %s/%s", a.Title, a.SiteID, a.Slug))
				return
			}
		}
		logAction(name, args, "not found")
		textResult(id, fmt.Sprintf("Article %q not found", articleID))

	case "get_topics":
		siteID := args["site_id"]
		// Return trending/suggested topics based on site
		var sites []Site
		readJSON("sites.json", &sites)
		for _, s := range sites {
			if s.ID == siteID {
				data, _ := json.MarshalIndent(map[string]any{
					"site":       s.Name,
					"categories": s.Categories,
					"trending":   getTrendingTopics(siteID),
					"gaps":       getContentGaps(siteID),
				}, "", "  ")
				logAction(name, args, "topics retrieved")
				textResult(id, string(data))
				return
			}
		}
		logAction(name, args, "site not found")
		textResult(id, fmt.Sprintf("Site %q not found", siteID))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func getTrendingTopics(siteID string) []map[string]string {
	switch siteID {
	case "marcoschwartz":
		return []map[string]string{
			{"topic": "Building AI agents with Claude and MCP", "interest": "high", "competition": "low"},
			{"topic": "One-person AI companies — the new conglomerate", "interest": "high", "competition": "medium"},
			{"topic": "Automating content creation with autonomous agents", "interest": "medium", "competition": "low"},
			{"topic": "From solo developer to AI-powered fleet", "interest": "high", "competition": "low"},
			{"topic": "Building production MCP servers in Go", "interest": "medium", "competition": "very low"},
		}
	case "makecademy":
		return []map[string]string{
			{"topic": "ESP32 with AI — voice assistants on microcontrollers", "interest": "high", "competition": "low"},
			{"topic": "Raspberry Pi 5 home automation 2026 guide", "interest": "high", "competition": "medium"},
		}
	default:
		return []map[string]string{
			{"topic": "Getting started with AI automation", "interest": "high", "competition": "medium"},
		}
	}
}

func getContentGaps(siteID string) []string {
	switch siteID {
	case "marcoschwartz":
		return []string{
			"No recent articles about MCP (Model Context Protocol) — your core technology",
			"No case study showing a real AI fleet managing a business",
			"No tutorial on building autonomous agent hierarchies",
			"Last article was 3 weeks ago — publishing frequency dropped",
		}
	case "makecademy":
		return []string{
			"No ESP32-S3 content yet (most popular board of 2026)",
			"Missing beginner-friendly AI + hardware integration guide",
		}
	default:
		return []string{"Content analysis not available for this site"}
	}
}

func main() {
	dataDir = os.Getenv("CMS_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	// Initialize article ID counter from existing data
	initNextArticleID()

	// Seed default data if not exists
	if _, err := os.Stat(filepath.Join(dataDir, "sites.json")); os.IsNotExist(err) {
		writeJSON("sites.json", []Site{
			{ID: "marcoschwartz", Name: "Marco Schwartz", URL: "https://marcoschwartz.com", Description: "Personal site — tech articles, AI agents, entrepreneurship", Categories: []string{"AI & Agents", "Software Engineering", "Entrepreneurship", "Tutorials"}},
			{ID: "makecademy", Name: "Makecademy", URL: "https://makecademy.com", Description: "Arduino, Raspberry Pi, ESP32 tutorials and courses", Categories: []string{"Arduino", "Raspberry Pi", "ESP32", "IoT", "Home Automation"}},
			{ID: "openneurons", Name: "OpenNeurons", URL: "https://openneurons.com", Description: "Open-source AI news, tutorials, and courses", Categories: []string{"Open Source AI", "LLMs", "Agent Frameworks", "Tutorials"}},
		})
	}
	if _, err := os.Stat(filepath.Join(dataDir, "articles.json")); os.IsNotExist(err) {
		writeJSON("articles.json", []Article{
			{ID: "art-1", SiteID: "marcoschwartz", Title: "Building a Serverless Platform from Scratch", Slug: "building-serverless-platform", Content: "Full article about OmniKit architecture...", Category: "Software Engineering", Tags: []string{"serverless", "architecture", "omnikit"}, Status: "published", Author: "marco", CreatedAt: "2026-03-15T10:00:00Z", PublishedAt: "2026-03-15T12:00:00Z"},
			{ID: "art-2", SiteID: "marcoschwartz", Title: "Why I Built My Own AI Agent Framework", Slug: "why-i-built-apteva", Content: "The story behind Apteva...", Category: "AI & Agents", Tags: []string{"ai", "agents", "apteva"}, Status: "published", Author: "marco", CreatedAt: "2026-03-20T10:00:00Z", PublishedAt: "2026-03-20T14:00:00Z"},
			{ID: "art-3", SiteID: "makecademy", Title: "Getting Started with ESP32 and Arduino IDE", Slug: "esp32-getting-started", Content: "Step by step ESP32 setup...", Category: "ESP32", Tags: []string{"esp32", "arduino", "beginner"}, Status: "published", Author: "marco", CreatedAt: "2026-03-10T10:00:00Z", PublishedAt: "2026-03-10T11:00:00Z"},
		})
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
				"serverInfo":     map[string]string{"name": "cms", "version": "1.0.0"},
			})
		case "tools/list":
			respond(rid, map[string]any{
				"tools": []map[string]any{
					{
						"name": "list_sites", "description": "List all managed sites with their categories and descriptions.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name": "get_site", "description": "Get details of a specific site.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"site_id": map[string]string{"type": "string", "description": "Site ID"},
						}, "required": []string{"site_id"}},
					},
					{
						"name": "list_articles", "description": "List articles for a site. Optional status filter (draft/published).",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"site_id": map[string]string{"type": "string", "description": "Site ID (optional — omit for all sites)"},
							"status":  map[string]string{"type": "string", "description": "Filter by status: draft or published"},
						}},
					},
					{
						"name": "get_article", "description": "Get full article content by ID.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"article_id": map[string]string{"type": "string", "description": "Article ID"},
						}, "required": []string{"article_id"}},
					},
					{
						"name": "create_draft", "description": "Create a new article draft. Returns the article ID for further editing or publishing.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"site_id":  map[string]string{"type": "string", "description": "Site to publish on"},
							"title":    map[string]string{"type": "string", "description": "Article title"},
							"content":  map[string]string{"type": "string", "description": "Article body (markdown)"},
							"category": map[string]string{"type": "string", "description": "Category"},
							"tags":     map[string]string{"type": "string", "description": "Comma-separated tags"},
						}, "required": []string{"site_id", "title"}},
					},
					{
						"name": "update_draft", "description": "Update an existing draft article's title, content, or category.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"article_id": map[string]string{"type": "string", "description": "Article ID"},
							"title":      map[string]string{"type": "string", "description": "New title"},
							"content":    map[string]string{"type": "string", "description": "New content"},
							"category":   map[string]string{"type": "string", "description": "New category"},
						}, "required": []string{"article_id"}},
					},
					{
						"name": "publish_article", "description": "Publish a draft article. Makes it live on the site.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"article_id": map[string]string{"type": "string", "description": "Article ID to publish"},
						}, "required": []string{"article_id"}},
					},
					{
						"name": "get_topics", "description": "Get trending topics and content gaps for a site. Useful for planning what to write next.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
							"site_id": map[string]string{"type": "string", "description": "Site ID"},
						}, "required": []string{"site_id"}},
					},
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
			args := make(map[string]string)
			for k, v := range params.Arguments {
				switch val := v.(type) {
				case string:
					args[k] = val
				default:
					b, _ := json.Marshal(val)
					args[k] = string(b)
				}
			}
			handleToolCall(rid, params.Name, args)
		default:
			respondError(rid, -32601, fmt.Sprintf("unknown method: %s", req.Method))
		}
	}
}
