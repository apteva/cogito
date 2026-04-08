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
	bin := filepath.Join(t.TempDir(), "mcp-crm")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "CRM_DATA_DIR="+dataDir)
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

func TestCreateContact(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	result := c.callTool(t, "create_contact", map[string]string{
		"name": "Alice Smith", "email": "alice@test.com", "company": "Acme", "website": "https://acme.com",
	})
	if !strings.Contains(result, "c-001") {
		t.Errorf("expected contact ID c-001, got: %s", result)
	}
	if !strings.Contains(result, "alice@test.com") {
		t.Errorf("expected email in result, got: %s", result)
	}
}

func TestCreateContact_DuplicateEmail(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	c.callTool(t, "create_contact", map[string]string{"name": "Alice", "email": "alice@test.com"})
	result := c.callTool(t, "create_contact", map[string]string{"name": "Alice2", "email": "alice@test.com"})
	if !strings.Contains(result, "already exists") {
		t.Errorf("expected duplicate rejection, got: %s", result)
	}
}

func TestGetContact(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	c.callTool(t, "create_contact", map[string]string{"name": "Bob", "email": "bob@test.com"})
	result := c.callTool(t, "get_contact", map[string]string{"id": "c-001"})
	if !strings.Contains(result, "Bob") {
		t.Errorf("expected Bob, got: %s", result)
	}
}

func TestUpdateContact(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	c.callTool(t, "create_contact", map[string]string{"name": "Carol", "email": "carol@test.com"})
	c.callTool(t, "update_contact", map[string]string{
		"id": "c-001", "industry": "Tech", "location": "NYC", "status": "enriched",
	})
	result := c.callTool(t, "get_contact", map[string]string{"id": "c-001"})
	if !strings.Contains(result, "Tech") || !strings.Contains(result, "NYC") || !strings.Contains(result, "enriched") {
		t.Errorf("expected updated fields, got: %s", result)
	}
}

func TestListContacts(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	c.callTool(t, "create_contact", map[string]string{"name": "A", "email": "a@t.com"})
	c.callTool(t, "create_contact", map[string]string{"name": "B", "email": "b@t.com"})
	c.callTool(t, "update_contact", map[string]string{"id": "c-001", "status": "enriched"})

	// All
	result := c.callTool(t, "list_contacts", map[string]string{})
	var all []Contact
	json.Unmarshal([]byte(result), &all)
	if len(all) != 2 {
		t.Errorf("expected 2 contacts, got %d", len(all))
	}

	// Filter
	result = c.callTool(t, "list_contacts", map[string]string{"status": "new"})
	var newOnly []Contact
	json.Unmarshal([]byte(result), &newOnly)
	if len(newOnly) != 1 {
		t.Errorf("expected 1 new contact, got %d", len(newOnly))
	}
}

func TestSearchContacts(t *testing.T) {
	dir := t.TempDir()
	c := startServer(t, dir)
	c.callTool(t, "create_contact", map[string]string{"name": "Alice", "email": "alice@acme.com", "company": "Acme Corp"})
	c.callTool(t, "create_contact", map[string]string{"name": "Bob", "email": "bob@globex.io", "company": "Globex"})

	result := c.callTool(t, "search_contacts", map[string]string{"query": "acme"})
	if !strings.Contains(result, "alice@acme.com") {
		t.Errorf("expected alice in acme search, got: %s", result)
	}
	if strings.Contains(result, "bob@globex.io") {
		t.Errorf("bob should not appear in acme search")
	}
}
