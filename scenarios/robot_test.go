package scenarios

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/apteva/core"
)

var robotScenario = Scenario{
	Name: "Robot",
	Directive: `You control a small robot. Spawn two team members:
1. A "pilot" thread at fast pace with small model — it continuously reads sensors and drives the motors.
   When it detects obstacles, it stops and reports to you. It executes movement commands you give it.
2. You (main) are the strategic planner. You decide where the robot should go and what to look for.
   Give the pilot high-level commands like "move forward 3 steps" or "turn right and scan".

The pilot must stay at fast pace and continuously monitor sensors between moves.
You stay at normal pace and coordinate.`,
	MCPServers: []MCPServerConfig{
		{
			Name:    "sensors",
			Command: "", // filled in test
			Env:     map[string]string{"ROBOT_DATA_DIR": "{{dataDir}}"},
		},
		{
			Name:    "motors",
			Command: "", // filled in test
			Env:     map[string]string{"ROBOT_DATA_DIR": "{{dataDir}}"},
		},
	},
	DataSetup: func(t *testing.T, dir string) {
		WriteJSONFile(t, dir, "world.json", map[string]any{
			"position":  map[string]float64{"x": 0, "y": 0},
			"heading":   0,
			"battery":   100,
			"obstacles": []any{},
			"objects":   []any{},
			"moving":    false,
			"speed":     "",
		})
	},
	Phases: []Phase{
		{
			Name:    "Startup — pilot spawned and reading sensors",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.Threads().Count() == 0 {
					return false
				}
				entries := ReadAuditEntries(dir)
				reads := CountTool(entries, "read_sensors")
				t.Logf("  ... read_sensors=%d threads=%v", reads, ThreadIDs(th))
				return reads > 0
			},
		},
		{
			Name:    "Navigate — move forward 3 steps",
			Timeout: 90 * time.Second,
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("Command: move the robot forward 3 steps")
					}
					entries := ReadAuditEntries(dir)
					moves := CountTool(entries, "move")
					t.Logf("  ... moves=%d threads=%v", moves, ThreadIDs(th))
					return moves >= 3
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				// Check position changed
				data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
				var w map[string]any
				json.Unmarshal(data, &w)
				pos := w["position"].(map[string]any)
				y := pos["y"].(float64)
				t.Logf("Position after moves: y=%.1f", y)
				if y < 2.0 {
					t.Logf("NOTE: expected Y >= 2.0, got %.1f", y)
				}
			},
		},
		{
			Name:    "Obstacle — robot detects and avoids",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Place obstacle ahead of current position
				data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
				var w map[string]any
				json.Unmarshal(data, &w)
				pos := w["position"].(map[string]any)
				y := pos["y"].(float64)
				w["obstacles"] = []map[string]float64{{"x": 0, "y": y + 1.5}}
				WriteJSONFile(t, dir, "world.json", w)
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					// Tell pilot to move forward once — it should detect the obstacle
					if !sent {
						sent = true
						th.InjectConsole("Command: move forward 2 more steps")
					}
					entries := ReadAuditEntries(dir)
					reads := CountTool(entries, "read_sensors")
					stops := CountTool(entries, "stop")
					moves := CountTool(entries, "move")
					t.Logf("  ... reads=%d stops=%d moves=%d threads=%v", reads, stops, moves, ThreadIDs(th))
					// Pilot should detect obstacle via sensors or blocked move, then stop or turn
					return stops > 0 || (moves > 3 && reads > 5)
				}
			}(),
		},
		{
			Name:    "Camera — find the red cup",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				// Place red cup in camera range
				data, _ := os.ReadFile(filepath.Join(dir, "world.json"))
				var w map[string]any
				json.Unmarshal(data, &w)
				pos := w["position"].(map[string]any)
				x := pos["x"].(float64)
				y := pos["y"].(float64)
				w["objects"] = []map[string]any{
					{"name": "red cup", "x": x + 2, "y": y + 3},
				}
				WriteJSONFile(t, dir, "world.json", w)
			},
			Wait: func() func(*testing.T, string, *Thinker) bool {
				sent := false
				return func(t *testing.T, dir string, th *Thinker) bool {
					if !sent {
						sent = true
						th.InjectConsole("Command: use the camera to look for a red cup nearby")
					}
					entries := ReadAuditEntries(dir)
					cams := CountTool(entries, "read_camera")
					t.Logf("  ... read_camera=%d threads=%v", cams, ThreadIDs(th))
					return cams >= 1
				}
			}(),
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := ReadAuditEntries(dir)
				t.Logf("Audit (%d entries):", len(entries))
				for _, e := range entries {
					if e.Tool != "read_sensors" {
						t.Logf("  %s %v", e.Tool, e.Args)
					}
				}
			},
		},
		{
			Name:    "Quiescence — pilot still alive",
			Timeout: 15 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				return th.Threads().Count() >= 1
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				if th.Threads().Count() < 1 {
					t.Errorf("expected pilot still alive, got %d threads", th.Threads().Count())
				}
			},
		},
	},
	Timeout:    5 * time.Minute,
	MaxThreads: 3,
}

func TestScenario_Robot(t *testing.T) {
	sensorsBin := BuildMCPBinary(t, "mcps/sensors")
	motorsBin := BuildMCPBinary(t, "mcps/motors")
	t.Logf("built sensors: %s, motors: %s", sensorsBin, motorsBin)

	s := robotScenario
	s.MCPServers[0].Command = sensorsBin
	s.MCPServers[1].Command = motorsBin
	RunScenario(t, s)
}
