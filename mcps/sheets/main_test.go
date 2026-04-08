package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type mcpClient struct {
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	nextID  int64
}

func startServer(t *testing.T, dataDir string) *mcpClient {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mcp-sheets")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "SHEETS_DATA_DIR="+dataDir)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })

	c := &mcpClient{stdin: stdin, scanner: bufio.NewScanner(stdout)}
	c.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	c.call(t, "initialize", map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "test", "version": "1.0.0"},
	})
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	fmt.Fprintf(stdin, "%s\n", data)
	return c
}

func (c *mcpClient) call(t *testing.T, method string, params any) jsonRPCResponse {
	t.Helper()
	c.nextID++
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	fmt.Fprintf(c.stdin, "%s\n", data)
	if !c.scanner.Scan() {
		t.Fatal("no response")
	}
	var resp jsonRPCResponse
	json.Unmarshal([]byte(c.scanner.Text()), &resp)
	return resp
}

func (c *mcpClient) callTool(t *testing.T, name string, args map[string]string) string {
	t.Helper()
	resp := c.call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		t.Fatalf("tool %s error: %s", name, resp.Error.Message)
	}
	raw, _ := json.Marshal(resp.Result)
	var result struct {
		Content []struct{ Type, Text string } `json:"content"`
	}
	json.Unmarshal(raw, &result)
	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func seedSheet(t *testing.T, dir string) {
	t.Helper()
	data, _ := json.MarshalIndent(map[string]*Sheet{
		"Leads": {
			Columns: []string{"name", "email", "status"},
			Rows: []map[string]string{
				{"name": "Alice", "email": "alice@test.com", "status": "new"},
				{"name": "Bob", "email": "bob@test.com", "status": "new"},
			},
		},
	}, "", "  ")
	os.WriteFile(filepath.Join(dir, "sheets.json"), data, 0644)
}

func TestListSheets(t *testing.T) {
	dir := t.TempDir()
	seedSheet(t, dir)
	c := startServer(t, dir)
	result := c.callTool(t, "list_sheets", map[string]string{})
	if !strings.Contains(result, "Leads") {
		t.Errorf("expected Leads in result, got: %s", result)
	}
}

func TestReadSheet(t *testing.T) {
	dir := t.TempDir()
	seedSheet(t, dir)
	c := startServer(t, dir)
	result := c.callTool(t, "read_sheet", map[string]string{"sheet": "Leads"})
	if !strings.Contains(result, "alice@test.com") {
		t.Errorf("expected alice@test.com, got: %s", result)
	}
	if !strings.Contains(result, "bob@test.com") {
		t.Errorf("expected bob@test.com, got: %s", result)
	}
}

func TestReadRow(t *testing.T) {
	dir := t.TempDir()
	seedSheet(t, dir)
	c := startServer(t, dir)
	result := c.callTool(t, "read_row", map[string]string{"sheet": "Leads", "row": "1"})
	if !strings.Contains(result, "Bob") {
		t.Errorf("expected Bob in row 1, got: %s", result)
	}
}

func TestUpdateCell(t *testing.T) {
	dir := t.TempDir()
	seedSheet(t, dir)
	c := startServer(t, dir)
	c.callTool(t, "update_cell", map[string]string{"sheet": "Leads", "row": "0", "column": "status", "value": "enriched"})

	// Verify
	result := c.callTool(t, "read_row", map[string]string{"sheet": "Leads", "row": "0"})
	if !strings.Contains(result, "enriched") {
		t.Errorf("expected enriched status, got: %s", result)
	}
}

func TestAppendRow(t *testing.T) {
	dir := t.TempDir()
	seedSheet(t, dir)
	c := startServer(t, dir)
	rowData, _ := json.Marshal(map[string]string{"name": "Carol", "email": "carol@test.com", "status": "new"})
	c.callTool(t, "append_row", map[string]string{"sheet": "Leads", "data": string(rowData)})

	result := c.callTool(t, "read_sheet", map[string]string{"sheet": "Leads"})
	if !strings.Contains(result, "carol@test.com") {
		t.Errorf("expected carol@test.com after append, got: %s", result)
	}
}

func TestReadSheet_NotFound(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	result := c.callTool(t, "read_sheet", map[string]string{"sheet": "Nonexistent"})
	if !strings.Contains(result, "not found") {
		t.Errorf("expected not found, got: %s", result)
	}
}
