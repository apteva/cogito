// fake_post is the downstream MCP that the agent spawns a worker against
// AFTER it has used fake_gateway to create a connection. It exposes four
// tools: post, schedule_post, delete_post, list_posts. Every call is
// validated against fake_gateway's connections.json:
//
//   1. The connection must exist AND have an api_key (proves the
//      create_connection step ran)
//   2. If the connection has a non-empty allowed_tools array, the
//      tool being called must be in it (proves the scoping step ran)
//
// This is the test-only mirror of the real apteva-server's per-MCP tool
// scoping feature. tools/list filters itself based on the connection's
// allowed_tools so an agent calling list first sees exactly the tools
// it's allowed to use.
//
// State:
//   FAKE_GATEWAY_DATA_DIR/connections.json  — read on every call
//   FAKE_POST_DATA_DIR/posts.jsonl          — live posts
//   FAKE_POST_DATA_DIR/scheduled.jsonl      — scheduled posts
//   FAKE_POST_DATA_DIR/audit.jsonl          — one entry per tool call,
//                                             including rejections
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

type gatewayConnection struct {
	ID           int64             `json:"id"`
	Slug         string            `json:"slug"`
	Credentials  map[string]string `json:"credentials"`
	AllowedTools []string          `json:"allowed_tools,omitempty"`
}

var (
	dataDir    string
	gatewayDir string
)

// toolCatalog is the full list of tools this MCP offers. A tools/list call
// intersects this with the connection's allowed_tools to produce the
// actually-visible set.
var toolCatalog = []map[string]any{
	{
		"name":        "post",
		"description": "Publish a status update to FakePost immediately.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]string{"type": "string", "description": "Post body (short status update, up to 280 chars)"},
			},
			"required": []string{"content"},
		},
	},
	{
		"name":        "schedule_post",
		"description": "Schedule a post to FakePost for a future time.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":  map[string]string{"type": "string", "description": "Post body"},
				"at_time":  map[string]string{"type": "string", "description": "ISO8601 UTC timestamp"},
			},
			"required": []string{"content", "at_time"},
		},
	},
	{
		"name":        "delete_post",
		"description": "Delete an existing FakePost. Dangerous — the post is gone forever.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"post_id": map[string]string{"type": "string", "description": "ID of the post to delete"},
			},
			"required": []string{"post_id"},
		},
	},
	{
		"name":        "list_posts",
		"description": "List the user's recent posts on FakePost.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{},
		},
	},
}

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

// loadConnection returns the first connections.json entry matching
// slug=fake_post, or nil if no valid connection exists. Valid = row
// present, has an api_key.
func loadConnection() *gatewayConnection {
	if gatewayDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(gatewayDir, "connections.json"))
	if err != nil {
		return nil
	}
	var conns []gatewayConnection
	if err := json.Unmarshal(data, &conns); err != nil {
		return nil
	}
	for i := range conns {
		if conns[i].Slug == "fake_post" && conns[i].Credentials["api_key"] != "" {
			return &conns[i]
		}
	}
	return nil
}

// allowedLookup builds a fast set from the connection's allowed_tools.
// Returns nil when the connection has no filter (all tools allowed).
func allowedLookup(conn *gatewayConnection) map[string]bool {
	if conn == nil || len(conn.AllowedTools) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, name := range conn.AllowedTools {
		set[name] = true
	}
	return set
}

func handleToolCall(id int64, name string, args map[string]any) {
	audit(name, args)

	// First check: does the caller have a valid connection at all?
	conn := loadConnection()
	if conn == nil {
		textResult(id, "REJECTED: no valid connection for fake_post. Use the gateway's create_connection tool first — it needs the api_key from the user.")
		return
	}
	// Second check: is the tool enabled on this connection?
	if allowed := allowedLookup(conn); allowed != nil && !allowed[name] {
		textResult(id, fmt.Sprintf(
			"REJECTED: tool %q is not enabled on this connection (filtered by allowed_tools=%v). Ask the user to widen the scope if you need it.",
			name, conn.AllowedTools,
		))
		return
	}

	switch name {
	case "post":
		content, _ := args["content"].(string)
		if content == "" {
			respondError(id, -32602, "content is required")
			return
		}
		line, _ := json.Marshal(map[string]any{
			"time":    time.Now().UTC().Format(time.RFC3339),
			"content": content,
		})
		f, err := os.OpenFile(filepath.Join(dataDir, "posts.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			respondError(id, -32603, fmt.Sprintf("persist: %v", err))
			return
		}
		f.WriteString(string(line) + "\n")
		f.Close()
		textResult(id, fmt.Sprintf("Posted %d chars to FakePost.", len(content)))

	case "schedule_post":
		content, _ := args["content"].(string)
		atTime, _ := args["at_time"].(string)
		if content == "" || atTime == "" {
			respondError(id, -32602, "content and at_time are required")
			return
		}
		line, _ := json.Marshal(map[string]any{
			"time":    time.Now().UTC().Format(time.RFC3339),
			"at_time": atTime,
			"content": content,
		})
		f, err := os.OpenFile(filepath.Join(dataDir, "scheduled.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			respondError(id, -32603, fmt.Sprintf("persist: %v", err))
			return
		}
		f.WriteString(string(line) + "\n")
		f.Close()
		textResult(id, fmt.Sprintf("Scheduled %d-char post for %s.", len(content), atTime))

	case "delete_post":
		postID, _ := args["post_id"].(string)
		if postID == "" {
			respondError(id, -32602, "post_id is required")
			return
		}
		// We don't actually delete anything — the scenario only uses
		// this tool to prove scoping works. If we reach this branch
		// it means the connection allowed delete_post AND the agent
		// called it; both facts get recorded via audit above.
		textResult(id, fmt.Sprintf("Deleted post %s.", postID))

	case "list_posts":
		data, _ := os.ReadFile(filepath.Join(dataDir, "posts.jsonl"))
		count := 0
		if len(data) > 0 {
			for _, line := range []byte(data) {
				if line == '\n' {
					count++
				}
			}
		}
		textResult(id, fmt.Sprintf("%d post(s) in history.", count))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

// filteredToolCatalog returns the subset of toolCatalog visible under the
// current connection's allowed_tools. When there's no connection or no
// filter, every tool is returned.
func filteredToolCatalog() []map[string]any {
	conn := loadConnection()
	allowed := allowedLookup(conn)
	if allowed == nil {
		return toolCatalog
	}
	var out []map[string]any
	for _, tool := range toolCatalog {
		name, _ := tool["name"].(string)
		if allowed[name] {
			out = append(out, tool)
		}
	}
	return out
}

func main() {
	dataDir = os.Getenv("FAKE_POST_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	gatewayDir = os.Getenv("FAKE_GATEWAY_DATA_DIR")

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
				"serverInfo":      map[string]string{"name": "fake_post", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": filteredToolCatalog(),
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
