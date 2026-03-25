// MCP server for robot motor control.
// Reads/writes world state in ROBOT_DATA_DIR/world.json.
// All tool calls appended to audit.jsonl.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
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

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Object struct {
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
}

type World struct {
	Position  Position   `json:"position"`
	Heading   float64    `json:"heading"`
	Battery   int        `json:"battery"`
	Obstacles []Position `json:"obstacles"`
	Objects   []Object   `json:"objects"`
	Moving    bool       `json:"moving"`
	Speed     string     `json:"speed"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

var dataDir string

func respond(id int64, result any) {
	data, _ := json.Marshal(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
	fmt.Println(string(data))
}

func respondError(id int64, code int, msg string) {
	data, _ := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &struct{ Code int `json:"code"`; Message string `json:"message"` }{code, msg},
	})
	fmt.Println(string(data))
}

func textResult(id int64, text string) {
	respond(id, map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

func audit(tool string, args map[string]string) {
	entry := AuditEntry{Time: time.Now().UTC().Format(time.RFC3339), Tool: tool, Args: args}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func loadWorld() World {
	data, err := os.ReadFile(filepath.Join(dataDir, "world.json"))
	if err != nil {
		return World{Battery: 100}
	}
	var w World
	json.Unmarshal(data, &w)
	return w
}

func saveWorld(w World) {
	data, _ := json.MarshalIndent(w, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "world.json"), data, 0644)
}

func distanceTo(a, b Position) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return math.Sqrt(dx*dx + dy*dy)
}

// checkObstacleAhead returns true if an obstacle is within 1 unit in the movement direction.
func checkObstacleAhead(w World, dx, dy float64) bool {
	newPos := Position{X: w.Position.X + dx, Y: w.Position.Y + dy}
	for _, obs := range w.Obstacles {
		if distanceTo(newPos, obs) < 0.8 {
			return true
		}
	}
	return false
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	w := loadWorld()

	switch name {
	case "move":
		direction := args["direction"]
		speed := args["speed"]
		if direction == "" {
			direction = "forward"
		}
		if speed == "" {
			speed = "normal"
		}

		// Calculate movement step based on speed
		step := 1.0
		switch speed {
		case "slow":
			step = 0.5
		case "fast":
			step = 2.0
		}

		headingRad := w.Heading * math.Pi / 180
		dx := math.Sin(headingRad) * step
		dy := math.Cos(headingRad) * step

		if direction == "backward" {
			dx = -dx
			dy = -dy
		}

		// Check for obstacle
		if checkObstacleAhead(w, dx, dy) {
			w.Moving = false
			w.Speed = ""
			saveWorld(w)
			time.Sleep(200 * time.Millisecond)
			textResult(id, "BLOCKED: obstacle in path, cannot move")
			return
		}

		// Move
		w.Position.X = math.Round((w.Position.X+dx)*10) / 10
		w.Position.Y = math.Round((w.Position.Y+dy)*10) / 10
		w.Moving = true
		w.Speed = speed
		w.Battery-- // drain 1% per move
		if w.Battery < 0 {
			w.Battery = 0
		}
		saveWorld(w)

		time.Sleep(300 * time.Millisecond) // simulate movement time
		textResult(id, fmt.Sprintf("OK: moved %s at %s speed. Position now (%.1f, %.1f). Battery: %d%%",
			direction, speed, w.Position.X, w.Position.Y, w.Battery))

	case "turn":
		direction := args["direction"]
		degreesStr := args["degrees"]
		if direction == "" {
			respondError(id, -32602, "direction is required (left or right)")
			return
		}
		degrees := 90.0
		if degreesStr != "" {
			d, err := strconv.ParseFloat(degreesStr, 64)
			if err == nil && d > 0 {
				degrees = d
			}
		}

		if direction == "left" {
			w.Heading -= degrees
		} else {
			w.Heading += degrees
		}
		// Normalize to 0-360
		for w.Heading < 0 {
			w.Heading += 360
		}
		for w.Heading >= 360 {
			w.Heading -= 360
		}
		w.Heading = math.Round(w.Heading*10) / 10
		saveWorld(w)

		time.Sleep(200 * time.Millisecond) // simulate turn time
		textResult(id, fmt.Sprintf("OK: turned %s %.0f degrees. Heading now %.1f", direction, degrees, w.Heading))

	case "stop":
		w.Moving = false
		w.Speed = ""
		saveWorld(w)
		textResult(id, "OK: stopped")

	case "get_status":
		status := "stopped"
		if w.Moving {
			status = fmt.Sprintf("moving at %s speed", w.Speed)
		}
		textResult(id, fmt.Sprintf("Status: %s, heading: %.1f, position: (%.1f, %.1f), battery: %d%%",
			status, w.Heading, w.Position.X, w.Position.Y, w.Battery))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("ROBOT_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
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
				"serverInfo":     map[string]string{"name": "motors", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "move",
						"description": "Move the robot forward or backward. Returns BLOCKED if obstacle in path. Each move drains 1% battery.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"direction": map[string]string{"type": "string", "description": "forward or backward (default: forward)"},
								"speed":     map[string]string{"type": "string", "description": "slow (0.5 unit), normal (1 unit), or fast (2 units) per step"},
							},
						},
					},
					{
						"name":        "turn",
						"description": "Turn the robot left or right by a given number of degrees.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"direction": map[string]string{"type": "string", "description": "left or right"},
								"degrees":   map[string]string{"type": "string", "description": "Degrees to turn (default: 90)"},
							},
							"required": []string{"direction"},
						},
					},
					{
						"name":        "stop",
						"description": "Emergency stop. Immediately halts all movement.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_status",
						"description": "Get current motor status: moving/stopped, speed, heading, position, battery.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
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
