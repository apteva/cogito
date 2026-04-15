// MCP server for home automation sensors: motion, door/window contacts,
// temperature probes. Reads state from HOME_DATA_DIR/home.json and events
// from HOME_DATA_DIR/sensor_events.jsonl. The scenario framework appends
// to sensor_events.jsonl between phases to inject motion/contact events
// the agent has to react to.
//
// State in HOME_DATA_DIR: home.json (shared with cameras/intercom/devices),
// sensor_events.jsonl (append-only), audit.jsonl (this server's tool call log).
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

// Sensor is a single physical sensor in the house.
type Sensor struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // "motion" | "door" | "window" | "temperature"
	Room    string `json:"room"`
	Enabled bool   `json:"enabled"`
}

// HomeState is the shared world state — same shape read by all four
// home_* MCP servers. Each server only consumes its own section.
type HomeState struct {
	Sensors   []Sensor       `json:"sensors"`
	Cameras   []any          `json:"cameras"`
	Devices   []any          `json:"devices"`
	Locks     []any          `json:"locks"`
	Occupancy map[string]any `json:"occupancy"`
	Presence  map[string]any `json:"presence"`
	Weather   map[string]any `json:"weather"`
	Visits    []any          `json:"visits"`
}

// SensorEvent is one line in sensor_events.jsonl.
type SensorEvent struct {
	Time     string `json:"time"`
	SensorID string `json:"sensor_id"`
	Type     string `json:"type"`
	Room     string `json:"room"`
	Value    string `json:"value"` // "triggered" | "open" | "closed" | raw reading
	Detail   string `json:"detail,omitempty"`
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
	entry := AuditEntry{Time: time.Now().UTC().Format(time.RFC3339), Tool: tool, Args: args}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func loadState() HomeState {
	data, err := os.ReadFile(filepath.Join(dataDir, "home.json"))
	if err != nil {
		return HomeState{}
	}
	var s HomeState
	json.Unmarshal(data, &s)
	return s
}

func loadSensorEvents() []SensorEvent {
	f, err := os.Open(filepath.Join(dataDir, "sensor_events.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var events []SensorEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var e SensorEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err == nil {
			events = append(events, e)
		}
	}
	return events
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "list_sensors":
		state := loadState()
		data, _ := json.Marshal(state.Sensors)
		textResult(id, string(data))

	case "read_sensor":
		sensorID := args["id"]
		if sensorID == "" {
			respondError(id, -32602, "id required")
			return
		}
		state := loadState()
		events := loadSensorEvents()
		// Find the most recent event for this sensor.
		var latest *SensorEvent
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].SensorID == sensorID {
				latest = &events[i]
				break
			}
		}
		var sensor *Sensor
		for i := range state.Sensors {
			if state.Sensors[i].ID == sensorID {
				sensor = &state.Sensors[i]
				break
			}
		}
		if sensor == nil {
			respondError(id, -32602, "unknown sensor "+sensorID)
			return
		}
		out := map[string]any{
			"id":      sensor.ID,
			"type":    sensor.Type,
			"room":    sensor.Room,
			"enabled": sensor.Enabled,
		}
		if latest != nil {
			out["last_event"] = latest
		} else {
			out["last_event"] = nil
		}
		data, _ := json.Marshal(out)
		textResult(id, string(data))

	case "read_all":
		state := loadState()
		events := loadSensorEvents()
		// Group latest event per sensor.
		latest := map[string]SensorEvent{}
		for _, e := range events {
			latest[e.SensorID] = e
		}
		var rows []map[string]any
		for _, s := range state.Sensors {
			row := map[string]any{
				"id":      s.ID,
				"type":    s.Type,
				"room":    s.Room,
				"enabled": s.Enabled,
			}
			if e, ok := latest[s.ID]; ok {
				row["last_event"] = e
			}
			rows = append(rows, row)
		}
		data, _ := json.Marshal(rows)
		textResult(id, string(data))

	case "get_events":
		since := args["since"]
		limit := 50
		if l := args["limit"]; l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		events := loadSensorEvents()
		var filtered []SensorEvent
		for _, e := range events {
			if since != "" && e.Time <= since {
				continue
			}
			filtered = append(filtered, e)
		}
		// Return the most recent N.
		if len(filtered) > limit {
			filtered = filtered[len(filtered)-limit:]
		}
		data, _ := json.Marshal(filtered)
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("HOME_DATA_DIR")
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
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "home_sensors", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_sensors",
						"description": "List every sensor in the house with id, type (motion/door/window/temperature), room, and enabled state. Call this once at startup to learn the sensor topology.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "read_sensor",
						"description": "Read the current state and most recent event for a specific sensor by id. Use after get_events returns a sensor id to confirm the latest reading.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Sensor id"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "read_all",
						"description": "Bulk read every sensor's current state and latest event. Heavier than read_sensor; use at startup or for a full house snapshot.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_events",
						"description": "Poll for sensor events (motion triggered, door opened, window opened, temperature threshold). Returns the event log since an optional `since` timestamp. The agent should call this on each wake to see what happened while it slept.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"since": map[string]string{"type": "string", "description": "ISO8601 timestamp to filter events after. Omit for recent events."},
								"limit": map[string]string{"type": "integer", "description": "Max events to return (default 50)."},
							},
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
