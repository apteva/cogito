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
	bin := filepath.Join(t.TempDir(), "mcp-webscraper")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "SCRAPER_DATA_DIR="+dataDir)
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

func seedSites(t *testing.T, dir string) {
	t.Helper()
	data, _ := json.MarshalIndent(map[string]*SiteInfo{
		"https://acme.com": {
			Title:       "Acme Corp",
			Description: "Industrial solutions provider",
			Body:        "We build robots.",
			Industry:    "Industrial Automation",
			Employees:   "500-1000",
			Location:    "San Francisco, CA",
			Founded:     "2015",
		},
		"https://globex.io": {
			Title:       "Globex Analytics",
			Description: "AI-powered analytics",
			Body:        "Data-driven decisions.",
			Industry:    "SaaS",
			Employees:   "50-100",
			Location:    "Austin, TX",
			Founded:     "2020",
		},
	}, "", "  ")
	os.WriteFile(filepath.Join(dir, "sites.json"), data, 0644)
}

func TestFetchPage(t *testing.T) {
	dir := t.TempDir()
	seedSites(t, dir)
	c := startServer(t, dir)
	result := c.callTool(t, "fetch_page", map[string]string{"url": "https://acme.com"})
	if !strings.Contains(result, "Acme Corp") {
		t.Errorf("expected Acme Corp title, got: %s", result)
	}
	if !strings.Contains(result, "Industrial solutions") {
		t.Errorf("expected description, got: %s", result)
	}
}

func TestFetchPage_NotFound(t *testing.T) {
	dir := t.TempDir()
	seedSites(t, dir)
	c := startServer(t, dir)
	result := c.callTool(t, "fetch_page", map[string]string{"url": "https://unknown.com"})
	if !strings.Contains(result, "ERROR") {
		t.Errorf("expected error for unknown URL, got: %s", result)
	}
}

func TestExtractInfo(t *testing.T) {
	dir := t.TempDir()
	seedSites(t, dir)
	c := startServer(t, dir)
	result := c.callTool(t, "extract_info", map[string]string{"url": "https://globex.io"})
	if !strings.Contains(result, "SaaS") {
		t.Errorf("expected SaaS industry, got: %s", result)
	}
	if !strings.Contains(result, "Austin") {
		t.Errorf("expected Austin location, got: %s", result)
	}
	if !strings.Contains(result, "50-100") {
		t.Errorf("expected 50-100 employees, got: %s", result)
	}
}

func TestExtractInfo_URLNormalization(t *testing.T) {
	dir := t.TempDir()
	seedSites(t, dir)
	c := startServer(t, dir)
	// Try with trailing slash
	result := c.callTool(t, "extract_info", map[string]string{"url": "https://acme.com/"})
	if !strings.Contains(result, "Industrial Automation") {
		t.Errorf("expected match with trailing slash, got: %s", result)
	}
	// Try without https
	result = c.callTool(t, "extract_info", map[string]string{"url": "http://acme.com"})
	if !strings.Contains(result, "Industrial Automation") {
		t.Errorf("expected match with http://, got: %s", result)
	}
}
