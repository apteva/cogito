// MCP server for persistent key-value storage.
// State in STORAGE_DATA_DIR: store.json
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

var (
	dataDir string
	store   map[string]string
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

func save() {
	data, _ := json.MarshalIndent(store, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "store.json"), data, 0644)
}

func load() {
	data, err := os.ReadFile(filepath.Join(dataDir, "store.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &store)
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {
	case "store":
		key := args["key"]
		value := args["value"]
		if key == "" {
			respondError(id, -32602, "key is required")
			return
		}
		store[key] = value
		save()
		textResult(id, fmt.Sprintf("stored %q", key))

	case "get":
		key := args["key"]
		if key == "" {
			respondError(id, -32602, "key is required")
			return
		}
		val, ok := store[key]
		if !ok {
			textResult(id, fmt.Sprintf("key %q not found", key))
			return
		}
		textResult(id, val)

	case "list":
		var keys []string
		for k := range store {
			keys = append(keys, k)
		}
		data, _ := json.Marshal(keys)
		textResult(id, string(data))

	case "delete":
		key := args["key"]
		if key == "" {
			respondError(id, -32602, "key is required")
			return
		}
		delete(store, key)
		save()
		textResult(id, fmt.Sprintf("deleted %q", key))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("STORAGE_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	store = make(map[string]string)
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
				"serverInfo":     map[string]string{"name": "storage", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "store",
						"description": "Store a value by key. Overwrites if key exists.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"key":   map[string]string{"type": "string", "description": "Storage key"},
								"value": map[string]string{"type": "string", "description": "Value to store (can be JSON string)"},
							},
							"required": []string{"key", "value"},
						},
					},
					{
						"name":        "get",
						"description": "Retrieve a value by key.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"key": map[string]string{"type": "string", "description": "Storage key"},
							},
							"required": []string{"key"},
						},
					},
					{
						"name":        "list",
						"description": "List all stored keys.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "delete",
						"description": "Delete a key from storage.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"key": map[string]string{"type": "string", "description": "Key to delete"},
							},
							"required": []string{"key"},
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
