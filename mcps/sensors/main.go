// MCP server for robot sensor readings.
// Reads world state from ROBOT_DATA_DIR/world.json.
// All tool calls appended to audit.jsonl.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	Position  Position `json:"position"`
	Heading   float64  `json:"heading"` // degrees, 0=north, 90=east
	Battery   int      `json:"battery"` // percentage
	Obstacles []Position `json:"obstacles"`
	Objects   []Object `json:"objects"`
}

type SensorReading struct {
	Position      Position `json:"position"`
	Heading       float64  `json:"heading"`
	Battery       int      `json:"battery_pct"`
	DistFront     float64  `json:"distance_front"`
	DistLeft      float64  `json:"distance_left"`
	DistRight     float64  `json:"distance_right"`
	ObstacleAhead bool     `json:"obstacle_detected"`
}

type CameraReading struct {
	Objects []CameraObject `json:"objects"`
}

type CameraObject struct {
	Name     string  `json:"name"`
	Distance float64 `json:"distance"`
	Angle    float64 `json:"angle"` // relative to heading
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

func distance(a, b Position) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return math.Sqrt(dx*dx + dy*dy)
}

// distanceInDirection returns the distance to the nearest obstacle in a given direction.
// direction is in degrees (0=north, 90=east). Returns 999 if nothing within 10 units.
func distanceInDirection(w World, dirDeg float64) float64 {
	dirRad := dirDeg * math.Pi / 180
	dx := math.Sin(dirRad)
	dy := math.Cos(dirRad)

	minDist := 999.0
	for _, obs := range w.Obstacles {
		// Project obstacle onto direction vector
		ox := obs.X - w.Position.X
		oy := obs.Y - w.Position.Y
		proj := ox*dx + oy*dy
		if proj > 0 && proj < minDist {
			// Check perpendicular distance (within 0.5 unit corridor)
			perp := math.Abs(ox*dy - oy*dx)
			if perp < 0.8 {
				minDist := proj
				return minDist
			}
		}
	}
	return minDist
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)
	time.Sleep(100 * time.Millisecond) // simulate sensor read delay

	w := loadWorld()

	switch name {
	case "read_sensors":
		distFront := distanceInDirection(w, w.Heading)
		distLeft := distanceInDirection(w, w.Heading-90)
		distRight := distanceInDirection(w, w.Heading+90)

		reading := SensorReading{
			Position:      w.Position,
			Heading:       w.Heading,
			Battery:       w.Battery,
			DistFront:     math.Round(distFront*10) / 10,
			DistLeft:      math.Round(distLeft*10) / 10,
			DistRight:     math.Round(distRight*10) / 10,
			ObstacleAhead: distFront < 2.0,
		}
		data, _ := json.Marshal(reading)
		textResult(id, string(data))

	case "read_camera":
		var visible []CameraObject
		for _, obj := range w.Objects {
			dist := distance(w.Position, Position{obj.X, obj.Y})
			if dist <= 10.0 { // camera range
				// Calculate angle relative to heading
				dx := obj.X - w.Position.X
				dy := obj.Y - w.Position.Y
				angleDeg := math.Atan2(dx, dy)*180/math.Pi - w.Heading
				// Normalize to -180..180
				for angleDeg > 180 {
					angleDeg -= 360
				}
				for angleDeg < -180 {
					angleDeg += 360
				}
				visible = append(visible, CameraObject{
					Name:     obj.Name,
					Distance: math.Round(dist*10) / 10,
					Angle:    math.Round(angleDeg*10) / 10,
				})
			}
		}
		reading := CameraReading{Objects: visible}
		if reading.Objects == nil {
			reading.Objects = []CameraObject{}
		}
		data, _ := json.Marshal(reading)
		textResult(id, string(data))

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
				"serverInfo":     map[string]string{"name": "sensors", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "read_sensors",
						"description": "Read all robot sensors. Returns position (x,y), heading (degrees, 0=north), battery percentage, distance to nearest obstacle in front/left/right, and whether an obstacle is dangerously close ahead.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "read_camera",
						"description": "Read the robot's camera. Returns a list of visible objects with their name, distance, and angle relative to the robot's heading.",
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
