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
	bin := filepath.Join(t.TempDir(), "mcp-sensors")
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

func TestReadSensors_ClearPath(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
	})
	c := startServer(t, dir)
	result := c.callTool(t, "read_sensors", map[string]string{})
	var reading SensorReading
	json.Unmarshal([]byte(result), &reading)

	if reading.Battery != 100 {
		t.Errorf("expected battery 100, got %d", reading.Battery)
	}
	if reading.ObstacleAhead {
		t.Error("expected no obstacle ahead")
	}
	if reading.DistFront < 100 {
		t.Errorf("expected large front distance, got %.1f", reading.DistFront)
	}
	t.Logf("Sensors: %+v", reading)
}

func TestReadSensors_ObstacleAhead(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position:  Position{X: 0, Y: 0}, Heading: 0, Battery: 85,
		Obstacles: []Position{{X: 0, Y: 1.5}},
	})
	c := startServer(t, dir)
	result := c.callTool(t, "read_sensors", map[string]string{})
	var reading SensorReading
	json.Unmarshal([]byte(result), &reading)

	if !reading.ObstacleAhead {
		t.Error("expected obstacle detected ahead")
	}
	if reading.DistFront > 2.0 {
		t.Errorf("expected short front distance, got %.1f", reading.DistFront)
	}
	t.Logf("Sensors with obstacle: %+v", reading)
}

func TestReadCamera_NoObjects(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
	})
	c := startServer(t, dir)
	result := c.callTool(t, "read_camera", map[string]string{})
	var reading CameraReading
	json.Unmarshal([]byte(result), &reading)

	if len(reading.Objects) != 0 {
		t.Errorf("expected 0 objects, got %d", len(reading.Objects))
	}
}

func TestReadCamera_ObjectsVisible(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 100,
		Objects: []Object{
			{Name: "red cup", X: 2, Y: 4},
			{Name: "chair", X: 100, Y: 100}, // out of range
		},
	})
	c := startServer(t, dir)
	result := c.callTool(t, "read_camera", map[string]string{})
	var reading CameraReading
	json.Unmarshal([]byte(result), &reading)

	if len(reading.Objects) != 1 {
		t.Fatalf("expected 1 visible object, got %d", len(reading.Objects))
	}
	if reading.Objects[0].Name != "red cup" {
		t.Errorf("expected red cup, got %s", reading.Objects[0].Name)
	}
	t.Logf("Camera: %+v", reading)
}

func TestReadSensors_LowBattery(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, "world.json", World{
		Position: Position{X: 0, Y: 0}, Heading: 0, Battery: 10,
	})
	c := startServer(t, dir)
	result := c.callTool(t, "read_sensors", map[string]string{})
	var reading SensorReading
	json.Unmarshal([]byte(result), &reading)

	if reading.Battery != 10 {
		t.Errorf("expected battery 10, got %d", reading.Battery)
	}
}
