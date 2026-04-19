// MCP server for the multi-center RubricLearning scenario.
//
// Exposes N call-center QA harnesses through a single MCP. Each center
// has its own rubric, dimension set, labeled training transcripts, and
// held-out test set with hidden ground truth. The agent's job is to
// study one center's rubric + training calls, internalize the patterns,
// then rate that center's test calls. The server scores each submission
// against the center-scoped ground truth and returns per-dimension
// deltas so the agent can self-correct.
//
// Multi-center fan-out: the canonical scenario spawns one sub-thread per
// center so each gets isolated memory and directive scope. The MCP
// itself is stateless w.r.t. which thread is calling — every tool takes
// an explicit `center` argument and looks up the matching Center
// struct. Multiple MCP processes (one per sub-thread) can write to
// {{dataDir}}/submissions.jsonl concurrently; appendJSONL serializes
// each row into a single write() so the file stays line-coherent.
//
// State:
//   {{dataDir}}/submissions.jsonl  — one row per submit_rating call,
//                                    tagged with center.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// LabeledCall is a training transcript with ground-truth ratings keyed
// by the center's dimension names. Notes explain WHY this call earned
// its scores — the most useful signal for the agent's calibration.
type LabeledCall struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Transcript string         `json:"transcript"`
	Ratings    map[string]int `json:"ratings"`
	Notes      string         `json:"notes,omitempty"`
}

// TestCall is a held-out transcript with no ratings exposed.
type TestCall struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Transcript string `json:"transcript"`
}

// Center is one self-contained QA domain: rubric text, ordered
// dimension keys, training pool, test pool, and hidden ground truth.
// Different centers intentionally use different dimension sets — a
// telesales rubric isn't a SaaS-demo rubric — which is the whole point
// of multi-center.
type Center struct {
	ID          string
	Name        string
	Description string
	Rubric      string
	Dims        []string
	Training    []LabeledCall
	Test        []TestCall
	Truth       map[string]map[string]int
}

// ─── Centers ───────────────────────────────────────────────────────────

var centers = map[string]*Center{
	"saas_demo":        saasDemoCenter,
	"telesales":        telesalesCenter,
	"support_recovery": supportRecoveryCenter,
}

// orderedCenterIDs returns the keys of the centers map in a stable
// alphabetical order so list_centers output is deterministic.
func orderedCenterIDs() []string {
	ids := make([]string, 0, len(centers))
	for id := range centers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ─── Center 1: saas_demo ───────────────────────────────────────────────

var saasDemoCenter = &Center{
	ID:          "saas_demo",
	Name:        "SaaS demo / discovery calls",
	Description: "Mid-market B2B SaaS demo and first-discovery calls. Reps are AEs, prospects are decision-makers (CFO, VP Eng, Head of Ops). Goal: qualify the deal and book a real next step.",
	Dims:        []string{"discovery", "objection", "next_steps", "pricing", "energy"},
	Rubric: `SAAS DEMO/DISCOVERY RUBRIC — 5 dimensions, each rated 1-5.

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

A "good call" averages 4+. A "needs work" call averages 2.5 or below.`,
	Training: []LabeledCall{
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
			Ratings: map[string]int{"discovery": 5, "objection": 4, "next_steps": 5, "pricing": 5, "energy": 5},
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
			Ratings: map[string]int{"discovery": 1, "objection": 1, "next_steps": 2, "pricing": 2, "energy": 2},
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
			Ratings: map[string]int{"discovery": 3, "objection": 2, "next_steps": 3, "pricing": 4, "energy": 3},
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
			Ratings: map[string]int{"discovery": 5, "objection": 3, "next_steps": 2, "pricing": 4, "energy": 4},
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
			Ratings: map[string]int{"discovery": 2, "objection": 3, "next_steps": 3, "pricing": 3, "energy": 3},
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
			Ratings: map[string]int{"discovery": 5, "objection": 3, "next_steps": 4, "pricing": 1, "energy": 5},
			Notes:   "Killer discovery and warm intro close. But total dodge on the differentiation question, and pricing was never even raised.",
		},
	},
	Test: []TestCall{
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
	},
	Truth: map[string]map[string]int{
		"test-1": {"discovery": 5, "objection": 4, "next_steps": 5, "pricing": 4, "energy": 4},
		"test-2": {"discovery": 1, "objection": 1, "next_steps": 1, "pricing": 2, "energy": 2},
		"test-3": {"discovery": 3, "objection": 4, "next_steps": 2, "pricing": 3, "energy": 3},
	},
}

// ─── Center 2: telesales ───────────────────────────────────────────────

var telesalesCenter = &Center{
	ID:          "telesales",
	Name:        "Outbound telesales / cold calls",
	Description: "B2C/B2SMB outbound cold calls. Rep has 60-90 seconds to earn the right to a meeting. Goal: book a follow-up appointment, not close on the spot.",
	Dims:        []string{"opener", "qualification", "rebuttal", "close", "tone"},
	Rubric: `OUTBOUND TELESALES RUBRIC — 5 dimensions, each rated 1-5.

DIMENSION 1: opener — does the rep earn the next 60 seconds?
  5 = pattern interrupt + relevance signal in <15s ("I know this is cold —
      I'm calling because I noticed X about your business")
  4 = polite intro + reason for call, no fluff
  3 = generic intro, takes 30+ seconds to get to the point
  2 = scripted opener that screams "telesales", no relevance hook
  1 = "is now a good time to talk?" — cardinal sin, hands the call away

DIMENSION 2: qualification — confirms the prospect is the right fit
  5 = surfaces fit/non-fit signals in <2 questions (size, role, current
      tool) and confirms or politely exits
  4 = qualifies but slowly, asking generic questions
  3 = jumps straight to pitch, qualification implicit
  2 = pitches a wildly unfit prospect for minutes before realizing
  1 = no qualification — pitches anyone who answers

DIMENSION 3: rebuttal — handling "not interested" / "send info" / "no time"
  5 = acknowledges + reframes ("totally fair — most people aren't until
      they see X") and earns one more sentence
  4 = polite rebuttal that lands the meeting
  3 = one rebuttal attempt, gives up
  2 = pushy rebuttal, prospect annoyed
  1 = no rebuttal — accepts "no" the moment it's said

DIMENSION 4: close — books a specific time
  5 = offers two specific time slots and locks one ("Thursday at 2 or
      Friday at 10?")
  4 = single specific time offered, prospect agrees
  3 = soft close ("can I follow up?")
  2 = "I'll send some info" — no calendar commitment
  1 = no close attempted

DIMENSION 5: tone — conversational, confident, not robotic
  5 = sounds like a peer, easy laugh, no jargon
  4 = professional and warm
  3 = pleasant but reads from script
  2 = monotone or aggressively scripted
  1 = robotic / pushy / making the prospect uncomfortable

A "good call" averages 4+. Most cold calls live around 2.5.`,
	Training: []LabeledCall{
		{
			ID:    "train-1",
			Title: "Strong opener, books the meeting",
			Transcript: `REP: Hi, this is Maya from Bridgeline. I know this is a cold call so I'll be quick — I noticed you guys just opened a new clinic in Austin, and we work with about 40 dental groups around your size on patient recall. Worth 30 seconds?
PROSPECT: Sure, go ahead.
REP: Real quick — how many active patients are you tracking, and what are you using today for recall?
PROSPECT: About 8,000. We use the built-in recall in our PMS but it's pretty basic.
REP: Yeah, that's the usual — sends a generic email and calls it done. Most groups your size are losing about 15% of recall opportunities to that. We get that down to 3-4%.
PROSPECT: Hmm. Send me some info?
REP: Happy to, but the info won't show you the lift on YOUR data. Could I grab 20 minutes Thursday at 2 or Friday at 10 to walk you through what it'd look like for an 8,000-patient panel?
PROSPECT: Friday at 10 works.
REP: Locked in. I'll send a calendar invite in the next 5 minutes.`,
			Ratings: map[string]int{"opener": 5, "qualification": 5, "rebuttal": 5, "close": 5, "tone": 5},
			Notes:   "Pattern interrupt opener, relevance hook (Austin clinic), 2-question qualification, perfect rebuttal on 'send info', two-time close. Textbook.",
		},
		{
			ID:    "train-2",
			Title: "Robotic script, gives up immediately",
			Transcript: `REP: Hi, my name is Brad and I'm calling from Megacorp Solutions. How are you doing today?
PROSPECT: Fine. What's this about?
REP: Great! I'm reaching out because we work with companies in your industry to streamline operations and improve efficiency. Do you have a few minutes to discuss your current workflow challenges?
PROSPECT: Not really, I'm busy.
REP: Oh OK. Should I call back another time?
PROSPECT: Just send me an email.
REP: Sure thing! What's a good email?
PROSPECT: It's on our website.
REP: Got it. Have a great day!`,
			Ratings: map[string]int{"opener": 1, "qualification": 1, "rebuttal": 1, "close": 1, "tone": 2},
			Notes:   "Cardinal sin opener ('how are you'). Vague pitch. No qualification. No rebuttal at all — accepted brush-off. No close.",
		},
		{
			ID:    "train-3",
			Title: "Decent opener, weak close",
			Transcript: `REP: Hi, this is Alex from CloudPipe. Quick call — we help logistics companies cut their fuel reporting time. I'm reaching out because I saw your fleet just expanded to 50 trucks. Got 30 seconds?
PROSPECT: Yeah, what is it?
REP: How are you doing fuel cost reporting today? Manual or automated?
PROSPECT: We're still doing it in Excel, honestly.
REP: That's the pain point. For 50 trucks that's probably 6-8 hours a week. We bring it down to about 30 minutes. Want me to send over a one-pager?
PROSPECT: Sure, send it over.
REP: Will do. I'll follow up in a couple weeks to see what you thought.`,
			Ratings: map[string]int{"opener": 4, "qualification": 4, "rebuttal": 2, "close": 2, "tone": 4},
			Notes:   "Solid opener with relevance hook. Good qualification. But folded on 'send info' — should have pushed for a meeting. No specific time = death.",
		},
	},
	Test: []TestCall{
		{
			ID:    "test-1",
			Title: "Strong opener, weak qualification",
			Transcript: `REP: Hi, this is Sam from Loomwise. I'll be quick — I'm calling because I saw your team is hiring 3 inside-sales reps and we help SDR teams ramp 40% faster. Worth 30 seconds?
PROSPECT: Sure.
REP: Cool. Quick question — what does your current ramp process look like?
PROSPECT: Honestly we don't really have one. We just throw people in.
REP: That tracks with most teams I talk to. We have a structured ramp playbook plus weekly call coaching that gets new reps to quota in about half the typical time.
PROSPECT: Interesting. Hmm.
REP: Could I grab 20 minutes Tuesday at 11 or Wednesday at 3 to show you what it'd look like for your team?
PROSPECT: Tuesday at 11 works.
REP: Done — calendar invite incoming.`,
		},
		{
			ID:    "test-2",
			Title: "Generic everything",
			Transcript: `REP: Hi, this is Tom from Nimbus Cloud. I'm reaching out to see if you'd be interested in learning more about our cloud solutions. Do you currently use any cloud services?
PROSPECT: Yes, we use AWS.
REP: Great! Would you be open to a 30-minute meeting to discuss how we could potentially help you optimize your cloud spend?
PROSPECT: Not really, we're happy with our setup.
REP: Are you sure? We've helped many companies save up to 30%.
PROSPECT: I'm sure. Thanks though.
REP: OK no problem, have a good day.`,
		},
	},
	Truth: map[string]map[string]int{
		"test-1": {"opener": 5, "qualification": 4, "rebuttal": 3, "close": 5, "tone": 4},
		"test-2": {"opener": 2, "qualification": 2, "rebuttal": 2, "close": 1, "tone": 3},
	},
}

// ─── Center 3: support_recovery ────────────────────────────────────────

var supportRecoveryCenter = &Center{
	ID:          "support_recovery",
	Name:        "Inbound support / escalation recovery",
	Description: "Inbound customer-support phone calls where the customer is already frustrated (escalated from chat or previous unresolved ticket). Goal: defuse, diagnose, and either resolve or hand off cleanly.",
	Dims:        []string{"empathy", "diagnosis", "resolution", "deescalation", "clarity"},
	Rubric: `SUPPORT-RECOVERY RUBRIC — 5 dimensions, each rated 1-5.

DIMENSION 1: empathy — does the rep acknowledge the customer's frustration?
  5 = explicit acknowledgement in first 30s ("I hear you, this is genuinely
      frustrating, I'd be upset too"), uses customer's own words back
  4 = acknowledges the issue, slightly clinical
  3 = polite but doesn't really acknowledge the emotion
  2 = jumps straight to "let me check your account" without acknowledgement
  1 = dismissive or defensive ("our system shows it's working")

DIMENSION 2: diagnosis — does the rep find the actual root cause?
  5 = asks targeted questions, identifies the real problem (not just
      symptoms), explains what happened in plain language
  4 = identifies the problem with some prodding
  3 = treats symptoms — solves the surface issue without finding root cause
  2 = guesses, asks generic "have you restarted" questions
  1 = no diagnosis attempted, throws solutions at the wall

DIMENSION 3: resolution — fixes it OR commits a clear path forward
  5 = resolves on the call, OR commits to a specific fix with a timeline
      and writes it into the ticket on the call
  4 = clear next step with timeline, slight ambiguity on responsibility
  3 = "we'll look into it and get back to you" — no timeline
  2 = passes the customer to another queue without warm handoff
  1 = no resolution path, customer is left holding the problem

DIMENSION 4: deescalation — does the call end calmer than it started?
  5 = customer audibly calmed, says thanks unprompted, frustration gone
  4 = customer calm by end, businesslike
  3 = customer still tense but no longer escalating
  2 = customer ends call still frustrated
  1 = customer escalates further, asks for a manager

DIMENSION 5: clarity — explains things without jargon or condescension
  5 = explanations match the customer's apparent technical level, no
      jargon, no "as I said before" patronizing
  4 = clear with minor jargon
  3 = some unclear explanations, customer asks for repetition
  2 = jargon-heavy or condescending
  1 = customer is more confused at the end than the start

A "good recovery call" averages 4+. A "needs work" call averages 2 or below.`,
	Training: []LabeledCall{
		{
			ID:    "train-1",
			Title: "Textbook recovery — angry customer, calm by end",
			Transcript: `CUSTOMER: This is the third time I'm calling about this. My order was supposed to be here Tuesday and it's Friday. Nobody can tell me anything.
REP: I'm so sorry, that is genuinely frustrating — three calls and still no answer is not OK. I want to actually fix this for you today. Can I grab your order number while we talk?
CUSTOMER: It's 4471-892.
REP: Got it, pulling it up… OK, I see what happened. Your order shipped on time but it got rerouted at the Memphis hub because the address had a unit number missing. The carrier flagged it but the alert never made it to you because of an old email on file. That's our process gap, not yours.
CUSTOMER: So where is it now?
REP: It's currently sitting in Memphis. I can either reroute it with the corrected address — that gets it to you Monday — or I can ship a replacement overnight from a closer warehouse and it'll be there tomorrow by noon. I'll cover the overnight either way.
CUSTOMER: Tomorrow by noon would be great.
REP: Done. I'm placing the overnight order right now and I'll text you the tracking in the next 10 minutes. I'm also updating your email on file so this doesn't happen again. Anything else I can do while we're on?
CUSTOMER: No, that's perfect. Thank you — honestly this is the first time anyone's actually helped.`,
			Ratings: map[string]int{"empathy": 5, "diagnosis": 5, "resolution": 5, "deescalation": 5, "clarity": 5},
			Notes:   "Acknowledged frustration in first 10 words. Found root cause (address + email). Two clear options with timelines. Customer thanks rep unprompted.",
		},
		{
			ID:    "train-2",
			Title: "Defensive, makes it worse",
			Transcript: `CUSTOMER: I've been charged twice for the same subscription and I want it refunded.
REP: Our system shows only one active subscription on your account.
CUSTOMER: I'm looking at my credit card statement right now. Two charges, same date, same amount.
REP: Have you tried logging out and back in?
CUSTOMER: That has nothing to do with my credit card.
REP: Ma'am, I can only see what's in the system. If you want to dispute the charge you can contact your bank.
CUSTOMER: I'm not contacting my bank, you charged me twice.
REP: I understand you're upset but I'm telling you what I'm seeing. Maybe one of the charges is from a different service?
CUSTOMER: It says your company name on both. Get me a manager.`,
			Ratings: map[string]int{"empathy": 1, "diagnosis": 1, "resolution": 1, "deescalation": 1, "clarity": 2},
			Notes:   "Defensive from word one. No empathy. Suggests irrelevant fixes. Tells customer to contact their bank for our billing error. Customer escalates.",
		},
		{
			ID:    "train-3",
			Title: "Empathetic but no real fix",
			Transcript: `CUSTOMER: My internet has been down all morning and I work from home.
REP: Oh no, I'm really sorry — that's super frustrating, especially when you're trying to work. Let me see what I can do.
CUSTOMER: Thank you.
REP: OK, I see there's an outage in your area. It looks like our team is working on it. Unfortunately I can't give you an exact ETA right now.
CUSTOMER: Is there anything you can do?
REP: I can put a note on your account so you don't get charged for the downtime. And honestly, the best I can do is suggest you try again in a few hours.
CUSTOMER: OK… so I just wait.
REP: Yeah, unfortunately. I'm so sorry. We'll send an email once it's resolved.
CUSTOMER: Alright. Thanks.`,
			Ratings: map[string]int{"empathy": 5, "diagnosis": 3, "resolution": 2, "deescalation": 3, "clarity": 4},
			Notes:   "Strong empathy. But no real path forward — 'just wait' is not a resolution. No timeline. No proactive escalation to engineering for ETA.",
		},
	},
	Test: []TestCall{
		{
			ID:    "test-1",
			Title: "Calm escalation, clear fix",
			Transcript: `CUSTOMER: I've been getting double-charged for two months. I called last month and was told it would be refunded but nothing happened.
REP: That's really frustrating — being told something would be fixed and then having to call back is honestly the worst. I'm going to make sure we actually resolve this on the call today. Can I grab your account number?
CUSTOMER: 7821-2243.
REP: Thanks. I see both charges, both months, and I see last month's call where the previous agent opened a refund ticket but never submitted it for approval. That's on us. I'm processing both refunds right now — they'll hit your card in 3-5 business days. I'm also adding a note on your account flagging this so if anything goes wrong on our side, the next agent sees it immediately.
CUSTOMER: OK, that's good to hear. How will I know it went through?
REP: I'm emailing you the refund confirmation as soon as we hang up — should be in your inbox in a couple minutes. If you don't see it within the hour, my direct extension is 4471 and I'll fix it personally.
CUSTOMER: Alright, thank you. Sorry for being short earlier.
REP: No need to apologize at all — you had every reason. Have a good rest of your day.`,
		},
		{
			ID:    "test-2",
			Title: "Tries hard but jargon-heavy",
			Transcript: `CUSTOMER: My device won't connect to anything anymore. It just keeps spinning.
REP: I'm sorry to hear that. Let me see — could be a couple things. Have you tried clearing your DNS cache?
CUSTOMER: My what?
REP: DNS — domain name system. It's like the address book for the internet. We can flush it via the terminal.
CUSTOMER: I don't… I don't know what that means.
REP: OK, let me back up. Is the wifi icon showing connected?
CUSTOMER: There's a little exclamation mark on it.
REP: That's a captive portal not authenticating. Try toggling airplane mode on and off.
CUSTOMER: OK… it's still doing the same thing.
REP: Hmm. Let me escalate this to tier 2 networking. Someone will call you back within 24 hours.
CUSTOMER: 24 hours? I need internet for work.
REP: I understand. I'll mark it as urgent.`,
		},
	},
	Truth: map[string]map[string]int{
		"test-1": {"empathy": 5, "diagnosis": 5, "resolution": 5, "deescalation": 5, "clarity": 5},
		"test-2": {"empathy": 3, "diagnosis": 2, "resolution": 2, "deescalation": 2, "clarity": 1},
	},
}

// ─── Tool dispatch ─────────────────────────────────────────────────────

var dataDir string

func handleToolCall(id int64, name string, args map[string]any) {
	// list_centers is the only tool that doesn't take a center arg.
	if name == "list_centers" {
		out := make([]map[string]any, 0, len(centers))
		for _, cid := range orderedCenterIDs() {
			c := centers[cid]
			out = append(out, map[string]any{
				"id":               c.ID,
				"name":             c.Name,
				"description":      c.Description,
				"dimensions":       c.Dims,
				"training_count":   len(c.Training),
				"test_count":       len(c.Test),
			})
		}
		j, _ := json.Marshal(out)
		textResult(id, string(j))
		return
	}

	// Every other tool requires a `center` argument. Look it up once;
	// fail fast and clearly when the agent passes a bogus or missing id.
	centerID, _ := args["center"].(string)
	if centerID == "" {
		respondError(id, -32602, "center required (call list_centers to discover available centers)")
		return
	}
	c, ok := centers[centerID]
	if !ok {
		textResult(id, fmt.Sprintf("unknown center %q — call list_centers to see what's available", centerID))
		return
	}

	switch name {
	case "get_rubric":
		textResult(id, c.Rubric)

	case "list_training_calls":
		out := make([]map[string]string, 0, len(c.Training))
		for _, tc := range c.Training {
			out = append(out, map[string]string{"id": tc.ID, "title": tc.Title})
		}
		j, _ := json.Marshal(out)
		textResult(id, string(j))

	case "get_training_call":
		callID, _ := args["id"].(string)
		if callID == "" {
			respondError(id, -32602, "id required")
			return
		}
		for _, tc := range c.Training {
			if tc.ID == callID {
				j, _ := json.Marshal(tc)
				textResult(id, string(j))
				return
			}
		}
		textResult(id, fmt.Sprintf("training call %q not found in center %q", callID, c.ID))

	case "list_test_calls":
		out := make([]map[string]string, 0, len(c.Test))
		for _, tc := range c.Test {
			out = append(out, map[string]string{"id": tc.ID, "title": tc.Title})
		}
		j, _ := json.Marshal(out)
		textResult(id, string(j))

	case "get_test_call":
		callID, _ := args["id"].(string)
		if callID == "" {
			respondError(id, -32602, "id required")
			return
		}
		for _, tc := range c.Test {
			if tc.ID == callID {
				j, _ := json.Marshal(map[string]any{
					"id":         tc.ID,
					"title":      tc.Title,
					"transcript": tc.Transcript,
				})
				textResult(id, string(j))
				return
			}
		}
		textResult(id, fmt.Sprintf("test call %q not found in center %q", callID, c.ID))

	case "submit_rating":
		callID, _ := args["call_id"].(string)
		if callID == "" {
			respondError(id, -32602, "call_id required")
			return
		}
		truth, ok := c.Truth[callID]
		if !ok {
			textResult(id, fmt.Sprintf("call %q is not in the test set for center %q", callID, c.ID))
			return
		}
		// Accept ratings either nested under args["ratings"] (preferred)
		// or as top-level keys (tolerant of the older flat shape so
		// agents that pattern-match the old single-center scenario keep
		// working).
		raw := map[string]any{}
		if nested, ok := args["ratings"].(map[string]any); ok {
			raw = nested
		} else {
			for _, dim := range c.Dims {
				if v, ok := args[dim]; ok {
					raw[dim] = v
				}
			}
		}
		got := map[string]int{}
		for _, dim := range c.Dims {
			got[dim] = parseInt(raw[dim])
		}
		if !validRatings(got, c.Dims) {
			textResult(id, fmt.Sprintf("each of {%s} must be an integer 1-5", strings.Join(c.Dims, ", ")))
			return
		}
		deltas := map[string]int{}
		for _, dim := range c.Dims {
			deltas[dim] = abs(got[dim] - truth[dim])
		}
		entry := map[string]any{
			"time":         time.Now().UTC().Format(time.RFC3339),
			"center":       c.ID,
			"call_id":      callID,
			"submitted":    got,
			"ground_truth": truth,
			"deltas":       deltas,
		}
		appendJSONL(filepath.Join(dataDir, "submissions.jsonl"), entry)
		textResult(id, summarizeDelta(c.Dims, got, truth))

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

func validRatings(got map[string]int, dims []string) bool {
	for _, d := range dims {
		v, ok := got[d]
		if !ok || v < 1 || v > 5 {
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

func summarizeDelta(dims []string, got, truth map[string]int) string {
	var b strings.Builder
	b.WriteString("Submitted. Per-dimension feedback (±1 counts as match):\n")
	matches := 0
	for _, d := range dims {
		dlt := abs(got[d] - truth[d])
		mark := "✓"
		if dlt > 1 {
			mark = "✗"
		} else {
			matches++
		}
		fmt.Fprintf(&b, "  %s %s: you=%d truth=%d delta=%d\n", mark, d, got[d], truth[d], dlt)
	}
	fmt.Fprintf(&b, "matched %d/%d dimensions", matches, len(dims))
	return b.String()
}

// appendJSONL writes one row to path. Combines the JSON body and the
// terminating newline into a single write() so concurrent appenders
// from multiple sub-thread MCP processes can't interleave bytes —
// Linux O_APPEND guarantees atomicity per write call up to PIPE_BUF
// (4 KB), and a submission row is well under that.
func appendJSONL(path string, obj any) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(obj)
	buf := make([]byte, 0, len(data)+1)
	buf = append(buf, data...)
	buf = append(buf, '\n')
	f.Write(buf)
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
				"serverInfo":      map[string]string{"name": "sales_qa", "version": "2.0.0"},
			})
		case "tools/list":
			respond(id, map[string]any{"tools": toolDefs()})
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

// toolDefs returns the tool catalog. center-scoped tools all take an
// explicit `center` argument so the agent can fan out across multiple
// centers from a single MCP connection.
func toolDefs() []map[string]any {
	stringField := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	intField := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}
	centerArg := stringField("Center id from list_centers — e.g. \"saas_demo\".")

	return []map[string]any{
		{
			"name":        "list_centers",
			"description": "List all available call centers. Each center has its own rubric, dimension set, training pool, and test pool. Returns id, name, description, dimensions, training/test counts.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "get_rubric",
			"description": "Get a center's grading rubric — full text with per-dimension level criteria. Call this first for any center you're going to grade.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"center": centerArg},
				"required":   []string{"center"},
			},
		},
		{
			"name":        "list_training_calls",
			"description": "List labeled training calls for a center (id + title only). Use get_training_call to read each transcript and its ratings.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"center": centerArg},
				"required":   []string{"center"},
			},
		},
		{
			"name":        "get_training_call",
			"description": "Get a single labeled training call: full transcript, ground-truth ratings (one int per dimension), and grader notes. The notes explain WHY the call earned that score — read them carefully.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"center": centerArg,
					"id":     stringField("Training call id (from list_training_calls)."),
				},
				"required": []string{"center", "id"},
			},
		},
		{
			"name":        "list_test_calls",
			"description": "List held-out test calls for a center (id + title only). Read each via get_test_call, then submit your ratings via submit_rating.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"center": centerArg},
				"required":   []string{"center"},
			},
		},
		{
			"name":        "get_test_call",
			"description": "Get a single test call. Returns transcript only — ground-truth ratings are hidden until you submit.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"center": centerArg,
					"id":     stringField("Test call id (from list_test_calls)."),
				},
				"required": []string{"center", "id"},
			},
		},
		{
			"name":        "submit_rating",
			"description": "Submit your rating for a test call. Pass `ratings` as an object keyed by the center's dimension names with integer values 1-5. The server scores you against hidden ground truth and returns per-dimension deltas (±1 counts as match). You may resubmit; the latest submission per (center, call_id) wins.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"center":  centerArg,
					"call_id": stringField("Test call id."),
					"ratings": map[string]any{
						"type":        "object",
						"description": "Map of dimension name → integer 1-5. Keys must match the center's dimensions exactly (see list_centers / get_rubric).",
						"additionalProperties": intField("integer 1-5"),
					},
				},
				"required": []string{"center", "call_id", "ratings"},
			},
		},
	}
}
