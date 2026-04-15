// MCP server for home automation cameras: list, snapshot URLs, AI
// scene descriptions (simulating a vision model), motion events,
// recording control.
//
// Scene descriptions are not produced by a vision model — the scenario
// framework pre-seeds them in HOME_DATA_DIR/scenes.json keyed by camera
// id, and describe_scene() returns whichever description is currently
// set. This lets us test agent decision-making on vision outputs
// deterministically without calling a real vision API.
//
// State in HOME_DATA_DIR: home.json (cameras section), scenes.json
// (camera_id → description), recordings.jsonl (append-only),
// audit.jsonl.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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

type Camera struct {
	ID        string `json:"id"`
	Room      string `json:"room"`
	StreamURL string `json:"stream_url"`
}

type HomeState struct {
	Sensors []any    `json:"sensors"`
	Cameras []Camera `json:"cameras"`
}

type AuditEntry struct {
	Time string            `json:"time"`
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

type RecordingEvent struct {
	Time     string `json:"time"`
	CameraID string `json:"camera_id"`
	Action   string `json:"action"` // "start" | "stop"
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

func loadScenes() map[string]string {
	data, err := os.ReadFile(filepath.Join(dataDir, "scenes.json"))
	if err != nil {
		return nil
	}
	var scenes map[string]string
	json.Unmarshal(data, &scenes)
	return scenes
}

func appendRecording(camID, action string) {
	entry := RecordingEvent{Time: time.Now().UTC().Format(time.RFC3339), CameraID: camID, Action: action}
	data, _ := json.Marshal(entry)
	f, _ := os.OpenFile(filepath.Join(dataDir, "recordings.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(string(data) + "\n")
		f.Close()
	}
}

func handleToolCall(id int64, name string, args map[string]string) {
	audit(name, args)

	switch name {
	case "list_cameras":
		state := loadState()
		data, _ := json.Marshal(state.Cameras)
		textResult(id, string(data))

	case "get_snapshot":
		camID := args["id"]
		if camID == "" {
			respondError(id, -32602, "id required")
			return
		}
		state := loadState()
		var cam *Camera
		for i := range state.Cameras {
			if state.Cameras[i].ID == camID {
				cam = &state.Cameras[i]
				break
			}
		}
		if cam == nil {
			respondError(id, -32602, "unknown camera "+camID)
			return
		}
		snapshot := map[string]any{
			"camera_id":    cam.ID,
			"room":         cam.Room,
			"snapshot_url": fmt.Sprintf("https://mock-cdn/%s-%s.jpg", cam.ID, time.Now().UTC().Format("20060102T150405")),
			"captured_at":  time.Now().UTC().Format(time.RFC3339),
		}
		data, _ := json.Marshal(snapshot)
		textResult(id, string(data))

	case "describe_scene":
		camID := args["id"]
		if camID == "" {
			respondError(id, -32602, "id required")
			return
		}
		state := loadState()
		var cam *Camera
		for i := range state.Cameras {
			if state.Cameras[i].ID == camID {
				cam = &state.Cameras[i]
				break
			}
		}
		if cam == nil {
			respondError(id, -32602, "unknown camera "+camID)
			return
		}
		scenes := loadScenes()
		desc := scenes[camID]
		if desc == "" {
			desc = "Empty " + cam.Room + " — no activity detected."
		}
		data, _ := json.Marshal(map[string]any{
			"camera_id":   cam.ID,
			"room":        cam.Room,
			"description": desc,
			"analyzed_at": time.Now().UTC().Format(time.RFC3339),
		})
		textResult(id, string(data))

	case "start_recording":
		camID := args["id"]
		if camID == "" {
			respondError(id, -32602, "id required")
			return
		}
		appendRecording(camID, "start")
		textResult(id, fmt.Sprintf("Recording started for %s", camID))

	case "stop_recording":
		camID := args["id"]
		if camID == "" {
			respondError(id, -32602, "id required")
			return
		}
		appendRecording(camID, "stop")
		textResult(id, fmt.Sprintf("Recording stopped for %s", camID))

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
				"serverInfo":      map[string]string{"name": "home_cameras", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "list_cameras",
						"description": "List every camera in the house with id, room, and stream URL. Call this once at startup to learn the camera topology.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_snapshot",
						"description": "Grab a still image from a camera by id. Returns a snapshot URL that can be included in notifications. Use together with describe_scene for vision-driven decisions.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Camera id from list_cameras"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "describe_scene",
						"description": "Run AI vision analysis on a camera's current view. Returns a natural-language description of what's in the frame (people, objects, activity). Use this when you need to decide whether motion or a doorbell press is legitimate — e.g. 'person in delivery uniform holding a package' vs 'unknown person wearing a mask'. This is your main tool for identifying visitors and intruders.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Camera id from list_cameras"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "start_recording",
						"description": "Begin recording video from a camera. Use during security events so there's a record to review later.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "stop_recording",
						"description": "Stop an active recording on a camera.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string"},
							},
							"required": []string{"id"},
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
