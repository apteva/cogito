// MCP server for the RubricLearning scenario.
//
// Exposes a sales-call grading rubric, a small set of pre-rated training
// transcripts, and a held-out test set with hidden ground truth. The
// agent's job is to read the rubric + training calls, internalize the
// patterns into memory + directive, then rate the test calls. The server
// scores each submission against the hidden ground truth and returns the
// per-dimension delta so the agent (and the test harness) can see how it
// did.
//
// State:
//   {{dataDir}}/submissions.jsonl  — one row per submit_rating call
//
// All training data + ground truth are baked into the binary so the
// scenario is hermetic.
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

// ─── JSON-RPC plumbing ─────────────────────────────────────────────────

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

// ─── Domain model ──────────────────────────────────────────────────────

type Ratings struct {
	Discovery  int `json:"discovery"`   // depth of needs/pain discovery
	Objection  int `json:"objection"`   // handling pushback / concerns
	NextSteps  int `json:"next_steps"`  // explicit, time-bound next action
	Pricing    int `json:"pricing"`     // clarity around pricing & value
	Energy     int `json:"energy"`      // tone, engagement, momentum
}

type LabeledCall struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Transcript string  `json:"transcript"`
	Ratings    Ratings `json:"ratings"`
	Notes      string  `json:"notes,omitempty"`
}

type TestCall struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Transcript string  `json:"transcript"`
}

// Hidden ground truth — never exposed via tools/list or get_*. Only
// referenced inside submit_rating to compute the delta.
var groundTruth = map[string]Ratings{
	"test-1": {Discovery: 5, Objection: 4, NextSteps: 5, Pricing: 4, Energy: 4},
	"test-2": {Discovery: 1, Objection: 1, NextSteps: 1, Pricing: 2, Energy: 2},
	"test-3": {Discovery: 3, Objection: 4, NextSteps: 2, Pricing: 3, Energy: 3},
}

// ─── Rubric ────────────────────────────────────────────────────────────

const rubricText = `SALES CALL QA RUBRIC — 5 dimensions, each rated 1-5.

DIMENSION 1: discovery — depth and quality of needs/pain discovery
  5 = rep asks 4+ open questions, surfaces concrete pain points (numbers,
      timelines, named blockers), confirms back what they heard
  4 = 2-3 good open questions, surfaces at least one concrete pain
  3 = some questions but mostly closed/yes-no, only vague pain
  2 = mostly pitches, asks 1 question max
  1 = pitches the entire call with no discovery whatsoever

DIMENSION 2: objection — how the rep handles pushback or concerns
  5 = acknowledges the concern, asks clarifying question, addresses it
      with a concrete example or proof point, confirms it's resolved
  4 = acknowledges + addresses but doesn't confirm resolution
  3 = addresses without acknowledging — feels dismissive
  2 = brushes off the objection or argues
  1 = no objection raised, OR argues with the prospect

DIMENSION 3: next_steps — clarity of the next action
  5 = specific named meeting at a specific date/time, agreed by both
      parties, with a stated agenda or deliverable
  4 = specific date for next contact but no agreed agenda
  3 = vague "I'll follow up next week" with no commitment from prospect
  2 = "let me know when you're ready" — no date at all
  1 = call ends with no mention of next contact

DIMENSION 4: pricing — clarity and confidence around pricing & value
  5 = pricing stated clearly with at least one ROI / value anchor;
      tied to the prospect's discovered pain
  4 = pricing stated clearly, generic value framing
  3 = pricing mentioned but vague ("starts at..." without commitment)
  2 = avoids pricing or defers to "let me send you a quote"
  1 = pricing not mentioned at all

DIMENSION 5: energy — tone, engagement, momentum
  5 = high energy on both sides, prospect is leaning in, rep matches pace
  4 = engaged, professional, no awkward silences
  3 = polite but flat — feels transactional
  2 = noticeable disengagement on one side, awkward gaps
  1 = openly disengaged, prospect distracted or trying to get off the call

A "good call" averages 4+. A "needs work" call averages 2.5 or below.`

// ─── Training data ─────────────────────────────────────────────────────

var trainingCalls = []LabeledCall{
	{
		ID:    "train-1",
		Title: "Acme — first discovery call",
		Transcript: `REP: Hi Sarah, thanks for taking the time. Before I jump into anything about us, I'd love to understand what made you book this — what's actually broken right now?
PROSPECT: Sure. We're spending about 15 hours a week reconciling spreadsheets between our CRM and our billing system. It's killing our ops team.
REP: 15 hours a week — is that one person or split across the team?
PROSPECT: Two people, mostly. And our CFO is starting to notice the cost.
REP: Got it. And when you say killing them — what does that actually look like? Are deals slipping, or is it more morale?
PROSPECT: Both. Last quarter we missed two renewals because the renewal date was wrong in the CRM.
REP: Two missed renewals — what was the lost revenue on those?
PROSPECT: About $80k combined.
REP: OK. So just to play it back: 30 hours a week of manual ops, $80k of leaked revenue last quarter, and your CFO is paying attention. Did I get that right?
PROSPECT: That's exactly it.
REP: That's the kind of thing we fix end-to-end. Our platform connects directly to both systems. Pricing-wise, for a team your size we're looking at $1,200/month — which against $80k of leakage in one quarter pays for itself in week one.
PROSPECT: That tracks. Send me a contract?
REP: Better — let's get your CFO on a 30-minute call together Thursday at 2pm. I'll prepare a side-by-side of your current state vs ours and we can sign live if it makes sense. Does Thursday 2pm work?
PROSPECT: Thursday 2pm works.`,
		Ratings: Ratings{Discovery: 5, Objection: 4, NextSteps: 5, Pricing: 5, Energy: 5},
		Notes:   "Textbook. 4+ open questions, concrete numbers surfaced, clear ROI tie, named follow-up.",
	},
	{
		ID:    "train-2",
		Title: "Globex — pure pitch, no discovery",
		Transcript: `REP: Hi! Thanks for hopping on. I'm super excited to walk you through our platform today. So we're an AI-powered analytics tool, we have over 500 customers, we've raised $40M in funding, and Gartner just named us a Cool Vendor. Let me share my screen.
PROSPECT: Sure.
REP: OK so here's our dashboard. As you can see we've got real-time charts, custom reports, alerts. We integrate with everything. And we have machine learning. Any questions?
PROSPECT: Uh, what does it cost?
REP: It depends on your needs. I can send you a quote. Should I email you?
PROSPECT: Sure, I guess.
REP: Great! I'll send something over. Talk soon!`,
		Ratings: Ratings{Discovery: 1, Objection: 1, NextSteps: 2, Pricing: 2, Energy: 2},
		Notes:   "Zero discovery. No real next step. Pricing dodged. Prospect clearly disengaged.",
	},
	{
		ID:    "train-3",
		Title: "Initech — partial pitch, weak handling",
		Transcript: `REP: Hi Mark, thanks for the meeting. Quick background — we help mid-market SaaS companies cut churn by about 30%. What's your current churn rate?
PROSPECT: Around 8% annual.
REP: And what are you doing about it today?
PROSPECT: Honestly nothing systematic. We try to call the at-risk accounts but it's ad-hoc.
REP: Right, so most companies your size are in the same boat. We have a churn-prediction engine that flags accounts 60 days before they cancel. Pricing is $2k/month. When can we get started?
PROSPECT: Honestly $2k feels steep for something we're not sure works.
REP: I get that, but our customers see ROI in the first quarter. Trust me, it pays for itself. So can I send the contract?
PROSPECT: I need to think about it. Let me get back to you next week.
REP: OK, I'll follow up next Wednesday.`,
		Ratings: Ratings{Discovery: 3, Objection: 2, NextSteps: 3, Pricing: 4, Energy: 3},
		Notes:   "Two questions but no real depth. Objection brushed aside with 'trust me'. Vague next step.",
	},
	{
		ID:    "train-4",
		Title: "Hooli — strong discovery, weak close",
		Transcript: `REP: Hi Priya, thanks for the time. Before I tell you anything about us, can you walk me through how your team handles customer feedback today?
PROSPECT: We use Intercom for support tickets, Typeform for NPS surveys, and a Google Sheet to manually tag everything.
REP: And the manual tagging — how long does that take?
PROSPECT: My CSM lead spends about 6 hours a week on it.
REP: 6 hours a week. What does she do with the tags once they're in there?
PROSPECT: Tries to spot trends, mostly. She presents them to me monthly.
REP: Does anything actually get acted on?
PROSPECT: Honestly, sometimes. We had a packaging complaint trend last quarter and we did fix it. But it takes a month to even see it.
REP: So the gap is the speed — by the time you see a trend, it's been a problem for weeks. Is that the main pain?
PROSPECT: Yes, exactly.
REP: That's what we automate. Real-time tagging plus alerts when a trend crosses a threshold.
PROSPECT: How much?
REP: Around $800/month.
PROSPECT: OK, send me some info.
REP: Will do — I'll email you a one-pager today.`,
		Ratings: Ratings{Discovery: 5, Objection: 3, NextSteps: 2, Pricing: 4, Energy: 4},
		Notes:   "Discovery is excellent. Pricing OK. But no real next meeting — just 'send me info' which is the death of a deal.",
	},
	{
		ID:    "train-5",
		Title: "Pied Piper — average all around",
		Transcript: `REP: Hi James, thanks for joining. So we're a marketing automation platform. What are you using today?
PROSPECT: Mailchimp.
REP: How is that working out?
PROSPECT: Fine I guess. We've outgrown some of the segmentation features.
REP: Got it. We do better segmentation. Want me to demo?
PROSPECT: Sure, briefly.
REP: [demos for 5 minutes] As you can see we have audience builder, journey designer, A/B testing.
PROSPECT: Looks similar to what's out there. What's pricing like?
REP: It's tiered based on contact count. For 50k contacts you'd be around $600/month.
PROSPECT: That's higher than Mailchimp.
REP: Yes but you get a lot more. Want to start a trial?
PROSPECT: Maybe. Let me check with my team.
REP: OK, I'll follow up Friday.`,
		Ratings: Ratings{Discovery: 2, Objection: 3, NextSteps: 3, Pricing: 3, Energy: 3},
		Notes:   "Polite, transactional, mediocre everywhere. One closed question, one vague next step, defensible pricing handle.",
	},
	{
		ID:    "train-6",
		Title: "Stark — confident close, weak objection",
		Transcript: `REP: Tony, thanks for making time. Quick context first — what triggered you reaching out?
PROSPECT: We tripled headcount in 6 months and our onboarding is a mess.
REP: Tripled in 6 months — congratulations. What does the mess look like specifically?
PROSPECT: New hires don't know where anything lives. We're losing a week per hire just on orientation.
REP: A week per hire. How many hires this quarter?
PROSPECT: About 40.
REP: So 40 weeks of lost productivity this quarter alone. At your blended rate that's roughly $200k.
PROSPECT: Sounds about right.
REP: We solve exactly that. Self-serve onboarding with role-based learning paths. Most customers cut onboarding from a week to under a day.
PROSPECT: Sure but every onboarding tool says that. How is yours different?
REP: Look, I could give you a feature comparison, but the easier thing is to talk to one of our customers your size. Let me set you up with a 20-min call with Lisa at Acme — same growth profile as you. Would that be useful?
PROSPECT: Yeah, that would actually be great.
REP: I'll send the intro today and you'll have a slot on the calendar by tomorrow morning.`,
		Ratings: Ratings{Discovery: 5, Objection: 3, NextSteps: 4, Pricing: 1, Energy: 5},
		Notes:   "Killer discovery and warm intro close. But total dodge on the differentiation question, and pricing was never even raised.",
	},
}

// ─── Test data (no ratings exposed) ────────────────────────────────────

var testCalls = []TestCall{
	{
		ID:    "test-1",
		Title: "Massive Dynamic — first call",
		Transcript: `REP: Hi Olivia, thanks for the meeting. Before I tell you what we do, I'd love to hear what made you take this call.
PROSPECT: We're a 200-person engineering org and our incident response is breaking down. We had three Sev1s last month and post-mortems are taking weeks.
REP: Three Sev1s last month — what was the customer impact?
PROSPECT: One was a 4-hour outage on our checkout flow. Lost about $120k in GMV plus a bunch of trust.
REP: $120k from one incident. And the post-mortems — what's the bottleneck there?
PROSPECT: Mostly pulling logs from 6 different places and then arguing about timeline.
REP: So if I had to summarize: incident response is ad-hoc, post-mortems take weeks because of fragmented data, and a single Sev1 is costing you six figures. Is that it?
PROSPECT: That's the exact picture.
REP: That's literally what we're built for. Unified incident timeline pulled from your existing tools, automatic post-mortem drafts from the runbook execution. Most customers cut Sev1 MTTR by 60% and post-mortem turnaround from weeks to days.
PROSPECT: Cost?
REP: For an org your size, $4,500/month. Against $120k from one incident that's roughly 4% — if we prevent even one Sev1 a year you 27x the spend.
PROSPECT: Makes sense. What do we do next?
REP: Let's get your VP of Engineering and your SRE lead on a 45-minute call Tuesday at 10am Pacific. I'll prepare a walkthrough with sample data from your tech stack and we can decide together if it's worth a 14-day trial. Does Tuesday at 10 work?
PROSPECT: Tuesday at 10 works.
REP: One more thing — anything I should NOT mention on that call?
PROSPECT: Yeah, don't bring up our previous tool. It was a political mess.
REP: Got it. I'll keep it focused on the future state. Talk Tuesday.`,
	},
	{
		ID:    "test-2",
		Title: "Cyberdyne — pitch dump",
		Transcript: `REP: Hey! Thanks for jumping on. So I'm going to walk you through what we do. We're a platform that uses AI to revolutionize sales workflows. We have integrations with Salesforce, HubSpot, Pipedrive, basically everything. We've raised $60M and we're growing 200% year over year. Let me share my screen.
PROSPECT: OK.
REP: [shares screen for 8 minutes uninterrupted] So as you can see we have all these features. Pretty cool right?
PROSPECT: Yeah.
REP: Any questions?
PROSPECT: Not really.
REP: Cool, well I'll send you some info. Bye!`,
	},
	{
		ID:    "test-3",
		Title: "Wonka — middling call",
		Transcript: `REP: Hi Charlie, thanks for the time. So tell me a bit about what you're trying to solve.
PROSPECT: We're growing fast and our knowledge base is a disaster. Nobody can find anything.
REP: How big is the team?
PROSPECT: 80 engineers across 3 offices.
REP: And what are you using today?
PROSPECT: Confluence. It's just become a graveyard.
REP: Got it, that's super common. Our search is way better than Confluence's — we use semantic search across everything.
PROSPECT: We tried a semantic search add-on and it didn't really work. The relevance was bad.
REP: Yeah, I hear that a lot. Most of those tools are just bolted on after the fact. We're built ground-up for semantic retrieval and we've got a custom embedding model fine-tuned for technical content. Want me to show you?
PROSPECT: Sure.
REP: [demos for 10 minutes] Pricing is $15 per seat per month.
PROSPECT: That's $1,200 a month for us. Not crazy.
REP: Right. Want to start with a pilot?
PROSPECT: Maybe. Let me think about it and circle back.
REP: OK, I'll ping you next week.`,
	},
}

// ─── Tool dispatch ─────────────────────────────────────────────────────

var dataDir string

func handleToolCall(id int64, name string, args map[string]any) {
	switch name {
	case "get_rubric":
		textResult(id, rubricText)

	case "list_training_calls":
		// Return id + title only — agent calls get_training_call to read body.
		// Cheaper context-wise and simulates a real "list then drill in" flow.
		out := make([]map[string]string, 0, len(trainingCalls))
		for _, c := range trainingCalls {
			out = append(out, map[string]string{
				"id":    c.ID,
				"title": c.Title,
			})
		}
		j, _ := json.Marshal(out)
		textResult(id, string(j))

	case "get_training_call":
		callID, _ := args["id"].(string)
		if callID == "" {
			respondError(id, -32602, "id required")
			return
		}
		for _, c := range trainingCalls {
			if c.ID == callID {
				j, _ := json.Marshal(c)
				textResult(id, string(j))
				return
			}
		}
		textResult(id, fmt.Sprintf("training call %q not found", callID))

	case "list_test_calls":
		out := make([]map[string]string, 0, len(testCalls))
		for _, c := range testCalls {
			out = append(out, map[string]string{
				"id":    c.ID,
				"title": c.Title,
			})
		}
		j, _ := json.Marshal(out)
		textResult(id, string(j))

	case "get_test_call":
		callID, _ := args["id"].(string)
		if callID == "" {
			respondError(id, -32602, "id required")
			return
		}
		for _, c := range testCalls {
			if c.ID == callID {
				// Return the transcript only — never the ratings.
				j, _ := json.Marshal(map[string]any{
					"id":         c.ID,
					"title":      c.Title,
					"transcript": c.Transcript,
				})
				textResult(id, string(j))
				return
			}
		}
		textResult(id, fmt.Sprintf("test call %q not found", callID))

	case "submit_rating":
		callID, _ := args["call_id"].(string)
		if callID == "" {
			respondError(id, -32602, "call_id required")
			return
		}
		truth, ok := groundTruth[callID]
		if !ok {
			textResult(id, fmt.Sprintf("call %q is not in the test set", callID))
			return
		}
		got := Ratings{
			Discovery: parseInt(args["discovery"]),
			Objection: parseInt(args["objection"]),
			NextSteps: parseInt(args["next_steps"]),
			Pricing:   parseInt(args["pricing"]),
			Energy:    parseInt(args["energy"]),
		}
		// Validate range
		if !validRating(got) {
			textResult(id, "all five dimensions must be integers 1-5")
			return
		}
		// Persist the submission so the test harness can read it.
		entry := map[string]any{
			"time":         time.Now().UTC().Format(time.RFC3339),
			"call_id":      callID,
			"submitted":    got,
			"ground_truth": truth,
			"deltas": map[string]int{
				"discovery":  abs(got.Discovery - truth.Discovery),
				"objection":  abs(got.Objection - truth.Objection),
				"next_steps": abs(got.NextSteps - truth.NextSteps),
				"pricing":    abs(got.Pricing - truth.Pricing),
				"energy":     abs(got.Energy - truth.Energy),
			},
		}
		appendJSONL(filepath.Join(dataDir, "submissions.jsonl"), entry)
		// Tell the agent how it did per-dimension. Within ±1 = "match",
		// otherwise show the delta. This lets the agent see whether
		// its heuristics generalized — it can self-correct on the next call.
		feedback := summarizeDelta(got, truth)
		textResult(id, feedback)

	default:
		respondError(id, -32601, fmt.Sprintf("unknown tool: %s", name))
	}
}

func parseInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n := 0
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func validRating(r Ratings) bool {
	for _, v := range []int{r.Discovery, r.Objection, r.NextSteps, r.Pricing, r.Energy} {
		if v < 1 || v > 5 {
			return false
		}
	}
	return true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func summarizeDelta(got, truth Ratings) string {
	var b strings.Builder
	b.WriteString("Submitted. Per-dimension feedback (±1 counts as match):\n")
	rows := []struct {
		name        string
		got, truth  int
	}{
		{"discovery", got.Discovery, truth.Discovery},
		{"objection", got.Objection, truth.Objection},
		{"next_steps", got.NextSteps, truth.NextSteps},
		{"pricing", got.Pricing, truth.Pricing},
		{"energy", got.Energy, truth.Energy},
	}
	matches := 0
	for _, r := range rows {
		d := abs(r.got - r.truth)
		mark := "✓"
		if d > 1 {
			mark = "✗"
		} else {
			matches++
		}
		fmt.Fprintf(&b, "  %s %s: you=%d truth=%d delta=%d\n", mark, r.name, r.got, r.truth, d)
	}
	fmt.Fprintf(&b, "matched %d/5 dimensions", matches)
	return b.String()
}

func appendJSONL(path string, obj any) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(obj)
	f.Write(data)
	f.Write([]byte("\n"))
}

// ─── MCP loop ──────────────────────────────────────────────────────────

func main() {
	dataDir = os.Getenv("SALES_QA_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	os.MkdirAll(dataDir, 0755)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var req jsonRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
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
				"serverInfo":      map[string]string{"name": "sales_qa", "version": "1.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "get_rubric",
						"description": "Get the sales call grading rubric — 5 dimensions, scale 1-5, with detailed criteria for each level.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "list_training_calls",
						"description": "List the labeled training calls (id + title only). Use get_training_call to read each transcript and its ratings.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_training_call",
						"description": "Get a single labeled training call: full transcript, ground-truth ratings on all 5 dimensions, and grader notes.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Training call id, e.g. 'train-1'"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "list_test_calls",
						"description": "List the held-out test calls (id + title only). Use get_test_call to read each transcript, then submit_rating with your scores.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						"name":        "get_test_call",
						"description": "Get a single held-out test call. Returns transcript only — ratings are hidden.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]string{"type": "string", "description": "Test call id, e.g. 'test-1'"},
							},
							"required": []string{"id"},
						},
					},
					{
						"name":        "submit_rating",
						"description": "Submit your rating for a test call. All 5 dimensions are integers 1-5. The server scores you against hidden ground truth and returns per-dimension deltas (±1 counts as match).",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"call_id":    map[string]string{"type": "string"},
								"discovery":  map[string]string{"type": "integer"},
								"objection":  map[string]string{"type": "integer"},
								"next_steps": map[string]string{"type": "integer"},
								"pricing":    map[string]string{"type": "integer"},
								"energy":     map[string]string{"type": "integer"},
							},
							"required": []string{"call_id", "discovery", "objection", "next_steps", "pricing", "energy"},
						},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
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
