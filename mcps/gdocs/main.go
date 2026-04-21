// MCP server simulating Google Docs.
//
// State in GDOCS_DATA_DIR: docs.json — {id → {title, content}}.
//
// Tools:
//   load_doc(doc_id)           → {id, title, content}
//   create_doc(title, content) → {id}
//   update_doc(doc_id, content) → ok
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

type Doc struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

var (
	dataDir string
	docs    map[string]*Doc
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
	respond(id, map[string]any{"content": []map[string]string{{"type": "text", "text": text}}})
}

func path() string { return filepath.Join(dataDir, "docs.json") }

func load() {
	data, err := os.ReadFile(path())
	if err != nil {
		return
	}
	json.Unmarshal(data, &docs)
}

func mutate(fn func()) {
	f, err := os.OpenFile(path(), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		fn()
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		fn()
		return
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	data, _ := os.ReadFile(path())
	if len(data) > 0 {
		fresh := map[string]*Doc{}
		if json.Unmarshal(data, &fresh) == nil {
			docs = fresh
		}
	}
	fn()
	out, _ := json.MarshalIndent(docs, "", "  ")
	f.Truncate(0)
	f.Seek(0, 0)
	f.Write(out)
	f.Sync()
}

func refresh() {
	f, err := os.Open(path())
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err == nil {
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	data, _ := os.ReadFile(path())
	if len(data) == 0 {
		return
	}
	fresh := map[string]*Doc{}
	if json.Unmarshal(data, &fresh) == nil {
		docs = fresh
	}
}

func nextID() string {
	n := 0
	for range docs {
		n++
	}
	return fmt.Sprintf("doc_%03d", n+1)
}

func handleToolCall(id int64, name string, args map[string]string) {
	refresh()
	switch name {
	case "load_doc":
		docID := args["doc_id"]
		if docID == "" {
			respondError(id, -32602, "doc_id required")
			return
		}
		d, ok := docs[docID]
		if !ok {
			textResult(id, fmt.Sprintf("doc %s not found", docID))
			return
		}
		data, _ := json.Marshal(d)
		textResult(id, string(data))

	case "create_doc":
		title := args["title"]
		content := args["content"]
		if title == "" {
			respondError(id, -32602, "title required")
			return
		}
		var createdID string
		mutate(func() {
			if docs == nil {
				docs = map[string]*Doc{}
			}
			createdID = nextID()
			docs[createdID] = &Doc{ID: createdID, Title: title, Content: content}
		})
		link := fmt.Sprintf("https://docs.mock/document/%s", createdID)
		data, _ := json.Marshal(map[string]string{"id": createdID, "title": title, "link": link})
		textResult(id, string(data))

	case "update_doc":
		docID := args["doc_id"]
		content := args["content"]
		if docID == "" {
			respondError(id, -32602, "doc_id required")
			return
		}
		var msg string
		mutate(func() {
			d, ok := docs[docID]
			if !ok {
				msg = fmt.Sprintf("doc %s not found", docID)
				return
			}
			d.Content = content
			msg = fmt.Sprintf("updated doc %s", docID)
		})
		textResult(id, msg)

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func toolSchemas() []map[string]any {
	str := func(desc string) map[string]string { return map[string]string{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "load_doc",
			"description": "Load the full content of a Google Doc by id. Returns {id, title, content}.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"doc_id": str("Google Doc id")},
				"required":   []string{"doc_id"},
			},
		},
		{
			"name":        "create_doc",
			"description": "Create a new Google Doc. Returns {id, title, link}. The link is the public-shareable URL you place in sheets or drive references.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":   str("Document title"),
					"content": str("Full document body as plain text or markdown"),
				},
				"required": []string{"title", "content"},
			},
		},
		{
			"name":        "update_doc",
			"description": "Replace the body of an existing Google Doc.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"doc_id":  str("Google Doc id"),
					"content": str("New document body"),
				},
				"required": []string{"doc_id", "content"},
			},
		},
	}
}

func main() {
	dataDir = os.Getenv("GDOCS_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	docs = map[string]*Doc{}
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
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "gdocs", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{"tools": toolSchemas()})
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
