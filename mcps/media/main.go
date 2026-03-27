// MCP server for video/media file management (simulated).
// State in MEDIA_DATA_DIR: files.json, assets.json
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type MediaFile struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"` // video, image, audio
	Duration   string `json:"duration,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Size       string `json:"size"`
	Status     string `json:"status"` // uploaded, processing, ready
	UploadedAt string `json:"uploaded_at"`
}

type Asset struct {
	ID       string `json:"id"`
	FileID   string `json:"file_id"`
	Type     string `json:"type"` // screenshot, reel, thumbnail
	Name     string `json:"name"`
	URL      string `json:"url"`
	Duration string `json:"duration,omitempty"`
	CreateAt string `json:"created_at"`
}

var (
	dataDir string
	files   []MediaFile
	assets  []Asset
	nextID  int
)

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

func genID() string {
	nextID++
	return fmt.Sprintf("m%d", nextID)
}

func save() {
	data, _ := json.MarshalIndent(map[string]any{"files": files, "assets": assets}, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "media.json"), data, 0644)
}

func load() {
	data, err := os.ReadFile(filepath.Join(dataDir, "media.json"))
	if err != nil {
		return
	}
	var state struct {
		Files  []MediaFile `json:"files"`
		Assets []Asset     `json:"assets"`
	}
	json.Unmarshal(data, &state)
	files = state.Files
	assets = state.Assets
}

func inferType(name string) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".mov") || strings.HasSuffix(lower, ".avi") || strings.HasSuffix(lower, ".webm") {
		return "video"
	}
	if strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".gif") {
		return "image"
	}
	if strings.HasSuffix(lower, ".mp3") || strings.HasSuffix(lower, ".wav") {
		return "audio"
	}
	return "video" // default
}

func handleToolCall(id int64, name string, args map[string]string) {
	switch name {
	case "upload_file":
		fileName := args["name"]
		if fileName == "" {
			respondError(id, -32602, "name is required")
			return
		}
		duration := args["duration"]
		if duration == "" {
			duration = "3:24"
		}
		resolution := args["resolution"]
		if resolution == "" {
			resolution = "1920x1080"
		}
		size := args["size"]
		if size == "" {
			size = "245MB"
		}

		f := MediaFile{
			ID:         genID(),
			Name:       fileName,
			Type:       inferType(fileName),
			Duration:   duration,
			Resolution: resolution,
			Size:       size,
			Status:     "uploaded",
			UploadedAt: time.Now().UTC().Format(time.RFC3339),
		}
		files = append(files, f)
		save()
		data, _ := json.Marshal(f)
		textResult(id, string(data))

	case "list_files":
		data, _ := json.Marshal(files)
		textResult(id, string(data))

	case "get_file":
		fileID := args["id"]
		for _, f := range files {
			if f.ID == fileID {
				data, _ := json.Marshal(f)
				textResult(id, string(data))
				return
			}
		}
		respondError(id, -32602, "file not found: "+fileID)

	case "extract_screenshots":
		fileID := args["file_id"]
		count := args["count"]
		if count == "" {
			count = "3"
		}

		// Find file
		var file *MediaFile
		for i := range files {
			if files[i].ID == fileID {
				file = &files[i]
				break
			}
		}
		if file == nil {
			respondError(id, -32602, "file not found: "+fileID)
			return
		}

		time.Sleep(500 * time.Millisecond) // simulate processing

		n := 3
		fmt.Sscanf(count, "%d", &n)
		var created []Asset
		for i := 0; i < n; i++ {
			a := Asset{
				ID:       genID(),
				FileID:   fileID,
				Type:     "screenshot",
				Name:     fmt.Sprintf("%s_screenshot_%d.jpg", strings.TrimSuffix(file.Name, filepath.Ext(file.Name)), i+1),
				URL:      fmt.Sprintf("https://cdn.media.fake/screenshots/%s_%d.jpg", fileID, i+1),
				CreateAt: time.Now().UTC().Format(time.RFC3339),
			}
			assets = append(assets, a)
			created = append(created, a)
		}

		file.Status = "ready"
		save()
		data, _ := json.Marshal(created)
		textResult(id, string(data))

	case "create_reel":
		fileID := args["file_id"]
		startTime := args["start"]
		duration := args["duration"]
		reelName := args["name"]

		if startTime == "" {
			startTime = "0:00"
		}
		if duration == "" {
			duration = "0:30"
		}

		var file *MediaFile
		for i := range files {
			if files[i].ID == fileID {
				file = &files[i]
				break
			}
		}
		if file == nil {
			respondError(id, -32602, "file not found: "+fileID)
			return
		}

		if reelName == "" {
			reelName = fmt.Sprintf("%s_reel.mp4", strings.TrimSuffix(file.Name, filepath.Ext(file.Name)))
		}

		time.Sleep(800 * time.Millisecond) // simulate processing

		a := Asset{
			ID:       genID(),
			FileID:   fileID,
			Type:     "reel",
			Name:     reelName,
			URL:      fmt.Sprintf("https://cdn.media.fake/reels/%s_reel.mp4", fileID),
			Duration: duration,
			CreateAt: time.Now().UTC().Format(time.RFC3339),
		}
		assets = append(assets, a)
		save()
		data, _ := json.Marshal(a)
		textResult(id, string(data))

	case "get_assets":
		fileID := args["file_id"]
		var result []Asset
		for _, a := range assets {
			if fileID == "" || a.FileID == fileID {
				result = append(result, a)
			}
		}
		data, _ := json.Marshal(result)
		textResult(id, string(data))

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func main() {
	dataDir = os.Getenv("MEDIA_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	load()

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
				"serverInfo":     map[string]string{"name": "media", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "upload_file",
						"description": "Upload/register a new media file. Returns file metadata with ID.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":       map[string]string{"type": "string", "description": "File name (e.g. product-demo.mp4)"},
								"duration":   map[string]string{"type": "string", "description": "Duration (e.g. 3:24). Optional."},
								"resolution": map[string]string{"type": "string", "description": "Resolution (e.g. 1920x1080). Optional."},
								"size":       map[string]string{"type": "string", "description": "File size (e.g. 245MB). Optional."},
							},
							"required": []string{"name"},
						},
					},
					{
						"name":        "list_files",
						"description": "List all uploaded media files.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_file",
						"description": "Get details of a specific media file by ID.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "File ID"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "extract_screenshots",
						"description": "Extract screenshot thumbnails from a video file. Returns list of generated screenshot assets.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"file_id": map[string]string{"type": "string", "description": "File ID to extract from"},
								"count":   map[string]string{"type": "string", "description": "Number of screenshots (default 3)"},
							},
							"required": []string{"file_id"},
						},
					},
					{
						"name":        "create_reel",
						"description": "Create a short reel/clip from a video file. Returns the reel asset with URL.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"file_id":  map[string]string{"type": "string", "description": "Source file ID"},
								"start":    map[string]string{"type": "string", "description": "Start time (e.g. 0:15). Default 0:00"},
								"duration": map[string]string{"type": "string", "description": "Reel duration (e.g. 0:30). Default 0:30"},
								"name":     map[string]string{"type": "string", "description": "Output file name. Optional."},
							},
							"required": []string{"file_id"},
						},
					},
					{
						"name":        "get_assets",
						"description": "List generated assets (screenshots, reels) optionally filtered by source file.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"file_id": map[string]string{"type": "string", "description": "Filter by source file ID. Omit for all."},
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
