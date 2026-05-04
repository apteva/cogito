package scenarios

import (
	"testing"
	"time"

	. "github.com/apteva/core"
)

var helpdeskScenario = Scenario{
	Name: "Helpdesk",
	Directive: `You run the support desk for my small business.
We have a helpdesk ticketing system — check it every 10-20 seconds for new tickets.
When tickets come in, look up the answer in our knowledge base, reply to the customer, and close the ticket.
Don't let more than 3 tickets be handled at the same time.`,
	MCPServers: []MCPServerConfig{{
		Name:    "helpdesk",
		Command: "", // filled in test
		Env:     map[string]string{"HELPDESK_DATA_DIR": "{{dataDir}}"},
	}},
	DataSetup: func(t *testing.T, dir string) {
		WriteJSONFile(t, dir, "kb.json", map[string]string{
			"hours":    "We are open Monday to Friday, 9am to 5pm.",
			"delivery": "We deliver within 10 miles for free.",
			"returns":  "You can return items within 30 days with a receipt.",
		})
		WriteJSONFile(t, dir, "tickets.json", []any{})
	},
	Phases: []Phase{
		{
			Name:    "Startup — thread spawned and list_tickets called",
			Timeout: 60 * time.Second,
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				if th.Threads().Count() == 0 {
					return false
				}
				entries := ReadAuditEntries(dir)
				lists := CountTool(entries, "list_tickets")
				t.Logf("  ... list_tickets=%d threads=%v", lists, ThreadIDs(th))
				return lists > 0
			},
		},
		{
			Name:    "Process 2 tickets",
			Timeout: 90 * time.Second,
			Setup: func(t *testing.T, dir string) {
				WriteJSONFile(t, dir, "tickets.json", []map[string]string{
					{"id": "t1", "question": "What are your hours?"},
					{"id": "t2", "question": "Do you deliver?"},
				})
			},
			Wait: func(t *testing.T, dir string, th *Thinker) bool {
				entries := ReadAuditEntries(dir)
				replies := CountTool(entries, "reply_ticket")
				closes := CountTool(entries, "close_ticket")
				t.Logf("  ... lookup=%d replies=%d closes=%d threads=%v",
					CountTool(entries, "lookup_kb"), replies, closes, ThreadIDs(th))
				return replies >= 2 && closes >= 2
			},
			Verify: func(t *testing.T, dir string, th *Thinker) {
				entries := ReadAuditEntries(dir)
				t.Logf("Audit log (%d entries):", len(entries))
				for _, e := range entries {
					t.Logf("  %s %v", e.Tool, e.Args)
				}
				lookups := CountTool(entries, "lookup_kb")
				if lookups < 2 {
					t.Logf("NOTE: lookup_kb called %d times (LLM may have answered without KB)", lookups)
				}
			},
		},
		{
			Name:    "Quiescence — workers done",
			Timeout: 45 * time.Second,
			// Quiescence = "no new MCP tool calls for 15s." Earlier this
			// asserted threads<=1, but Kimi sometimes spawns ad-hoc
			// per-ticket workers (a valid alternative strategy) and
			// keeps them alive in `pace sleep` after the work finishes.
			// The audit log going idle is the real signal: the agent
			// is done acting on the world, regardless of how many
			// reasoning threads are paced/sleeping in the background.
			Wait: (func() func(t *testing.T, dir string, th *Thinker) bool {
				lastCount := -1
				var stableSince time.Time
				return func(t *testing.T, dir string, th *Thinker) bool {
					entries := ReadAuditEntries(dir)
					count := len(entries)
					if count != lastCount {
						lastCount = count
						stableSince = time.Now()
						t.Logf("  ... audit=%d (changed) threads=%d %v",
							count, th.Threads().Count(), ThreadIDs(th))
						return false
					}
					elapsed := time.Since(stableSince).Round(time.Second)
					t.Logf("  ... audit=%d (stable %s) threads=%d",
						count, elapsed, th.Threads().Count())
					return elapsed >= 15*time.Second
				}
			})(),
		},
	},
	Timeout:    3 * time.Minute,
	MaxThreads: 5,
}

func TestScenario_Helpdesk(t *testing.T) {
	bin := BuildMCPBinary(t, "mcps/helpdesk")
	t.Logf("built mcp-helpdesk: %s", bin)

	s := helpdeskScenario
	s.MCPServers[0].Command = bin
	RunScenario(t, s)
}
