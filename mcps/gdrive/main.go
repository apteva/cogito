// MCP server simulating Google Drive.
//
// State in GDRIVE_DATA_DIR: drive.json — a flat list of {id, name, type,
// parent_id, size}. Types: folder, doc, sheet, audio, file.
//
// Tools:
//   find_folder(name)          → {id, ...} or error if not unique
//   find_file(parent_id, name) → {id, type, size, ...}
//   list_folder(folder_id)     → [{id, name, type, ...}, ...]
//   get_download_url(file_id)  → s3 URL (synthetic)
//   create_file(parent_id, name, type, doc_id?) → {id}
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type Entry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	ParentID string `json:"parent_id,omitempty"`
	Size     int    `json:"size,omitempty"`
	DocID    string `json:"doc_id,omitempty"` // link to gdocs for type=doc
}

var (
	dataDir string
	entries []Entry
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

func path() string { return filepath.Join(dataDir, "drive.json") }

func load() {
	data, err := os.ReadFile(path())
	if err != nil {
		return
	}
	json.Unmarshal(data, &entries)
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
		var fresh []Entry
		if json.Unmarshal(data, &fresh) == nil {
			entries = fresh
		}
	}
	fn()
	out, _ := json.MarshalIndent(entries, "", "  ")
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
	var fresh []Entry
	if json.Unmarshal(data, &fresh) == nil {
		entries = fresh
	}
}

func nextID(prefix string) string {
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.ID, prefix) {
			n++
		}
	}
	return fmt.Sprintf("%s_%03d", prefix, n+1)
}

func handleToolCall(id int64, name string, args map[string]string) {
	refresh()
	switch name {
	case "find_folder":
		target := args["name"]
		if target == "" {
			respondError(id, -32602, "name is required")
			return
		}
		var matches []Entry
		for _, e := range entries {
			if e.Type == "folder" && e.Name == target {
				matches = append(matches, e)
			}
		}
		if len(matches) == 0 {
			textResult(id, fmt.Sprintf("no folder named %q", target))
			return
		}
		if len(matches) > 1 {
			data, _ := json.Marshal(matches)
			textResult(id, fmt.Sprintf("multiple folders named %q: %s", target, data))
			return
		}
		data, _ := json.Marshal(matches[0])
		textResult(id, string(data))

	case "find_file":
		parent := args["parent_id"]
		target := args["name"]
		if parent == "" || target == "" {
			respondError(id, -32602, "parent_id and name required")
			return
		}
		for _, e := range entries {
			if e.ParentID == parent && e.Name == target {
				data, _ := json.Marshal(e)
				textResult(id, string(data))
				return
			}
		}
		textResult(id, fmt.Sprintf("no file %q in folder %s", target, parent))

	case "list_folder":
		folderID := args["folder_id"]
		if folderID == "" {
			respondError(id, -32602, "folder_id required")
			return
		}
		var children []Entry
		for _, e := range entries {
			if e.ParentID == folderID {
				children = append(children, e)
			}
		}
		if children == nil {
			children = []Entry{}
		}
		data, _ := json.Marshal(children)
		textResult(id, string(data))

	case "get_download_url":
		fileID := args["file_id"]
		if fileID == "" {
			respondError(id, -32602, "file_id required")
			return
		}
		var found *Entry
		for i := range entries {
			if entries[i].ID == fileID {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			textResult(id, fmt.Sprintf("file %s not found", fileID))
			return
		}
		url := fmt.Sprintf("https://s3.mock/apteva-drive/%s/%s?sig=test", found.ID, found.Name)
		data, _ := json.Marshal(map[string]string{
			"file_id": found.ID,
			"name":    found.Name,
			"url":     url,
		})
		textResult(id, string(data))

	case "create_file":
		parent := args["parent_id"]
		nm := args["name"]
		tp := args["type"]
		if parent == "" || nm == "" || tp == "" {
			respondError(id, -32602, "parent_id, name, type required")
			return
		}
		var createdID string
		mutate(func() {
			ent := Entry{
				ID:       nextID("f"),
				Name:     nm,
				Type:     tp,
				ParentID: parent,
				DocID:    args["doc_id"],
			}
			entries = append(entries, ent)
			createdID = ent.ID
		})
		data, _ := json.Marshal(map[string]string{"id": createdID, "name": nm})
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func toolSchemas() []map[string]any {
	str := func(desc string) map[string]string { return map[string]string{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "find_folder",
			"description": "Find a folder by name. Returns {id, name, type, parent_id}.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": str("Folder name")},
				"required":   []string{"name"},
			},
		},
		{
			"name":        "find_file",
			"description": "Find a file by exact name inside a folder. Returns {id, name, type, parent_id, size}.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"parent_id": str("Parent folder id"),
					"name":      str("File name"),
				},
				"required": []string{"parent_id", "name"},
			},
		},
		{
			"name":        "list_folder",
			"description": "List every entry (files and subfolders) inside a folder.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"folder_id": str("Folder id")},
				"required":   []string{"folder_id"},
			},
		},
		{
			"name":        "get_download_url",
			"description": "Return a time-limited S3 download URL for a file. Use this URL when passing the file to an external transcription API.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"file_id": str("File id")},
				"required":   []string{"file_id"},
			},
		},
		{
			"name":        "create_file",
			"description": "Create a new file (or doc reference) inside a folder. Use type=doc and doc_id to link a generated Google Doc.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"parent_id": str("Parent folder id"),
					"name":      str("File name"),
					"type":      str("file | doc | sheet | audio"),
					"doc_id":    str("Optional gdocs doc id (for type=doc)"),
				},
				"required": []string{"parent_id", "name", "type"},
			},
		},
	}
}

func main() {
	dataDir = os.Getenv("GDRIVE_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
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
				"serverInfo":      map[string]string{"name": "gdrive", "version": "1.0.0"},
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
