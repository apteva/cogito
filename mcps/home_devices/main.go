// MCP server for home automation devices: lights, thermostat, locks,
// announcement speakers, scene presets. Owns the "change the physical
// world" side of the house.
//
// State in HOME_DATA_DIR: home.json (lights/thermostat/locks sections),
// notifications.jsonl (append-only — where owner notifications land),
// audit.jsonl.
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

type Light struct {
	ID         string `json:"id"`
	Room       string `json:"room"`
	On         bool   `json:"on"`
	Brightness int    `json:"brightness"` // 0-100
}

type Thermostat struct {
	ID        string  `json:"id"`
	Room      string  `json:"room"`
	CurrentC  float64 `json:"current_c"`
	SetpointC float64 `json:"setpoint_c"`
	Mode      string  `json:"mode"` // heat | cool | off
}

type Lock struct {
	ID     string `json:"id"`
	Door   string `json:"door"`
	Locked bool   `json:"locked"`
}

type HomeState struct {
	Lights      []Light      `json:"lights"`
	Thermostats []Thermostat `json:"thermostats"`
	Locks       []Lock       `json:"locks"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

type Notification struct {
	Time    string `json:"time"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Level   string `json:"level"`
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

// saveState merges the devices slice back into the full home.json (which
// contains sections owned by other MCPs we must not clobber).
func saveState(state HomeState) {
	path := filepath.Join(dataDir, "home.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var full map[string]any
	json.Unmarshal(data, &full)
	full["lights"] = state.Lights
	full["thermostats"] = state.Thermostats
	full["locks"] = state.Locks
	out, _ := json.MarshalIndent(full, "", "  ")
	os.WriteFile(path, out, 0644)
}

func appendNotification(n Notification) {
	data, _ := json.Marshal(n)
	f, _ := os.OpenFile(filepath.Join(dataDir, "notifications.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "list_devices":
		state := loadState()
		out := map[string]any{
			"lights":      state.Lights,
			"thermostats": state.Thermostats,
			"locks":       state.Locks,
		}
		data, _ := json.Marshal(out)
		textResult(id, string(data))

	case "set_light":
		lightID := args["id"]
		if lightID == "" {
			respondError(id, -32602, "id required")
			return
		}
		state := loadState()
		var light *Light
		for i := range state.Lights {
			if state.Lights[i].ID == lightID {
				light = &state.Lights[i]
				break
			}
		}
		if light == nil {
			respondError(id, -32602, "unknown light "+lightID)
			return
		}
		if onStr, ok := args["on"]; ok {
			light.On = onStr == "true" || onStr == "1"
		}
		if b := args["brightness"]; b != "" {
			if n, err := strconv.Atoi(b); err == nil {
				if n < 0 {
					n = 0
				}
				if n > 100 {
					n = 100
				}
				light.Brightness = n
				light.On = n > 0
			}
		}
		saveState(state)
		textResult(id, fmt.Sprintf("Light %s (%s) set: on=%v brightness=%d", light.ID, light.Room, light.On, light.Brightness))

	case "set_thermostat":
		tID := args["id"]
		if tID == "" {
			tID = "main"
		}
		state := loadState()
		var t *Thermostat
		for i := range state.Thermostats {
			if state.Thermostats[i].ID == tID {
				t = &state.Thermostats[i]
				break
			}
		}
		if t == nil {
			respondError(id, -32602, "unknown thermostat "+tID)
			return
		}
		if s := args["setpoint_c"]; s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				t.SetpointC = f
			}
		}
		if m := args["mode"]; m != "" {
			t.Mode = m
		}
		saveState(state)
		textResult(id, fmt.Sprintf("Thermostat %s set: setpoint=%.1fC mode=%s", t.ID, t.SetpointC, t.Mode))

	case "lock":
		doorID := args["id"]
		if doorID == "" {
			respondError(id, -32602, "id required")
			return
		}
		state := loadState()
		for i := range state.Locks {
			if state.Locks[i].ID == doorID {
				state.Locks[i].Locked = true
				saveState(state)
				textResult(id, fmt.Sprintf("Door %s locked", doorID))
				return
			}
		}
		respondError(id, -32602, "unknown lock "+doorID)

	case "unlock":
		doorID := args["id"]
		if doorID == "" {
			respondError(id, -32602, "id required")
			return
		}
		state := loadState()
		for i := range state.Locks {
			if state.Locks[i].ID == doorID {
				state.Locks[i].Locked = false
				saveState(state)
				textResult(id, fmt.Sprintf("Door %s unlocked", doorID))
				return
			}
		}
		respondError(id, -32602, "unknown lock "+doorID)

	case "announce":
		msg := args["message"]
		room := args["room"]
		if msg == "" {
			respondError(id, -32602, "message required")
			return
		}
		if room == "" {
			room = "all"
		}
		textResult(id, fmt.Sprintf("Announced in %s: %q", room, msg))

	case "notify_owner":
		title := args["title"]
		msg := args["message"]
		level := args["level"]
		if msg == "" {
			respondError(id, -32602, "message required")
			return
		}
		if title == "" {
			title = "Home alert"
		}
		if level == "" {
			level = "info"
		}
		appendNotification(Notification{
			Time:    time.Now().UTC().Format(time.RFC3339),
			Title:   title,
			Message: msg,
			Level:   level,
		})
		textResult(id, fmt.Sprintf("Notified owner: [%s] %s — %s", level, title, msg))

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
				"serverInfo":      map[string]string{"name": "home_devices", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_devices",
						"description": "Return every device in the house (lights, thermostats, locks) with current state. Call once at startup to learn the device topology.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "set_light",
						"description": "Turn a light on/off or adjust its brightness. Pass brightness 0-100; 0 turns it off, anything >0 turns it on at that level.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":         map[string]string{"type": "string", "description": "Light id from list_devices"},
								"on":         map[string]string{"type": "boolean", "description": "On/off (optional if brightness set)"},
								"brightness": map[string]string{"type": "integer", "description": "0-100"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "set_thermostat",
						"description": "Change a thermostat's setpoint or mode. Use Celsius for setpoint_c.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":         map[string]string{"type": "string", "description": "Thermostat id (default 'main')"},
								"setpoint_c": map[string]string{"type": "number", "description": "Target temperature in Celsius"},
								"mode":       map[string]string{"type": "string", "description": "heat | cool | off"},
							},
						},
					},
					{
						"name":        "lock",
						"description": "Lock a door by id. Use for bedtime routines and when leaving the house.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Lock id (front, back, garage)"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "unlock",
						"description": "Unlock a door by id. Used for expected visitors, morning routine, or when the owner is returning.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "announce",
						"description": "Play a voice announcement in a specific room via the smart speakers. Use 'all' to broadcast house-wide.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"message": map[string]string{"type": "string"},
								"room":    map[string]string{"type": "string", "description": "Room name or 'all'"},
							},
							"required": []string{"message"},
						},
					},
					{
						"name":        "notify_owner",
						"description": "Send a push notification to the homeowner's phone. Use for security events, denied visitors, and anything the owner needs to know while they're away. Pass level=alert for urgent events (intruder, smoke) so their phone bypasses do-not-disturb.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"title":   map[string]string{"type": "string", "description": "Notification title"},
								"message": map[string]string{"type": "string", "description": "Notification body"},
								"level":   map[string]string{"type": "string", "description": "info | warn | alert"},
							},
							"required": []string{"message"},
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
