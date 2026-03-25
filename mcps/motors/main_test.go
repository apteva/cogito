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
	bin := filepath.Join(t.TempDir(), "mcp-motors")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "ROBOT_DATA_DIR="+dataDir)
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

func writeJSON(t *testing.T, dir, name string, v any) {
	t.Helper()
	data, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(filepath.Join(dir, name), data, 0644)
}

func readWorld(t *testing.T, dir string) World {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
	var w World
	json.Unmarshal(data, &w)
	return w
}

func TestMove_Forward(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
	})
	c := startServer(t, dir)
	result := c.callTool(t, "move", map[string]string{"direction": "forward"})
	if !strings.Contains(result, "OK") {
		t.Errorf("expected OK, got: %s", result)
	}

	w := readWorld(t, dir)
	if w.Position.Y != 1.0 {
		t.Errorf("expected Y=1.0, got %.1f", w.Position.Y)
	}
	if w.Battery != 99 {
		t.Errorf("expected battery 99, got %d", w.Battery)
	}
	t.Logf("After move: pos=(%.1f,%.1f) battery=%d", w.Position.X, w.Position.Y, w.Battery)
}

func TestMove_Blocked(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position:  Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
		Obstacles: []Position{{X: 0, Y: 0.8}},
	})
	c := startServer(t, dir)
	result := c.callTool(t, "move", map[string]string{"direction": "forward"})
	if !strings.Contains(result, "BLOCKED") {
		t.Errorf("expected BLOCKED, got: %s", result)
	}

	w := readWorld(t, dir)
	if w.Position.Y != 0 {
		t.Errorf("should not have moved, got Y=%.1f", w.Position.Y)
	}
}

func TestTurn(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
	})
	c := startServer(t, dir)

	// Turn right 90
	result := c.callTool(t, "turn", map[string]string{"direction": "right", "degrees": "90"})
	if !strings.Contains(result, "OK") {
		t.Errorf("expected OK, got: %s", result)
	}
	w := readWorld(t, dir)
	if w.Heading != 90 {
		t.Errorf("expected heading 90, got %.1f", w.Heading)
	}

	// Turn left 45
	c.callTool(t, "turn", map[string]string{"direction": "left", "degrees": "45"})
	w = readWorld(t, dir)
	if w.Heading != 45 {
		t.Errorf("expected heading 45, got %.1f", w.Heading)
	}
}

func TestStop(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100, Moving: true, Speed: "fast",
	})
	c := startServer(t, dir)
	result := c.callTool(t, "stop", map[string]string{})
	if !strings.Contains(result, "OK") {
		t.Errorf("expected OK, got: %s", result)
	}
	w := readWorld(t, dir)
	if w.Moving {
		t.Error("expected stopped")
	}
}

func TestGetStatus(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 3, Y: 5}, Heading: 45, Battery: 72, Moving: true, Speed: "normal",
	})
	c := startServer(t, dir)
	result := c.callTool(t, "get_status", map[string]string{})
	if !strings.Contains(result, "moving") || !strings.Contains(result, "72") {
		t.Errorf("expected status with moving and battery, got: %s", result)
	}
	t.Logf("Status: %s", result)
}

func TestNavigate_MultipleSteps(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
	})
	c := startServer(t, dir)

	// Move forward 3 times
	c.callTool(t, "move", map[string]string{"direction": "forward"})
	c.callTool(t, "move", map[string]string{"direction": "forward"})
	c.callTool(t, "move", map[string]string{"direction": "forward"})

	w := readWorld(t, dir)
	if w.Position.Y != 3.0 {
		t.Errorf("expected Y=3.0, got %.1f", w.Position.Y)
	}
	if w.Battery != 97 {
		t.Errorf("expected battery 97, got %d", w.Battery)
	}

	// Turn east, move forward
	c.callTool(t, "turn", map[string]string{"direction": "right"})
	c.callTool(t, "move", map[string]string{"direction": "forward"})

	w = readWorld(t, dir)
	if w.Position.X != 1.0 || w.Position.Y != 3.0 {
		t.Errorf("expected (1,3), got (%.1f,%.1f)", w.Position.X, w.Position.Y)
	}
	t.Logf("Final position: (%.1f, %.1f) heading=%.0f battery=%d",
		w.Position.X, w.Position.Y, w.Heading, w.Battery)
}
