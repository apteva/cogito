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

// mutate runs fn under an advisory file lock on sheets.json so concurrent
// stdio subprocesses (one per thread worker) can't clobber each other's
// cell updates.
//
// Without this, the classic last-writer-wins race loses any edit that lands
// between a different subprocess's load() and save(): each subprocess has
// its own in-memory `sheets` map bootstrapped from disk, and the last one
// to save() overwrites everyone else. We saw exactly this in the
// AutonomousSheetEnrichment scenario — rows that had correct update_cell
// calls in the audit were empty in the final sheets.json because parallel
// workers clobbered each other.
//
// Flow per call:
//  1. Open sheets.json (create empty if missing).
//  2. Take an exclusive flock — blocks if another subprocess is mid-write.
//  3. Re-read sheets.json into the in-memory map so we see the latest
//     state (including edits from other subprocesses).
//  4. Run the mutator (apply the cell edit).
//  5. Write the full map back.
//  6. Release the flock.
func mutate(fn func()) {
	path := filepath.Join(dataDir, "sheets.json")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		// Fall back to in-memory mutation — better than crashing, but
		// will lose writes if concurrent.
		fn()
		return
	}
	defer f.Close()

	// Exclusive advisory lock for the whole read-modify-write.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		fn()
		return
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Refresh from disk so we see writes from other subprocesses.
	data, _ := os.ReadFile(path)
	if len(data) > 0 {
		fresh := map[string]*Sheet{}
		if json.Unmarshal(data, &fresh) == nil {
			sheets = fresh
		}
	}

	fn()

	out, _ := json.MarshalIndent(sheets, "", "  ")
	// Truncate and rewrite from the start so we don't leave stale trailing
	// bytes if the new payload is shorter than the old one.
	f.Truncate(0)
	f.Seek(0, 0)
	f.Write(out)
	f.Sync()
}

func save() {
	data, _ := json.MarshalIndent(sheets, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "sheets.json"), data, 0644)
}

// refresh reloads sheets.json under a shared lock so readers always see the
// latest state written by other subprocesses. Cheap enough to run on every
// tool call in the test fake.
func refresh() {
	path := filepath.Join(dataDir, "sheets.json")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err == nil {
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}
	fresh := map[string]*Sheet{}
	if json.Unmarshal(data, &fresh) == nil {
		sheets = fresh
	}
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)
	refresh()

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
		rowIdx, convErr := strconv.Atoi(rowStr)
		if convErr != nil {
			textResult(id, fmt.Sprintf("row index %s is not a number", rowStr))
			return
		}
		var resultMsg string
		var ok2 bool
		mutate(func() {
			s, ok := sheets[sheetName]
			if !ok {
				resultMsg = fmt.Sprintf("sheet %q not found", sheetName)
				return
			}
			if rowIdx < 0 || rowIdx >= len(s.Rows) {
				resultMsg = fmt.Sprintf("row index %d out of range (0-%d)", rowIdx, len(s.Rows)-1)
				return
			}
			s.Rows[rowIdx][column] = value
			resultMsg = fmt.Sprintf("updated [%s] row %d column %q = %q", sheetName, rowIdx, column, value)
			ok2 = true
		})
		_ = ok2
		textResult(id, resultMsg)

	case "append_row":
		sheetName := args["sheet"]
		rowJSON := args["data"]
		if sheetName == "" || rowJSON == "" {
			respondError(id, -32602, "sheet and data are required")
			return
		}
		var row map[string]string
		if err := json.Unmarshal([]byte(rowJSON), &row); err != nil {
			respondError(id, -32602, "data must be a JSON object of column→value")
			return
		}
		var resultMsg string
		mutate(func() {
			s, ok := sheets[sheetName]
			if !ok {
				resultMsg = fmt.Sprintf("sheet %q not found", sheetName)
				return
			}
			s.Rows = append(s.Rows, row)
			resultMsg = fmt.Sprintf("appended row %d to %q", len(s.Rows)-1, sheetName)
		})
		textResult(id, resultMsg)

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
