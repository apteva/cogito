// MCP server for Google Sheets simulation.
// State in SHEETS_DATA_DIR: sheets.json
// Each sheet is a named spreadsheet with columns and rows.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

type Sheet struct {
	Columns []string            `json:"columns"`
	Rows    []map[string]string `json:"rows"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

var (
	dataDir string
	sheets  map[string]*Sheet
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
	data, err := os.ReadFile(filepath.Join(dataDir, "sheets.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &sheets)
}

func save() {
	data, _ := json.MarshalIndent(sheets, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "sheets.json"), data, 0644)
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "list_sheets":
		var names []string
		for n := range sheets {
			names = append(names, n)
		}
		if names == nil {
			names = []string{}
		}
		data, _ := json.Marshal(names)
		textResult(id, string(data))

	case "read_sheet":
		sheetName := args["sheet"]
		if sheetName == "" {
			respondError(id, -32602, "sheet is required")
			return
		}
		s, ok := sheets[sheetName]
		if !ok {
			textResult(id, fmt.Sprintf("sheet %q not found", sheetName))
			return
		}
		// Return rows with row indices
		type indexedRow struct {
			Index int               `json:"index"`
			Data  map[string]string `json:"data"`
		}
		var result []indexedRow
		for i, row := range s.Rows {
			result = append(result, indexedRow{Index: i, Data: row})
		}
		if result == nil {
			result = []indexedRow{}
		}
		data, _ := json.Marshal(map[string]any{
			"sheet":   sheetName,
			"columns": s.Columns,
			"rows":    result,
		})
		textResult(id, string(data))

	case "read_row":
		sheetName := args["sheet"]
		rowStr := args["row"]
		if sheetName == "" || rowStr == "" {
			respondError(id, -32602, "sheet and row are required")
			return
		}
		s, ok := sheets[sheetName]
		if !ok {
			textResult(id, fmt.Sprintf("sheet %q not found", sheetName))
			return
		}
		rowIdx, err := strconv.Atoi(rowStr)
		if err != nil || rowIdx < 0 || rowIdx >= len(s.Rows) {
			textResult(id, fmt.Sprintf("row index %s out of range (0-%d)", rowStr, len(s.Rows)-1))
			return
		}
		data, _ := json.Marshal(s.Rows[rowIdx])
		textResult(id, string(data))

	case "update_cell":
		sheetName := args["sheet"]
		rowStr := args["row"]
		column := args["column"]
		value := args["value"]
		if sheetName == "" || rowStr == "" || column == "" {
			respondError(id, -32602, "sheet, row, column are required")
			return
		}
		s, ok := sheets[sheetName]
		if !ok {
			textResult(id, fmt.Sprintf("sheet %q not found", sheetName))
			return
		}
		rowIdx, err := strconv.Atoi(rowStr)
		if err != nil || rowIdx < 0 || rowIdx >= len(s.Rows) {
			textResult(id, fmt.Sprintf("row index %s out of range (0-%d)", rowStr, len(s.Rows)-1))
			return
		}
		s.Rows[rowIdx][column] = value
		save()
		textResult(id, fmt.Sprintf("updated [%s] row %d column %q = %q", sheetName, rowIdx, column, value))

	case "append_row":
		sheetName := args["sheet"]
		rowJSON := args["data"]
		if sheetName == "" || rowJSON == "" {
			respondError(id, -32602, "sheet and data are required")
			return
		}
		s, ok := sheets[sheetName]
		if !ok {
			textResult(id, fmt.Sprintf("sheet %q not found", sheetName))
			return
		}
		var row map[string]string
		if err := json.Unmarshal([]byte(rowJSON), &row); err != nil {
			respondError(id, -32602, "data must be a JSON object of column→value")
			return
		}
		s.Rows = append(s.Rows, row)
		save()
		textResult(id, fmt.Sprintf("appended row %d to %q", len(s.Rows)-1, sheetName))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("SHEETS_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	sheets = make(map[string]*Sheet)
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
				"serverInfo":     map[string]string{"name": "sheets", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_sheets",
						"description": "List all available spreadsheets by name.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "read_sheet",
						"description": "Read all rows from a spreadsheet. Returns columns and rows with index numbers.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"sheet": map[string]string{"type": "string", "description": "Sheet name"},
							},
							"required": []string{"sheet"},
						},
					},
					{
						"name":        "read_row",
						"description": "Read a single row by index from a spreadsheet.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"sheet": map[string]string{"type": "string", "description": "Sheet name"},
								"row":   map[string]string{"type": "string", "description": "Row index (0-based)"},
							},
							"required": []string{"sheet", "row"},
						},
					},
					{
						"name":        "update_cell",
						"description": "Update a specific cell in a spreadsheet row.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"sheet":  map[string]string{"type": "string", "description": "Sheet name"},
								"row":    map[string]string{"type": "string", "description": "Row index (0-based)"},
								"column": map[string]string{"type": "string", "description": "Column name"},
								"value":  map[string]string{"type": "string", "description": "New value"},
							},
							"required": []string{"sheet", "row", "column", "value"},
						},
					},
					{
						"name":        "append_row",
						"description": "Append a new row to a spreadsheet.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"sheet": map[string]string{"type": "string", "description": "Sheet name"},
								"data":  map[string]string{"type": "string", "description": "JSON object of column→value pairs"},
							},
							"required": []string{"sheet", "data"},
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
