// fake_gateway is a test-only mock of the apteva-server MCP gateway. It
// lets scenarios exercise the "agent discovers → asks user → creates
// connection → uses it" flow without bringing up the real server.
//
// Tools exposed:
//   list_integrations       — returns a hardcoded catalog
//   get_integration(slug)   — returns credential_fields for one integration
//   create_connection(...)  — records a connection in connections.json
//
// State lives in FAKE_GATEWAY_DATA_DIR:
//   - catalog.json      (the hardcoded integration catalog, written at boot)
//   - connections.json  (append-only list written by create_connection)
//   - audit.jsonl       (one line per tool call, so scenarios can Wait on it)
//
// The `create_connection` tool only validates that credentials are present;
// it does NOT actually talk to any upstream. The downstream fake_post MCP
// reads connections.json on every call to decide whether to accept the
// request — that's how we verify end-to-end that the agent actually did
// the create-connection step.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// catalogEntry mirrors the real server's /integrations response shape: slug,
// name, description, and the credential fields the agent will need. Agents
// use `get_integration(slug)` to discover that `fake_post` needs an
// `api_key`, then they ask the user for it.
type catalogEntry struct {
	Slug             string            `json:"slug"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Tools            []string          `json:"tools"`
	CredentialFields []credentialField `json:"credential_fields"`
	MCPName          string            `json:"mcp_name"` // the fake stdio MCP the agent should spawn with after creating a connection
}

type credentialField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// hardcoded catalog — two integrations, only fake_post is fully wired.
// fake_chat is here so list_integrations has more than one entry and the
// agent actually has to make a choice. fake_post deliberately exposes
// four tools (post / schedule_post / delete_post / list_posts) so the
// scoping step has something to actually filter — a single-tool
// integration would make the allowed_tools flow untestable.
var catalog = []catalogEntry{
	{
		Slug:        "fake_post",
		Name:        "FakePost",
		Description: "Publish status updates to your FakePost account. Supports four operations: post (publish now), schedule_post (publish later), delete_post (remove an existing post), list_posts (read post history).",
		Tools:       []string{"post", "schedule_post", "delete_post", "list_posts"},
		CredentialFields: []credentialField{
			{Name: "api_key", Label: "API Key", Type: "password", Description: "Your FakePost API key (starts with fp_)"},
		},
		MCPName: "fake_post",
	},
	{
		Slug:        "fake_chat",
		Name:        "FakeChat",
		Description: "Send direct messages on FakeChat. Not suitable for public posting — use FakePost for that.",
		Tools:       []string{"send_dm"},
		CredentialFields: []credentialField{
			{Name: "bot_token", Label: "Bot Token", Type: "password", Description: "Your FakeChat bot token"},
		},
		MCPName: "fake_chat",
	},
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
		"time": time.Now().UTC().Format(time.RFC3339),
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

// Connection is what create_connection appends to connections.json. The
// fake_post MCP reads this file on every call to decide whether the caller
// has a valid connection AND whether the requested tool is in the scope.
//
// AllowedTools mirrors the real apteva-server's mcp_servers.allowed_tools
// feature we shipped earlier: when nil/empty the connection exposes every
// tool the integration offers; when populated it restricts the downstream
// to that exact subset. The downstream MCP (fake_post) is responsible for
// enforcing the filter — it reads this field on every tools/list and
// tools/call.
type Connection struct {
	ID           int64             `json:"id"`
	Slug         string            `json:"slug"`
	Credentials  map[string]string `json:"credentials"`
	AllowedTools []string          `json:"allowed_tools,omitempty"`
	CreatedAt    string            `json:"created_at"`
}

func loadConnections() []Connection {
	data, err := os.ReadFile(filepath.Join(dataDir, "connections.json"))
	if err != nil {
		return nil
	}
	var out []Connection
	json.Unmarshal(data, &out)
	return out
}

// saveConnections writes the full list under an exclusive advisory lock so
// two concurrent create_connection calls (if an agent ever fires them in
// parallel) can't stomp each other.
func saveConnections(conns []Connection) {
	path := filepath.Join(dataDir, "connections.json")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err == nil {
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	out, _ := json.MarshalIndent(conns, "", "  ")
	f.Truncate(0)
	f.Seek(0, 0)
	f.Write(out)
}

func findCatalog(slug string) *catalogEntry {
	for i := range catalog {
		if catalog[i].Slug == slug {
			return &catalog[i]
		}
	}
	return nil
}

func handleToolCall(id int64, name string, args map[string]any) {
	audit(name, args)

	switch name {
	case "list_integrations":
		// Return a lightweight summary — slug + name + description + tool count.
		// Agents typically follow up with get_integration(slug) to see the
		// credential schema.
		type summary struct {
			Slug        string `json:"slug"`
			Name        string `json:"name"`
			Description string `json:"description"`
			ToolCount   int    `json:"tool_count"`
		}
		out := make([]summary, 0, len(catalog))
		for _, c := range catalog {
			out = append(out, summary{
				Slug: c.Slug, Name: c.Name, Description: c.Description,
				ToolCount: len(c.Tools),
			})
		}
		data, _ := json.Marshal(out)
		textResult(id, string(data))

	case "get_integration":
		slug, _ := args["slug"].(string)
		if slug == "" {
			respondError(id, -32602, "slug is required")
			return
		}
		c := findCatalog(slug)
		if c == nil {
			textResult(id, fmt.Sprintf("integration %q not found. Call list_integrations to see what's available.", slug))
			return
		}
		data, _ := json.Marshal(c)
		textResult(id, string(data))

	case "create_connection":
		slug, _ := args["slug"].(string)
		if slug == "" {
			respondError(id, -32602, "slug is required")
			return
		}
		c := findCatalog(slug)
		if c == nil {
			textResult(id, fmt.Sprintf("integration %q not found", slug))
			return
		}
		// Accept credentials as a JSON string (the agent's most common
		// shape) or a native object. Either way we flatten to a string map.
		creds := map[string]string{}
		switch v := args["credentials"].(type) {
		case string:
			if v != "" {
				_ = json.Unmarshal([]byte(v), &creds)
			}
		case map[string]any:
			for k, val := range v {
				creds[k] = fmt.Sprintf("%v", val)
			}
		}
		// Validate that every required credential field was supplied. This
		// is the check that forces the agent to actually ask the user for
		// the api_key — without this the LLM might just skip the step.
		var missing []string
		for _, f := range c.CredentialFields {
			if creds[f.Name] == "" {
				missing = append(missing, f.Name)
			}
		}
		if len(missing) > 0 {
			textResult(id, fmt.Sprintf(
				"missing credentials for %s: %v. Ask the user for these values first, then call create_connection again.",
				slug, missing,
			))
			return
		}

		// Parse allowed_tools. Accept comma-separated string OR JSON array
		// OR native []any — mirrors what parseCSV does server-side. When
		// empty or missing, the connection exposes every tool the
		// integration offers (legacy behaviour). When populated we
		// validate every name is actually part of this integration,
		// otherwise the agent could mask bad decisions as typos.
		var allowedTools []string
		switch v := args["allowed_tools"].(type) {
		case string:
			s := strings.TrimSpace(v)
			if s != "" {
				if strings.HasPrefix(s, "[") {
					_ = json.Unmarshal([]byte(s), &allowedTools)
				} else {
					for _, part := range strings.Split(s, ",") {
						if p := strings.TrimSpace(part); p != "" {
							allowedTools = append(allowedTools, p)
						}
					}
				}
			}
		case []any:
			for _, item := range v {
				if str, ok := item.(string); ok && str != "" {
					allowedTools = append(allowedTools, str)
				}
			}
		}
		if len(allowedTools) > 0 {
			validSet := map[string]bool{}
			for _, t := range c.Tools {
				validSet[t] = true
			}
			var bad []string
			for _, name := range allowedTools {
				if !validSet[name] {
					bad = append(bad, name)
				}
			}
			if len(bad) > 0 {
				textResult(id, fmt.Sprintf(
					"unknown tool(s) for %s: %v. Valid tools are: %v. Call get_integration(slug=%q) to see the list, then retry.",
					slug, bad, c.Tools, slug,
				))
				return
			}
		}

		conns := loadConnections()
		newID := int64(len(conns) + 1)
		conn := Connection{
			ID:           newID,
			Slug:         slug,
			Credentials:  creds,
			AllowedTools: allowedTools,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		conns = append(conns, conn)
		saveConnections(conns)

		// Return the same shape the real apteva-server gateway returns —
		// connection_id + a connect_now instruction telling the agent
		// how to use it next. The mcp_name field points at the sibling
		// fake_post MCP binary the scenario has already attached as a
		// catalogued (non-main) MCP. When the caller scoped the
		// connection we surface the effective tool count so the agent
		// knows what's actually reachable.
		effectiveTools := c.Tools
		if len(allowedTools) > 0 {
			effectiveTools = allowedTools
		}
		result := map[string]any{
			"connection_id":  newID,
			"status":         "connected",
			"mcp_name":       c.MCPName,
			"tools_count":    len(effectiveTools),
			"enabled_tools":  effectiveTools,
			"connect_now": fmt.Sprintf(
				"Connection ready. To use it, spawn a worker with mcp=%q — that exposes %d tool(s): %v. Credentials are stored; DO NOT pass the api_key around in directives or messages.",
				c.MCPName, len(effectiveTools), effectiveTools,
			),
		}
		out, _ := json.Marshal(result)
		textResult(id, string(out))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("FAKE_GATEWAY_DATA_DIR")
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
				"serverInfo":      map[string]string{"name": "fake_gateway", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_integrations",
						"description": "Browse available integration templates. Returns slug, name, description and tool count for each. Call get_integration(slug) to see the credential fields for one.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_integration",
						"description": "Get the full manifest for one integration — including which credentials the user must provide. Always call this before create_connection so you know what fields are required.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"slug": map[string]string{"type": "string", "description": "Integration slug from list_integrations"},
							},
							"required": []string{"slug"},
						},
					},
					{
						"name":        "create_connection",
						"description": "Create a connection for an integration using credentials supplied by the user. Returns a connect_now hint with the MCP name you should spawn workers with to actually use the tools. NEVER invent credentials — ask the user first. Use `allowed_tools` to scope the connection down to the exact subset of tools needed (least-privilege); omit or leave empty to expose every tool from the integration.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"slug":          map[string]string{"type": "string", "description": "Integration slug"},
								"credentials":   map[string]string{"type": "string", "description": "JSON object of credential fields, e.g. {\"api_key\":\"fp_real_key_here\"}"},
								"allowed_tools": map[string]string{"type": "string", "description": "Comma-separated list of tool names to expose on this connection (e.g. 'post,schedule_post'). Leave empty for all. Use get_integration first to see valid names."},
							},
							"required": []string{"slug", "credentials"},
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
