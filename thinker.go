package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apteva/core/pkg/computer"
)

// Default context window sizes by role
const (
	maxHistoryMain   = 100 // main coordinator
	maxHistoryLead   = 100 // team leads (depth 0)
	maxHistoryWorker = 20 // workers (depth 1+)
)

// MCPServerInfo is a lightweight catalog entry for an MCP server.
// Main uses this to show available servers in its prompt without registering all tools.
type MCPServerInfo struct {
	Name      string
	ToolCount int
}



type ModelTier int

const (
	ModelLarge ModelTier = iota
	ModelMedium
	ModelSmall
)

var modelNames = map[string]ModelTier{
	"large":  ModelLarge,
	"medium": ModelMedium,
	"small":  ModelSmall,
}

func (m ModelTier) String() string {
	switch m {
	case ModelLarge:
		return "large"
	case ModelMedium:
		return "medium"
	case ModelSmall:
		return "small"
	default:
		return "medium"
	}
}

// modelID returns the model ID from the provider for a given tier.
func (t *Thinker) modelID() string {
	if t.provider != nil {
		return t.provider.Models()[t.model]
	}
	return "unknown"
}

// shouldEmitBlobHint decides whether to include the [FILE HANDLES]
// explainer. The hint is only actionable when the conversation
// actually contains a blob handle or a tool likely to produce one.
//
// Prior heuristic (any MCP present → emit) was too generous: channels
// is a text-only MCP and triggered the hint every turn for ~500 bytes
// of dead weight. The new rule narrows to three concrete signals:
//
//  1. A blob reference already appears in the message history — the
//     model is about to see a "blobref://" token and needs the rule
//     to understand it. This is the strongest signal.
//  2. A blob-producing local tool is registered (read_file, exec,
//     computer_use, etc.) — these emit handles on the next call, so
//     the hint needs to ride even before the first blob appears.
//  3. An MCP whose name hints at binary content (media, audio,
//     image, file, video, deepgram, pdf) is attached to this thread
//     or an active sub-thread. Conservative allowlist — if an unknown
//     MCP produces a handle and we didn't match, signal #1 kicks in
//     on the turn AFTER so the model recovers within one iteration.
func shouldEmitBlobHint(registry *ToolRegistry, messages []Message, activeThreads []ThreadInfo) bool {
	// Signal 1: already a blob in context — always emit.
	for _, m := range messages {
		if strings.Contains(m.Content, "blobref://") {
			return true
		}
		for _, tr := range m.ToolResults {
			if strings.Contains(tr.Content, "blobref://") {
				return true
			}
		}
	}
	if registry == nil {
		return false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	// Signal 2: local tool likely to produce binaries.
	for _, t := range registry.tools {
		switch t.Name {
		case "read_file", "list_files", "write_file",
			"exec", "computer_use", "browser_session":
			return true
		}
	}
	// Signal 3: MCP name on a known binary-producing server.
	binaryMCPs := map[string]bool{
		"media": true, "audio": true, "image": true,
		"file": true, "files": true, "video": true,
		"deepgram": true, "pdf": true, "storage": true,
		"gdrive": true,
	}
	for _, t := range registry.tools {
		if t.MCPServer != "" && binaryMCPs[t.MCPServer] {
			return true
		}
	}
	for _, th := range activeThreads {
		for _, name := range th.MCPNames {
			if binaryMCPs[name] {
				return true
			}
		}
	}
	return false
}

// poolSupportsNativeTools returns true if the pool's default provider
// receives tool schemas via NativeTools. Used by buildSystemPrompt to
// decide whether to emit full CoreDocs prose (text-only providers) or
// the compact summary (native-tool providers already get the full
// schemas in `tools[]`). nil pool → false so test callers without a
// pool get the conservative full-prose path.
func poolSupportsNativeTools(pool *ProviderPool) bool {
	if pool == nil {
		return false
	}
	p := pool.Default()
	if p == nil {
		return false
	}
	return p.SupportsNativeTools()
}

// baseSystemPrompt contains the fixed rules/tools. The editable directive is prepended at runtime.
const baseSystemPrompt = `You are the main coordinating thread of a continuous thinking engine. You observe all events, manage threads, and coordinate work.

THINKING:
- Every thought has at least one sentence of reasoning. Never output only tool calls.
- Keep thoughts short — 1-2 short paragraphs. Skip narration between calls; act.

EVENTS:
- [console] message — external event/command; act on it.
- [from:id] message — a thread sent this via send.
- [thread:id done] message — a thread terminated.
- NEVER invent events. If no [Events:] block arrived, do nothing except pace.

SPAWNING THREADS — critical rules:
- Before spawning, check [ACTIVE THREADS]: if an existing thread has matching tools and directive, send(id="...") to it instead. Spawn only when no existing thread fits, or when you need parallelism over independent inputs.
- tools= lists which tools the worker can use. ALWAYS include EVERY tool the worker needs to carry out its directive — if the directive says "run a script", include exec; if "transcribe audio", include the deepgram tool. A missing tool = worker reports failure and can't act. Use FULL prefixed names exactly as shown in [available tools] (e.g. "schedule_get_schedule", NOT "get_schedule").
- directive= is PLAIN NATURAL LANGUAGE describing the thread's goal. Never put tool names in the directive — the thread already receives its own tool documentation.
  BAD:  directive="Call helpdesk_list_tickets to check for tickets"
  GOOD: directive="Check for new support tickets periodically. Report findings to main."
- provider= (optional) picks a specific LLM; omit to inherit. Use a stronger provider for complex tasks, a cheaper one for coordination. See [AVAILABLE PROVIDERS].
- For recurring schedules with >1 timer or noisy traffic, spawn a pace,send-only coordinator thread that wakes on timer and delegates to the domain workers that own execution.

PACING:
- Events wake you instantly regardless of sleep — including [from:id] worker replies and [thread:id done] notifications. Never short-sleep to "check" on a delegated worker; pace "1h" and let the reply wake you.
- Sleep long ("1h", small model) the moment you have nothing actionable this iteration — delegating to a worker counts as nothing actionable.
- Short sleep (2-10s) is ONLY for timer-driven polls you own yourself (e.g. retry a rate-limited API in N seconds). Not for waiting on another thread.
- Pace persists — don't re-set it every thought. When an event wakes you, you auto-switch to large model for that turn.

TOOL CALLS:
- Every tool takes a "_reason" string: 3-6 words, imperative, describing THIS call (e.g. "find ventes sheet id", "update Score cell"). No "to …" clauses — the thought above already holds the why.

You have persistent memory across restarts. Relevant memories appear as [memories] blocks.`

func buildSystemPrompt(directive string, mode RunMode, registry *ToolRegistry, extraToolDocs string, servers []MCPConn, activeThreads []ThreadInfo, pool *ProviderPool, mcpCatalog []MCPServerInfo) string {
	coreDocs := ""
	if registry != nil {
		// Prefer the compact summary when the thread's provider receives
		// full tool schemas via NativeTools — the prose listing would
		// just duplicate Description+Rules already in tools[]. Fall back
		// to the full CoreDocs prose for providers without native tool
		// support (ollama text-only, some local runners) so they keep
		// seeing every rule they need to behave.
		if poolSupportsNativeTools(pool) {
			coreDocs = "\n" + registry.CoreDocsSummary(true)
		} else {
			coreDocs = "\n" + registry.CoreDocs(true)
		}
	}
	prompt := baseSystemPrompt + coreDocs
	if extraToolDocs != "" {
		prompt += "\n" + extraToolDocs
	}

	// Inject lightweight MCP server catalog — just names and tool counts
	if len(mcpCatalog) > 0 {
		prompt += "\n\n[AVAILABLE MCP SERVERS]\n"
		prompt += "These servers provide tools for sub-threads. Use mcp=\"servername\" when spawning to give the thread its own connection.\n"
		prompt += "The thread will auto-discover all tools from that server. You do NOT need to list individual tool names.\n"
		prompt += "Example: spawn(id=\"ops\", directive=\"Manage inventory\", mcp=\"store\", tools=\"web\")\n\n"
		for _, info := range mcpCatalog {
			prompt += fmt.Sprintf("- %s (%d tools)\n", info.Name, info.ToolCount)
		}
	} else if registry != nil {
		// Fallback: show old-style MCP summary from registry (for main_access tools still registered)
		if summary := registry.MCPToolSummary(); summary != "" {
			prompt += summary
		}
	}

	// Inject available providers when multiple are configured
	if pool != nil && pool.Count() > 1 {
		prompt += "\n\n[AVAILABLE PROVIDERS]\n"
		for _, name := range pool.Names() {
			prompt += "- " + pool.ProviderSummary(name) + "\n"
		}
		prompt += "\nUse provider=\"name\" in spawn or pace to select a specific provider. Default: " + pool.DefaultName() + ".\n"
	}

	// Inject active thread state so main always knows what's running
	if len(activeThreads) > 0 {
		prompt += "\n\n[ACTIVE THREADS]\n"
		for _, t := range activeThreads {
			age := time.Since(t.Started).Truncate(time.Second)
			subInfo := ""
			if t.SubThreads > 0 {
				subInfo = fmt.Sprintf(", sub-threads: %d", t.SubThreads)
			}
			prompt += fmt.Sprintf("- %s (running %s, iter #%d, pace %s, model %s%s)\n  directive: %s\n  tools: %s\n",
				t.ID, age, t.Iteration, t.Rate.String(), t.Model.String(), subInfo, truncateStr(t.Directive, 150), strings.Join(t.Tools, ", "))
		}
	}

	// Safety guidance based on mode
	prompt += "\n\n[SAFETY MODE: " + string(mode) + "]\n"
	switch mode {
	case ModeCautious:
		prompt += `You act carefully. Read-only tools (screenshot, list, query, read_file, web search, memory_scan) are free — use them at will.

Before any STATE-CHANGING tool (exec, write, delete, deploy, restart, purchase, send-as-user, browser actions on logged-in sites):
- Send one concise channels_respond explaining action + target + why (one sentence each).
- Wait for the user's next message before executing. Don't chain tool calls.
- If unsure whether an action is state-changing, ask. Asking is cheap; undoing is expensive.

Remember liberally — every correction, preference, or approved decision. Use consistent bracketed tags ([correction], [preference], [decision], [fact]) so recall surfaces the right memory next time. The more you remember, the fewer times you'll need to ask.`
	case ModeLearn:
		prompt += `You are learning the user's preferences through conversation. You're soft-gated: nothing blocks you — the quality of this mode depends on YOU actually pausing, asking, and remembering.

BEFORE A NEW KIND OF ACTION:
1. Check what memories the recall system already surfaced this turn. If a [preference] or [correction] already covers this action + target, follow it silently.
2. Otherwise, if the action could affect the user (state-changing, external, touches their data/accounts, irreversible), send ONE short channels_respond:
   "About to <verb> <target>. Reason: <one sentence>. OK?"
   Wait for their answer before proceeding.
3. Skip asking for obviously-safe tools (screenshot, list, read_file, web search, memory_scan, pace, think-only actions).

AFTER THE USER ANSWERS, ALWAYS remember their decision in a consistent structured form so recall actually surfaces it next time:
  [[remember text="[preference] <tool>: <when it applies> — <user's decision>"]]
Good examples:
  [preference] exec: shell commands on user's own server — OK without asking
  [preference] delete_file: any path under /work — always ask first
  [preference] browser: logging into banking sites — never
  [correction] tone: user prefers terse replies, no headings
  [correction] email: don't send email before 8am user-local
  [fact] user's server: 46.224.160.146, alias "worker-d0e70653"
  [decision] approved: daily 9am digest via Telegram

Remember MORE than you think you should. Corrections especially — any "no", "don't", "stop", "I didn't want that" becomes a [correction] memory IMMEDIATELY. User tone/style, project context, recurring tasks, names and deadlines — all worth storing.

The point of learn mode is that asking frequency DROPS OVER TIME. If you keep asking about the same thing, you didn't remember it well enough — rewrite the memory more specifically.`
	default: // ModeAutonomous
		prompt += `You operate independently and are trusted to act. Use that trust to get things done.

- For irreversible or high-blast-radius actions (mass delete, publish externally, spend money, send as user), tell the user briefly before acting — don't ask, inform.
- Assess risk honestly. If genuinely unsure, ask.
- When the user corrects or pushes back, stop and adjust immediately — don't argue.

ACT, DON'T NARRATE. You have no live audience between thoughts — every tool result comes back as structured input, not as something a human is watching scroll by. Skip the "let me think about this, I'll take a screenshot to see what's there, then I'll consider the options before..." prose. Take the next tool call. The tool's output is your feedback; react to it on the next iteration. Reserve natural-language output for channels_respond (actually talking to the user) and [[remember]] (storing knowledge). Thoughts that produce only prose and no tool call waste a round-trip.

Remember actively. Every correction, preference, and consequential decision gets a [[remember]] with a bracketed tag ([correction], [preference], [decision], [fact]) so recall surfaces it on future turns. Remember liberally — storage is cheap, confusion is expensive.`
	}

	// Inject learned skills if any exist
	if skills := loadSkills(); skills != "" {
		prompt += "\n\n" + skills
	}

	// blobPromptHint explains the {"_file": true, ...} handle format.
	// Only emit when a blob is already in context OR the scope has a
	// tool likely to produce one — see shouldEmitBlobHint. Can't check
	// the current messages from here (buildSystemPrompt is stateless)
	// so we approximate via the registry and threads; callers with
	// conversation context can override by setting a sentinel MCP.
	if shouldEmitBlobHint(registry, nil, activeThreads) {
		prompt += blobPromptHint
	}

	prompt += "\n\n[DIRECTIVE — EXECUTE ON STARTUP]\nThe following is your mission. On your FIRST thought, take any actions needed to fulfill it (spawn threads, etc). This overrides default idle behavior.\n\n" + directive
	return prompt
}

// loadSkills reads all skills/*.md files and returns them as a prompt block.
func loadSkills() string {
	files, err := filepath.Glob("skills/*.md")
	if err != nil || len(files) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[LEARNED SKILLS]\n")
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || len(data) == 0 {
			continue
		}
		sb.WriteString(string(data))
		sb.WriteString("\n\n")
	}
	if sb.Len() < 20 {
		return ""
	}
	return sb.String()
}

func truncateStr(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

type TokenUsage struct {
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
}

type ThinkRate int

const (
	RateReactive ThinkRate = iota // 500ms — event just arrived
	RateFast                      // 2s — actively working
	RateNormal                    // 10s — thinking, no urgency
	RateSlow                      // 30s — not much to do
	RateSleep                     // 120s — deep idle
)

// rateAliases maps named rates to durations (backwards compat + convenience)
var rateAliases = map[string]time.Duration{
	"reactive": 500 * time.Millisecond,
	"fast":     2 * time.Second,
	"normal":   10 * time.Second,
	"slow":     30 * time.Second,
	"sleep":    2 * time.Minute,
}

// rateNames kept for ThinkRate enum mapping (used by eventbus, TUI, etc.)
var rateNames = map[string]ThinkRate{
	"reactive": RateReactive,
	"fast":     RateFast,
	"normal":   RateNormal,
	"slow":     RateSlow,
	"sleep":    RateSleep,
}

const (
	minSleep = 500 * time.Millisecond
	maxSleep = 24 * time.Hour
)

// parseSleepDuration parses a sleep duration from agent input.
// Accepts Go duration strings ("30s", "5m", "2h") or named aliases ("slow", "sleep").
func parseSleepDuration(s string) (time.Duration, bool) {
	// Check named aliases first
	if d, ok := rateAliases[s]; ok {
		return d, true
	}
	// Try Go duration string
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	// Clamp to bounds
	if d < minSleep {
		d = minSleep
	}
	if d > maxSleep {
		d = maxSleep
	}
	return d, true
}

// formatSleep returns a human-readable sleep duration string.
func formatSleep(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	if d >= time.Minute {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (r ThinkRate) String() string {
	switch r {
	case RateReactive:
		return "reactive"
	case RateFast:
		return "fast"
	case RateNormal:
		return "normal"
	case RateSlow:
		return "slow"
	case RateSleep:
		return "sleep"
	default:
		return "normal"
	}
}

func (r ThinkRate) Delay() time.Duration {
	switch r {
	case RateReactive:
		return 500 * time.Millisecond
	case RateFast:
		return 2 * time.Second
	case RateNormal:
		return 10 * time.Second
	case RateSlow:
		return 30 * time.Second
	case RateSleep:
		return 120 * time.Second
	default:
		return 10 * time.Second
	}
}

type APIEvent struct {
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`                 // "thought", "chunk", "reply", "thread_started", "thread_done", "error"
	ThreadID  string    `json:"thread_id"`
	Message   string    `json:"message,omitempty"`
	Iteration int       `json:"iteration,omitempty"`
	Duration  string    `json:"duration,omitempty"`
}

// ToolHandler processes parsed tool calls from a thought. Returns replies, tool names logged, and tool results for inline-handled tools.
// consumed contains the events that were consumed this iteration (for context).
type ToolHandler func(t *Thinker, calls []toolCall, consumed []string) (replies []string, toolNames []string, results []ToolResult)


type Thinker struct {
	apiKey    string
	pool      *ProviderPool // all available providers (shared across threads)
	provider  LLMProvider   // current active provider for this thinker
	messages  []Message
	bus       *EventBus
	sub       *Subscription
	pause     chan bool
	quit      chan struct{}
	iteration int
	paused    bool
	rate       ThinkRate
	agentRate  ThinkRate
	agentSleep time.Duration // freeform sleep duration (takes priority over agentRate when > 0)
	model      ModelTier
	agentModel ModelTier
	memory     *MemoryStore
	session    *Session
	threads    *ThreadManager
	config     *Config
	registry   *ToolRegistry

	maxHistory     int // max messages in context window (varies by role)

	// Hooks — set these to customize behavior. nil = defaults.
	handleTools    ToolHandler
	rebuildPrompt  func(toolDocs string) string // rebuild system prompt with current tool docs
	onStop         func()
	toolAllowlist  map[string]bool // nil = all tools allowed (main thread)

	// API event log — shared across all threads, owned by main thinker
	apiLog    *[]APIEvent
	apiMu     *sync.RWMutex
	apiNotify chan struct{}
	threadID  string // "main" for main thinker, thread ID for sub-threads

	// Telemetry — shared across all threads, owned by main thinker
	telemetry *Telemetry

	// Live MCP connections — servers connected at runtime
	mcpServers []MCPConn
	// In-process blob store. Used by mcpProxyHandler to intercept
	// binary tool results (rewriting them to compact handles the LLM
	// can reference) and to rehydrate those references on outbound
	// tool calls. Nil = passthrough (no binary-handle indirection).
	blobs *BlobStore
	// MCP server catalog — lightweight metadata for prompt (name + tool count)
	mcpCatalog []MCPServerInfo
	computer     computer.Computer // screen-based environment (nil = no computer use)
	pendingTools sync.Map         // tool call IDs with pending async results

	// Placeholders injected for tool calls that didn't finish within the
	// iter-boundary wait barrier. Keyed by call id → placeholderInfo.
	// When the real result eventually arrives, the tools.go publish path
	// routes it through a "late-result" text message instead of a
	// ToolResult (the tool_use is already paired with the placeholder).
	placeholdersSent sync.Map

	// Multimodal — parts waiting to be attached to next message
}

// placeholderInfo tracks a synthesised "⏳ in progress" tool_result that
// was injected at the iteration boundary because its real result didn't
// land in time. Used to (a) route late arrivals through the text-event
// path and (b) let the stale-placeholder sweeper emit a synthetic timeout
// message if the goroutine never returns.
type placeholderInfo struct {
	iteration    int
	toolName     string
	dispatchedAt time.Time
}

func NewThinker(apiKey string, provider LLMProvider, cfg ...*Config) *Thinker {
	var config *Config
	if len(cfg) > 0 && cfg[0] != nil {
		config = cfg[0]
	} else {
		config = NewConfig()
	}

	// Build provider pool from config (provider arg becomes default if pool is empty)
	pool, _ := buildProviderPool(config)
	if pool == nil {
		pool = &ProviderPool{providers: map[string]LLMProvider{}, order: []string{}}
	}
	// If a specific provider was passed in, ensure it's in the pool
	if provider != nil {
		name := provider.Name()
		if pool.Get(name) == nil {
			pool.providers[name] = provider
			pool.order = append([]string{name}, pool.order...)
		}
		if pool.default_ == "" {
			pool.default_ = name
		}
	}
	// Resolve the active provider from pool
	activeProvider := pool.Default()
	if activeProvider == nil {
		activeProvider = provider // fallback to passed-in provider
	}

	bus := NewEventBus()
	t := &Thinker{
		apiKey:   apiKey,
		pool:     pool,
		provider: activeProvider,
		messages: []Message{
			{Role: "system", Content: buildSystemPrompt(config.GetDirective(), config.GetMode(), nil, "", nil, nil, nil, nil)},
		},
		config:    config,
		bus:       bus,
		sub:       bus.Subscribe("main", 100),
		pause:     make(chan bool, 1),
		quit:      make(chan struct{}),
		rate:       RateSlow,
		agentRate:  RateSlow,
		agentSleep: 30 * time.Second,
		memory:    NewMemoryStore(apiKey),
		session:   NewSession(".", "main"),
		apiLog:    &[]APIEvent{},
		apiMu:     &sync.RWMutex{},
		apiNotify: make(chan struct{}, 1),
		threadID:   "main",
		maxHistory: maxHistoryMain,
		telemetry:  NewTelemetry(),
		blobs:      NewBlobStore(DefaultBlobMaxTotal, DefaultBlobTTL),
	}
	t.threads = NewThreadManager(t)
	t.registry = NewToolRegistry(apiKey)

	// Register system-only tools (for unconscious thread)
	registerSystemTools(t.registry, t.memory)

	// Rebuild system prompt now that registry exists (with core tool docs)
	t.messages[0] = Message{Role: "system", Content: buildSystemPrompt(config.GetDirective(), config.GetMode(), t.registry, "", nil, nil, t.pool, nil)}

	// Embed tool descriptions in background (non-blocking)
	go t.registry.EmbedAll(t.memory)

	// Main thread hooks
	t.handleTools = mainToolHandler(t)
	t.rebuildPrompt = func(toolDocs string) string {
		var threads []ThreadInfo
		if t.threads != nil {
			threads = t.threads.List()
		}
		return buildSystemPrompt(t.config.GetDirective(), t.config.GetMode(), t.registry, toolDocs, t.mcpServers, threads, t.pool, t.mcpCatalog)
	}

	// Connect MCP servers:
	// - main_access servers: fully registered (main can call them directly)
	// - non-main_access servers: catalog only (name + tool count for prompt, threads connect on demand)
	if len(config.MCPServers) > 0 {
		var mainServers []MCPServerConfig
		var catalogServers []MCPServerConfig
		for _, cfg := range config.MCPServers {
			if cfg.MainAccess {
				mainServers = append(mainServers, cfg)
			} else {
				catalogServers = append(catalogServers, cfg)
			}
		}
		// Fully connect main_access servers (gateway, channels, etc.)
		if len(mainServers) > 0 {
			t.mcpServers = connectAndRegisterMCP(mainServers, t.registry, t.memory, t.blobs)
		}
		// Discover catalog servers (connect, count tools, keep connection for thread reuse)
		for _, cfg := range catalogServers {
			srv, err := connectAnyMCP(cfg)
			if err != nil {
				logMsg("MCP-CATALOG", fmt.Sprintf("%s: connect error: %v", cfg.Name, err))
				continue
			}
			tools, err := srv.ListTools()
			if err != nil {
				logMsg("MCP-CATALOG", fmt.Sprintf("%s: list tools error: %v", cfg.Name, err))
				srv.Close()
				continue
			}
			t.mcpCatalog = append(t.mcpCatalog, MCPServerInfo{Name: cfg.Name, ToolCount: len(tools)})
			srv.Close() // don't keep connection — threads connect on demand
			logMsg("MCP-CATALOG", fmt.Sprintf("%s: %d tools cataloged (threads connect on demand)", cfg.Name, len(tools)))
		}
		// Rebuild prompt with catalog
		t.messages[0] = Message{Role: "system", Content: buildSystemPrompt(config.GetDirective(), config.GetMode(), t.registry, "", t.mcpServers, nil, t.pool, t.mcpCatalog)}
	}

	// Load conversation history from persistent session
	if saved, summaries := t.session.LoadTail(defaultLoadTail); len(saved) > 0 {
		// Prepend compacted summaries as context in system prompt
		if len(summaries) > 0 {
			contextBlock := "\n\n[PREVIOUS CONTEXT]\n"
			for _, s := range summaries {
				contextBlock += s + "\n"
			}
			t.messages[0].Content += contextBlock
		}
		// Append saved messages after system prompt
		t.messages = append(t.messages, saved...)
		logMsg("SESSION", fmt.Sprintf("loaded %d messages from history (%d compacted summaries)", len(saved), len(summaries)))
	}

	// Computer use environment is injected externally via SetComputer()

	// Respawn persistent threads from config, sorted by depth (parents before children).
	// DeferRun=true so all threads are created before any starts thinking.
	// This ensures parents see their children in [ACTIVE SUB-THREADS] on first iteration.
	persistedThreads := config.GetThreads()
	sort.Slice(persistedThreads, func(i, j int) bool {
		return persistedThreads[i].Depth < persistedThreads[j].Depth
	})
	for _, pt := range persistedThreads {
		parentID := pt.ParentID
		if parentID == "" || parentID == "main" {
			t.threads.SpawnWithOpts(pt.ID, pt.Directive, pt.Tools, SpawnOpts{
				ParentID: "main",
				Depth:    pt.Depth,
				DeferRun: true,
				MCPNames: pt.MCPNames,
			})
		} else {
			mgr := findThreadManager(t.threads, parentID)
			if mgr != nil {
				mgr.SpawnWithOpts(pt.ID, pt.Directive, pt.Tools, SpawnOpts{
					ParentID: parentID,
					Depth:    pt.Depth,
					DeferRun: true,
					MCPNames: pt.MCPNames,
				})
			} else {
				logMsg("RESPAWN", fmt.Sprintf("skipping thread %q: parent %q not found", pt.ID, parentID))
			}
		}
	}
	// Auto-spawn unconscious thread if enabled and not already persisted
	if config.Unconscious {
		hasUnconscious := false
		for _, pt := range persistedThreads {
			if pt.ID == "unconscious" {
				hasUnconscious = true
				break
			}
		}
		if !hasUnconscious {
			unconsciousDirective := `You are the unconscious. You maintain memory quality and extract skills silently.

Every cycle:
1. Scan memories with memory_scan. Edit unclear entries with memory_edit. Prune duplicates, stale, or noisy entries with memory_prune.
2. Review what has been learned. Extract recurring useful patterns as reusable skills with skill_write.

You never communicate with other threads. You never interact with users.
Your work surfaces naturally through improved recall and loaded skills.
Sleep 30 minutes between cycles.`
			t.threads.SpawnWithOpts("unconscious", unconsciousDirective,
				[]string{"memory_scan", "memory_edit", "memory_prune", "skill_write", "pace"},
				SpawnOpts{ParentID: "main", Depth: 0, DeferRun: true},
			)
			config.SaveThread(PersistentThread{
				ID: "unconscious", ParentID: "main", Depth: 0, System: true,
				Directive: unconsciousDirective,
				Tools:     []string{"memory_scan", "memory_edit", "memory_prune", "skill_write", "pace"},
			})
		}
	}

	// Now start all respawned threads (parents already see their children)
	if len(persistedThreads) > 0 || config.Unconscious {
		t.threads.StartAll()
	}

	return t
}

// findThreadManager walks the thread tree to find the ThreadManager that owns the given parent ID.
// Returns the Children manager of the parent thread, or nil if not found.
func findThreadManager(root *ThreadManager, parentID string) *ThreadManager {
	root.mu.RLock()
	defer root.mu.RUnlock()
	for _, thread := range root.threads {
		if thread.ID == parentID {
			return thread.Children // may be nil if parent is a leaf
		}
		// Recurse into children
		if thread.Children != nil {
			if found := findThreadManager(thread.Children, parentID); found != nil {
				return found
			}
		}
	}
	return nil
}

// mainToolHandler returns the tool handler for the main coordinating thread.
func mainToolHandler(t *Thinker) ToolHandler {
	return func(_ *Thinker, calls []toolCall, consumed []string) ([]string, []string, []ToolResult) {
		var replies []string
		var toolNames []string
		var results []ToolResult
		if len(calls) > 0 {
			names := make([]string, len(calls))
			for i, c := range calls {
				names[i] = c.Name
			}
			logMsg("TOOLS", fmt.Sprintf("[%s] handling %d tool call(s): %v", t.threadID, len(calls), names))
		}
		for _, call := range calls {
			// Check if this is an inline tool (handled here) or registry tool (handled by executeTool)
			isInline := true
			switch call.Name {
			case "spawn", "kill", "update", "send", "evolve", "remember", "pace", "connect", "disconnect", "list_connected", "done":
				// inline — we handle _reason and telemetry here
			default:
				isInline = false // executeTool handles _reason and telemetry
			}

			// Only strip _reason for inline tools — executeTool needs it
			reason := ""
			if isInline {
				reason = call.Args["_reason"]
				delete(call.Args, "_reason")
			}

			// Emit tool.call telemetry only for inline tools
			if isInline && t.telemetry != nil {
				t.telemetry.Emit("tool.call", t.threadID, ToolCallData{
					ID: call.NativeID, Name: call.Name, Args: call.Args, Reason: reason,
				})
			}
			// Helper to add inline tool result + emit telemetry
			addResult := func(content string) {
				if call.NativeID != "" {
					results = append(results, ToolResult{CallID: call.NativeID, Content: content})
				}
				if t.telemetry != nil {
					t.telemetry.Emit("tool.result", t.threadID, ToolResultData{
						ID: call.NativeID, Name: call.Name, Success: true, Result: content,
					})
				}
			}

			switch call.Name {
			case "spawn":
				id := call.Args["id"]
				directive := call.Args["directive"]
				if directive == "" {
					directive = call.Args["prompt"]
				}
				toolsStr := call.Args["tools"]
				var tools []string
				if toolsStr != "" {
					tools = strings.Split(toolsStr, ",")
				}
				mediaStr := call.Args["media"]
				mediaParts := parseMediaURLs(mediaStr)
				providerName := call.Args["provider"]
				// MCP scoping: thread connects only to listed servers
				var mcpNames []string
				if mcpStr := call.Args["mcp"]; mcpStr != "" {
					for _, name := range strings.Split(mcpStr, ",") {
						if n := strings.TrimSpace(name); n != "" {
							mcpNames = append(mcpNames, n)
						}
					}
				}
				// Provider builtin scoping
				var builtinTools []string
				if btStr, hasBuiltins := call.Args["builtins"]; hasBuiltins {
					if btStr == "" {
						builtinTools = []string{} // explicit empty = no builtins
					} else {
						for _, bt := range strings.Split(btStr, ",") {
							if b := strings.TrimSpace(bt); b != "" {
								builtinTools = append(builtinTools, b)
							}
						}
					}
				}
				if id == "" || directive == "" {
					logMsg("SPAWN", fmt.Sprintf("skip: missing id=%q or directive_len=%d in LLM call", id, len(directive)))
					addResult(fmt.Sprintf("error: spawn requires both id and directive (got id=%q, directive_len=%d)", id, len(directive)))
				} else {
					logMsg("SPAWN", fmt.Sprintf("LLM-requested id=%q tools=%v mcp=%v provider=%q builtins=%v directive_len=%d",
						id, tools, mcpNames, providerName, builtinTools, len(directive)))
					err := t.threads.SpawnWithOpts(id, directive, tools, SpawnOpts{
						MediaParts:   mediaParts,
						ProviderName: providerName,
						ParentID:     "main",
						Depth:        0,
						MCPNames:     mcpNames,
						BuiltinTools: builtinTools,
					})
					if err != nil {
						logMsg("SPAWN", fmt.Sprintf("FAILED id=%q: %v", id, err))
						addResult(fmt.Sprintf("error: %v", err))
					} else {
						logMsg("SPAWN", fmt.Sprintf("OK id=%q", id))
						t.config.SaveThread(PersistentThread{ID: id, ParentID: "main", Depth: 0, Directive: directive, Tools: tools, MCPNames: mcpNames})
						addResult(fmt.Sprintf("thread %s spawned", id))
					}
				}
				toolNames = append(toolNames, call.Raw)
			case "kill":
				id := call.Args["id"]
				if id == "" {
					addResult("error: kill requires id")
				} else {
					t.threads.Kill(id)
					t.config.RemoveThread(id)
					addResult(fmt.Sprintf("thread %s killed", id))
				}
				toolNames = append(toolNames, call.Raw)
			case "update":
				id := call.Args["id"]
				directive := call.Args["directive"]
				toolsStr := call.Args["tools"]
				if id == "" {
					addResult("error: update requires id")
				} else {
					var tools []string
					if toolsStr != "" {
						tools = strings.Split(toolsStr, ",")
					}
					if err := t.threads.Update(id, directive, tools); err != nil {
						addResult(fmt.Sprintf("error: %v", err))
					} else {
						if directive != "" {
							t.threads.Send(id, fmt.Sprintf("[directive updated] %s", directive))
						}
						addResult(fmt.Sprintf("thread %s updated", id))
					}
				}
				toolNames = append(toolNames, call.Raw)
			case "send":
				id := call.Args["id"]
				msg := call.Args["message"]
				mediaStr := call.Args["media"]
				if id == "" || msg == "" {
					addResult(fmt.Sprintf("error: send requires both id and message (got id=%q, message_len=%d)", id, len(msg)))
				} else {
					parts := parseMediaURLs(mediaStr)
					if !t.threads.SendWithParts(id, msg, parts) {
						addResult(fmt.Sprintf("error: thread %q not found", id))
					} else {
						if t.telemetry != nil {
							t.telemetry.Emit("thread.message", "main", ThreadMessageData{From: "main", To: id, Message: msg})
						}
						addResult(fmt.Sprintf("sent to %s", id))
					}
				}
				toolNames = append(toolNames, call.Raw)
			case "evolve":
				d := call.Args["directive"]
				if d == "" {
					addResult("error: evolve requires directive")
				} else {
					t.config.SetDirective(d)
					t.messages[0] = Message{Role: "system", Content: buildSystemPrompt(d, t.config.GetMode(), t.registry, "", t.mcpServers, nil, t.pool, t.mcpCatalog)}
					t.logAPI(APIEvent{Type: "evolved", ThreadID: "main", Message: d})
					if t.telemetry != nil {
						t.telemetry.Emit("directive.evolved", t.threadID, DirectiveChangeData{New: d})
					}
					addResult("directive updated")
				}
			case "remember":
				text := call.Args["text"]
				if text == "" {
					addResult("error: remember requires text")
				} else if t.memory == nil {
					addResult("error: memory is not configured")
				} else {
					// Synchronous so the LLM actually learns whether the
					// memory was stored. Previously async fire-and-forget
					// would emit "stored" before the embedding call and
					// silently drop failures.
					if err := t.memory.Store(text); err != nil {
						addResult(fmt.Sprintf("error: %v", err))
					} else {
						addResult("stored")
					}
				}
			case "pace":
				var parts []string
				if s := call.Args["sleep"]; s != "" {
					if d, ok := parseSleepDuration(s); ok {
						t.agentSleep = d
						t.agentRate = RateSleep
						parts = append(parts, "sleep="+s)
					}
				} else if r, ok := rateNames[call.Args["rate"]]; ok {
					t.agentRate = r
					if d, ok2 := rateAliases[call.Args["rate"]]; ok2 {
						t.agentSleep = d
					}
					parts = append(parts, "rate="+call.Args["rate"])
				}
				if m, ok := modelNames[call.Args["model"]]; ok {
					t.agentModel = m
					parts = append(parts, "model="+call.Args["model"])
				}
				if pn := call.Args["provider"]; pn != "" && t.pool != nil {
					if p := t.pool.Get(pn); p != nil {
						t.provider = p
						parts = append(parts, "provider="+pn)
					}
				}
				if len(parts) > 0 {
					addResult("set " + strings.Join(parts, " "))
				} else {
					addResult("ok")
				}
			case "connect":
				name := call.Args["name"]
				command := call.Args["command"]
				argsStr := call.Args["args"]
				url := call.Args["url"]
				transport := call.Args["transport"]
				toolNames = append(toolNames, call.Raw)

				func() {
					if name == "" {
						// Silent no-op was hiding model confusion —
						// always emit a result so the tool_use is paired
						// and the model sees the error on its next turn.
						addResult("error: connect requires name=\"<server>\"")
						return
					}
					// Catalog fallback: if the model omitted command/url
					// but we already know this server from config (the
					// catalog shown to main in [AVAILABLE MCP SERVERS]),
					// use the stored config. This is what the model
					// usually "means" when it tries connect name=<catalog
					// name> — promote the server to main instead of
					// asking it to re-guess transport details the host
					// already knows.
					if command == "" && url == "" && t.config != nil {
						for _, sc := range t.config.GetMCPServers() {
							if sc.Name == name {
								command = sc.Command
								url = sc.URL
								transport = sc.Transport
								if len(sc.Args) > 0 && argsStr == "" {
									argsStr = strings.Join(sc.Args, ",")
								}
								break
							}
						}
					}
					if command == "" && url == "" {
						addResult(fmt.Sprintf("error: unknown server %q — either pass command=... (stdio) or url=... (http), or use a name listed in [AVAILABLE MCP SERVERS]", name))
						return
					}
					// Reject re-connect of an already-attached server so
					// the model gets a clear "already done" signal
					// instead of silently duplicating state.
					for _, srv := range t.mcpServers {
						if srv.GetName() == name {
							addResult(fmt.Sprintf("already connected to %s (use list_connected to see current servers)", name))
							return
						}
					}
					var mcpArgs []string
					if argsStr != "" {
						mcpArgs = strings.Split(argsStr, ",")
					}
					cfg := MCPServerConfig{Name: name, Command: command, Args: mcpArgs, URL: url, Transport: transport}
					srv, err := connectAnyMCP(cfg)
					if err != nil {
						addResult(fmt.Sprintf("error: %v", err))
						return
					}
					tools, err := srv.ListTools()
					if err != nil {
						srv.Close()
						addResult(fmt.Sprintf("error: %v", err))
						return
					}
					t.mcpServers = append(t.mcpServers, srv)
					for _, tool := range tools {
						fullName := name + "_" + tool.Name
						syntax := buildMCPSyntax(fullName, tool.InputSchema)
						t.registry.Register(&ToolDef{
							Name:        fullName,
							Description: fmt.Sprintf("[%s] %s", name, tool.Description),
							Syntax:      syntax,
							Rules:       fmt.Sprintf("Provided by MCP server '%s'.", name),
							Handler:     mcpProxyHandler(srv, tool.Name, t.blobs),
							InputSchema: tool.InputSchema,
							MCP:         true,
							MCPServer:   name,
						})
					}
					if t.memory != nil {
						go func(srvName string, srvTools []mcpToolDef) {
							for _, tl := range srvTools {
								fullName := srvName + "_" + tl.Name
								emb, err := t.memory.embed(fullName + ": " + tl.Description)
								if err == nil {
									td := t.registry.Get(fullName)
									if td != nil {
										td.Embedding = emb
									}
								}
							}
						}(name, tools)
					}
					t.config.SaveMCPServer(cfg)
					addResult(fmt.Sprintf("connected to %s: %d tools", name, len(tools)))
				}()
			case "disconnect":
				name := call.Args["name"]
				if name != "" {
					found := false
					for i, srv := range t.mcpServers {
						if srv.GetName() == name {
							srv.Close()
							t.mcpServers = append(t.mcpServers[:i], t.mcpServers[i+1:]...)
							t.registry.RemoveByMCPServer(name)
							t.config.RemoveMCPServer(name)
							found = true
							break
						}
					}
					if found {
						addResult(fmt.Sprintf("disconnected from %s", name))
					} else {
						addResult(fmt.Sprintf("server %q not found", name))
					}
				}
				toolNames = append(toolNames, call.Raw)
			case "list_connected":
				var names []string
				for _, srv := range t.mcpServers {
					names = append(names, srv.GetName())
				}
				addResult(fmt.Sprintf("%d servers: %s", len(names), strings.Join(names, ", ")))
			default:
				// Dispatch to registry (MCP tools, etc)
				executeTool(t, call)
				toolNames = append(toolNames, call.Raw)
			}
		}
		return replies, toolNames, results
	}
}

func (t *Thinker) Run() {
	defer func() {
		if t.onStop != nil {
			t.onStop()
		}
	}()

	for {
		// Check pause/quit
		select {
		case <-t.quit:
			return
		case p := <-t.pause:
			t.paused = p
			if t.paused {
				select {
				case p = <-t.pause:
					t.paused = p
				case <-t.quit:
					return
				}
			}
		default:
		}

		t.iteration++
		logMsg("RUN", fmt.Sprintf("[%s] iteration #%d start, rate=%s", t.threadID, t.iteration, t.rate.String()))

		// Drain events from bus, optionally filter/route
		drained := t.drainEvents()

		// Extract text strings, collect media parts, and separate tool results
		var consumed []string
		var mediaParts []ContentPart
		var toolResults []ToolResult
		for _, de := range drained {
			consumed = append(consumed, de.Text)
			mediaParts = append(mediaParts, de.Parts...)
			if de.ToolResult != nil {
				toolResults = append(toolResults, *de.ToolResult)
			}
		}

		// --- Iter-boundary wait barrier for parallel async tool calls ---
		// Without this, when the previous iteration dispatched N parallel
		// tool calls and only some of their results landed before the
		// first Wake, the half-finished batch would reach think() and
		// the model would retry the "missing" ones. The barrier drains
		// additional events as they arrive, up to a short deadline, and
		// for anything still pending after the deadline it injects a
		// placeholder tool_result (see injectPlaceholdersForPending) so
		// the tool_use is properly paired and the model is told not to
		// retry.
		t.waitForPendingTools(&toolResults, &consumed, &mediaParts, 3*time.Second)
		if t.pendingToolCount() > 0 {
			injectedBefore := len(toolResults)
			t.injectPlaceholdersForPending(&toolResults)
			if injected := len(toolResults) - injectedBefore; injected > 0 {
				logMsg("RUN", fmt.Sprintf("[%s] injected %d in-progress placeholders for tools still running", t.threadID, injected))
			}
		}
		t.sweepStalePlaceholders()

		if len(consumed) > 0 {
			logMsg("RUN", fmt.Sprintf("[%s] drained %d events (media_parts=%d)", t.threadID, len(consumed), len(mediaParts)))
			for i, ev := range consumed {
				preview := ev
				if len(preview) > 100 {
					preview = preview[:100] + "..."
				}
				logMsg("RUN", fmt.Sprintf("[%s]   event[%d]: %s", t.threadID, i, preview))

				// Telemetry: emit each drained event (skip tool results — those have their own telemetry)
				if t.telemetry != nil && !strings.HasPrefix(ev, "[tool:") {
					source := "bus"
					if strings.HasPrefix(ev, "[console]") {
						source = "console"
					} else if strings.HasPrefix(ev, "[from:") {
						source = "thread"
					} else if strings.HasPrefix(ev, "[webhook:") || strings.HasPrefix(ev, "[subscription:") {
						source = "webhook"
					}
					t.telemetry.Emit("event.received", t.threadID, map[string]string{
						"source":  source,
						"message": preview,
					})
				}
			}
		}
		// Only go reactive for non-tool events (user messages, console, thread sends)
		hasExternalEvent := false
		for _, ev := range consumed {
			if !strings.HasPrefix(ev, "[tool:") {
				hasExternalEvent = true
				break
			}
		}

		hadEvents := len(consumed) > 0
		if hasExternalEvent {
			t.rate = RateReactive
			t.model = ModelMedium
		} else if hadEvents {
			// Tool results — wake but less aggressive than external events
			t.rate = RateFast
		}

		now := time.Now().Format("2006-01-02 15:04:05")

		// If we have tool results, add them as a proper tool_result message first
		if len(toolResults) > 0 {
			trMsg := Message{Role: "user", ToolResults: toolResults}
			t.messages = append(t.messages, trMsg)
			if t.session != nil {
				t.session.AppendMessage(trMsg, t.iteration, TokenUsage{})
			}
		}

		if hadEvents {
			// Filter out tool result text from the events text (they're already in ToolResults)
			var textEvents []string
			for _, ev := range consumed {
				if len(toolResults) > 0 && strings.HasPrefix(ev, "[tool:computer_use]") {
					continue // skip, already handled as ToolResult
				}
				textEvents = append(textEvents, ev)
			}

			var sb strings.Builder
			if len(textEvents) > 0 {
				sb.WriteString(fmt.Sprintf("[%s] Events:\n", now))
				for _, ev := range textEvents {
					sb.WriteString("• " + ev + "\n")
				}
			}
			if sb.Len() > 0 || len(mediaParts) > 0 {
				msg := Message{Role: "user", Content: sb.String()}
				if len(mediaParts) > 0 {
					msg.Parts = append([]ContentPart{{Type: "text", Text: sb.String()}}, mediaParts...)
				}
				t.messages = append(t.messages, msg)
				if t.session != nil {
					t.session.AppendMessage(msg, t.iteration, TokenUsage{})
				}
			}
		} else if len(toolResults) == 0 {
			// Only add "no events" if we also have no tool results
			t.messages = append(t.messages, Message{Role: "user", Content: fmt.Sprintf("[%s] (no events)", now)})
		}

		// Memory recall
		if t.memory != nil && t.memory.Count() > 0 {
			var memQuery string
			if hadEvents {
				memQuery = strings.Join(consumed, " ")
			} else {
				for i := len(t.messages) - 1; i >= 0; i-- {
					if t.messages[i].Role == "assistant" {
						memQuery = t.messages[i].Content
						break
					}
				}
			}
			if memQuery != "" {
				// Namespace-aware recall: thread sees own memories + global
				recalled := t.memory.RetrieveWithNamespace(memQuery, recallTopN, t.threadID)
				if ctx := t.memory.BuildContext(recalled); ctx != "" {
					t.messages = append(t.messages, Message{Role: "system", Content: ctx})
				}
			}
		}

		// Tool discovery via RAG — update system prompt with discovered tools
		if t.registry != nil && t.rebuildPrompt != nil {
			var toolQuery string
			if hadEvents {
				toolQuery = strings.Join(consumed, " ")
			} else {
				for i := len(t.messages) - 1; i >= 0; i-- {
					if t.messages[i].Role == "assistant" {
						toolQuery = t.messages[i].Content
						break
					}
				}
			}
			tools := t.registry.Retrieve(toolQuery, 5, t.allowedTools(), t.memory)
			toolDocs := t.registry.BuildDocs(tools)
			t.messages[0] = Message{Role: "system", Content: t.rebuildPrompt(toolDocs)}
		}

		start := time.Now()
		chatResp, err := t.think()

		// Fallback: if the provider errored and we have alternatives, try next in pool
		if err != nil && t.pool != nil && t.pool.Count() > 1 {
			original := t.provider.Name()
			if fb := t.pool.Fallback(original); fb != nil {
				logMsg("FALLBACK", fmt.Sprintf("[%s] %s failed (%v), trying %s", t.threadID, original, err, fb.Name()))
				t.provider = fb
				chatResp, err = t.think()
				if err != nil {
					// Restore original provider for next iteration
					t.provider = t.pool.Get(original)
				}
			}
		}

		duration := time.Since(start)
		reply := chatResp.Text
		usage := chatResp.Usage

		if err != nil {
			t.bus.Publish(Event{Type: EventThinkError, From: t.threadID, Error: err, Iteration: t.iteration})
			if t.telemetry != nil {
				t.telemetry.Emit("llm.error", t.threadID, LLMErrorData{
					Model: t.modelID(), Error: err.Error(), Iteration: t.iteration,
				})
			}
			select {
			case <-time.After(5 * time.Second):
			case <-t.quit:
				return
			}
			continue
		}

		// Build assistant message — may include native tool calls
		assistantMsg := Message{Role: "assistant", Content: reply, ToolCalls: chatResp.ToolCalls}
		t.messages = append(t.messages, assistantMsg)

		// Persist to session history
		if t.session != nil {
			t.session.AppendMessage(assistantMsg, t.iteration, usage)
		}

		// Log server-executed built-in tool results (code execution, etc.)
		for _, sr := range chatResp.ServerResults {
			logMsg("BUILTIN", fmt.Sprintf("server tool %s: output=%s err=%s", sr.ToolName, truncateStr(sr.Output, 200), sr.Error))
			if t.telemetry != nil {
				t.telemetry.Emit("builtin.result", t.threadID, map[string]any{
					"tool":   sr.ToolName,
					"output": sr.Output,
					"error":  sr.Error,
				})
			}
		}

		// Log and stream native tool calls
		if len(chatResp.ToolCalls) > 0 {
			var names []string
			for _, ntc := range chatResp.ToolCalls {
				names = append(names, ntc.Name)
			}
			logMsg("RUN", fmt.Sprintf("[%s] LLM returned %d tool calls: %v", t.threadID, len(chatResp.ToolCalls), names))
			for _, ntc := range chatResp.ToolCalls {
				summary := "\n→ " + ntc.Name + "("
				first := true
				for k, v := range ntc.Args {
					if !first {
						summary += ", "
					}
					if len(v) > 60 {
						v = v[:60] + "..."
					}
					summary += k + "=" + v
					first = false
				}
				summary += ")"
				t.bus.Publish(Event{Type: EventChunk, From: t.threadID, Text: summary, Iteration: t.iteration})
			}
		}

		// Dispatch tool calls via handler
		// Prefer native tool calls; fall back to text parsing if none
		var calls []toolCall
		if len(chatResp.ToolCalls) > 0 {
			for _, ntc := range chatResp.ToolCalls {
				// Intercept computer_use calls — execute via Computer interface with image ToolResults
				if isComputerUseTool(ntc.Name) && t.computer != nil {
					go t.executeComputerAction(ntc)
					continue
				}
				calls = append(calls, toolCall{Name: ntc.Name, Args: ntc.Args, Raw: ntc.Name, NativeID: ntc.ID})
			}
		}
		// NOTE: text-based [[...]] parsing removed — all providers use native tool calling now
		var replies []string
		var toolNames []string
		var inlineResults []ToolResult
		if t.handleTools != nil {
			replies, toolNames, inlineResults = t.handleTools(t, calls, consumed)
		}

		// Inject results for inline-handled tools (pace, spawn, kill, etc.)
		// so providers like Anthropic see matching tool_result for every tool_use
		if len(inlineResults) > 0 {
			t.messages = append(t.messages, Message{Role: "user", ToolResults: inlineResults})
			if t.session != nil {
				t.session.AppendMessage(Message{Role: "user", ToolResults: inlineResults}, t.iteration, TokenUsage{})
			}
		}

		// Sliding window — keep tool_use/tool_result pairs together
		maxHist := t.maxHistory
		if maxHist <= 0 {
			maxHist = maxHistoryMain // fallback
		}
		if len(t.messages) > maxHist+1 {
			start := len(t.messages) - maxHist
			// Don't start on a tool_result message (orphaned result)
			for start > 1 && len(t.messages[start].ToolResults) > 0 {
				start--
			}
			t.messages = append(t.messages[:1], t.messages[start:]...)
			// Sanitize any remaining orphaned tool_results after trimming
			// (no pending IDs needed here — this runs during the same iteration)
		}

		// Evict old screenshots — keep the 3 most recent images
		imageCount := 0
		maxImages := 3
		for i := len(t.messages) - 1; i >= 1; i-- {
			for j := range t.messages[i].ToolResults {
				if t.messages[i].ToolResults[j].Image != nil {
					imageCount++
					if imageCount > maxImages {
						// Replace old screenshot with text placeholder
						t.messages[i].ToolResults[j].Image = nil
						t.messages[i].ToolResults[j].Content = "[previous screenshot replaced — see latest for current screen state]"
					}
				}
			}
		}

		// Compact session history if it's grown too large
		if t.session != nil && t.session.NeedsCompaction() {
			logMsg("SESSION", fmt.Sprintf("[%s] triggering compaction (count=%d)", t.threadID, t.session.Count()))
			t.session.Compact(func(text string) string {
				// Simple summary — truncate to key points (no LLM call to avoid cost)
				if len(text) > 2000 {
					text = text[:2000]
				}
				return fmt.Sprintf("Summary of %d earlier messages: %s", t.session.Count(), text)
			})
		}

		// After processing, fall back to agent's chosen rate/sleep
		// (external events already set reactive above for this iteration)
		t.rate = t.agentRate
		t.model = t.agentModel

		// Compute actual sleep duration: agentSleep takes priority, else rate enum
		sleepDur := t.agentSleep
		if sleepDur <= 0 {
			sleepDur = t.rate.Delay()
		}

		// Thread count (0 if no thread manager)
		threadCount := 0
		if t.threads != nil {
			threadCount = t.threads.Count()
		}

		// Context size
		ctxChars := 0
		for _, msg := range t.messages {
			ctxChars += len(msg.Content)
		}

		t.bus.Publish(Event{
			Type: EventThinkDone, From: t.threadID,
			Iteration: t.iteration, Duration: duration,
			ConsumedEvents: consumed, Usage: usage,
			ToolCalls: toolNames, Replies: replies,
			Rate: t.rate, SleepDuration: sleepDur, Model: t.model,
			MemoryCount: t.memory.Count(), ThreadCount: threadCount,
			ContextMsgs: len(t.messages), ContextChars: ctxChars,
		})

		// Log to API — include full reply so tool calls are visible too
		thoughtLog := strings.TrimSpace(reply)
		if len(thoughtLog) > 1000 {
			thoughtLog = thoughtLog[:1000] + "..."
		}
		t.logAPI(APIEvent{Type: "thought", Iteration: t.iteration, Message: thoughtLog, Duration: duration.Round(time.Millisecond).String()})
		for _, r := range replies {
			t.logAPI(APIEvent{Type: "reply", Message: r})
		}

		// Telemetry: llm.done with full data
		if t.telemetry != nil {
			model := t.modelID()
			t.telemetry.Emit("llm.done", t.threadID, LLMDoneData{
				Model:            model,
				TokensIn:         usage.PromptTokens,
				TokensCached:     usage.CachedTokens,
				TokensOut:        usage.CompletionTokens,
				DurationMs:       duration.Milliseconds(),
				// cost_usd intentionally omitted — server enriches with
				// canonical pricing at ingest so we're not double-booking
				// the model→cost knowledge in core.
				Iteration:        t.iteration,
				Rate:             formatSleep(sleepDur),
				ContextMsgs:      len(t.messages),
				ContextChars:     ctxChars,
				MaxContextTokens: ModelContextWindow(model),
				MemoryCount:      t.memory.Count(),
				ThreadCount:      threadCount,
				Message:          thoughtLog,
			})
		}

		// Check if session needs compaction (background, non-blocking)
		if t.session != nil && t.session.NeedsCompaction() {
			go t.session.Compact(nil) // nil = simple count-based summary, no LLM call for now
		}

		// Interruptible sleep — wakes on new event, quit, or pause
		logMsg("RUN", fmt.Sprintf("[%s] sleeping %s", t.threadID, formatSleep(sleepDur)))
		select {
		case <-time.After(sleepDur):
			logMsg("RUN", fmt.Sprintf("[%s] woke: timer expired", t.threadID))
		case <-t.sub.Wake:
			logMsg("RUN", fmt.Sprintf("[%s] woke: event received", t.threadID))
		case p := <-t.pause:
			t.paused = p
			logMsg("RUN", fmt.Sprintf("[%s] paused=%v during sleep", t.threadID, t.paused))
			if t.paused {
				// Block until unpaused or quit
				select {
				case p = <-t.pause:
					t.paused = p
					logMsg("RUN", fmt.Sprintf("[%s] resumed", t.threadID))
				case <-t.quit:
					return
				}
			}
		case <-t.quit:
			logMsg("RUN", fmt.Sprintf("[%s] woke: quit signal", t.threadID))
			return
		}
	}
}

func (t *Thinker) think() (ChatResponse, error) {
	if t.provider == nil {
		return ChatResponse{}, fmt.Errorf("no provider configured")
	}

	// Sanitize messages before every API call �� removes orphaned tool_use/tool_result pairs
	// Pass pending tool IDs so the sanitizer doesn't strip in-flight async results
	if len(t.messages) > 1 {
		pending := map[string]bool{}
		t.pendingTools.Range(func(k, v any) bool {
			if id, ok := k.(string); ok {
				pending[id] = true
			}
			return true
		})
		t.messages = append(t.messages[:1], sanitizeToolPairs(t.messages[1:], pending)...)
	}

	onChunk := func(chunk string) {
		t.bus.Publish(Event{Type: EventChunk, From: t.threadID, Text: chunk, Iteration: t.iteration})
		if t.telemetry != nil && chunk != "" {
			t.telemetry.EmitLive("llm.chunk", t.threadID, LLMChunkData{
				Text: chunk, Iteration: t.iteration,
			})
		}
	}

	// Build native tools from registry if provider supports it
	var nativeTools []NativeTool
	if t.provider != nil && t.provider.SupportsNativeTools() && t.registry != nil {
		nativeTools = t.registry.NativeTools(t.toolAllowlist)
	}

	// For Anthropic: add _display dimensions to computer_use tool params
	// so the provider can extract them for the native spec
	if t.computer != nil && t.provider != nil && t.provider.Name() == "anthropic" {
		display := t.computer.DisplaySize()
		logMsg("COMPUTER", fmt.Sprintf("injecting display dims for anthropic: %dx%d", display.Width, display.Height))
		for i, nt := range nativeTools {
			if nt.Name == "computer_use" {
				if nativeTools[i].Parameters == nil {
					nativeTools[i].Parameters = make(map[string]any)
				}
				nativeTools[i].Parameters["_display_width"] = display.Width
				nativeTools[i].Parameters["_display_height"] = display.Height
				break
			}
		}
	}

	onThinking := func(chunk string) {
		if t.telemetry != nil && chunk != "" {
			t.telemetry.EmitLive("llm.thinking", t.threadID, map[string]any{
				"text": chunk, "iteration": t.iteration,
			})
		}
	}

	onToolChunk := func(toolName, callID, chunk string) {
		t.bus.Publish(Event{Type: EventToolChunk, From: t.threadID, Text: chunk, ToolName: toolName, Iteration: t.iteration})
		if t.telemetry != nil {
			t.telemetry.EmitLive("llm.tool_chunk", t.threadID, map[string]any{
				"tool": toolName, "id": callID, "chunk": chunk, "iteration": t.iteration,
			})
		}
	}

	// Emit llm.start so the UI can show a "thinking" indicator before
	// any tokens arrive. Live-only — not stored in the DB.
	if t.telemetry != nil {
		t.telemetry.EmitLive("llm.start", t.threadID, map[string]any{
			"model":     t.modelID(),
			"iteration": t.iteration,
		})
	}

	// Bracket the provider call with enter/exit logs so we can see when
	// we go in and how long until we come out. Any "hang" on a spawn
	// request shows up here as an unbalanced enter with no exit.
	callStart := time.Now()
	logMsg("THINK", fmt.Sprintf("[%s] provider.Chat enter model=%s msgs=%d tools=%d",
		t.threadID, t.modelID(), len(t.messages), len(nativeTools)))
	resp, err := t.provider.Chat(t.messages, t.modelID(), nativeTools, onChunk, onThinking, onToolChunk)
	logMsg("THINK", fmt.Sprintf("[%s] provider.Chat exit model=%s dur=%s tool_calls=%d err=%v",
		t.threadID, t.modelID(), time.Since(callStart).Round(time.Millisecond), len(resp.ToolCalls), err))
	return resp, err
}

// drainEvents reads all pending events and wake signals from this thinker's bus subscription.
type drainedEvent struct {
	Text       string
	Parts      []ContentPart
	ToolResult *ToolResult
}

// drainEventTexts is a convenience for tests — returns just the text strings.
func (t *Thinker) drainEventTexts() []string {
	events := t.drainEvents()
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Text
	}
	return out
}

func (t *Thinker) drainEvents() []drainedEvent {
	var items []drainedEvent
	for {
		select {
		case ev := <-t.sub.C:
			if ev.Type == EventInbox {
				items = append(items, drainedEvent{Text: ev.Text, Parts: ev.Parts, ToolResult: ev.ToolResult})
			}
		case <-t.sub.Wake:
			continue
		default:
			return items
		}
	}
}

// pendingToolCount returns the number of in-flight async tool calls.
// Used by the iteration wait barrier to decide whether to poll.
func (t *Thinker) pendingToolCount() int {
	n := 0
	t.pendingTools.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// waitForPendingTools implements the iteration-boundary barrier that
// prevents the parallel-tool-call retry bug. Scenario:
//
//  1. LLM fires N parallel tool calls in one assistant message.
//  2. Goroutine A finishes fast, publishes its ToolResult.
//  3. The publish wakes sub.Wake → iter N+1 starts immediately.
//  4. drainEvents is non-blocking → captures only A's result.
//  5. Goroutines B, C, D are still running upstream at this instant.
//  6. iter N+1's think() sends a half-finished context to the LLM.
//  7. LLM rationalises "B/C/D missing results" as "retry B/C/D."
//
// This barrier inserts a bounded wait before think() runs: if any tool
// from the previous iteration is still in pendingTools, drain the bus
// repeatedly (absorbing events as they arrive) until either pendingTools
// is empty or the deadline fires. Any extracted events are appended to
// the caller's slices so they end up in t.messages as usual.
//
// Bounded to keep genuine long-running tools from freezing the main
// loop. When the deadline fires and some tools are still pending, the
// caller is expected to inject placeholder tool_results for them (see
// injectPlaceholdersForPending).
func (t *Thinker) waitForPendingTools(
	toolResults *[]ToolResult,
	consumed *[]string,
	mediaParts *[]ContentPart,
	deadline time.Duration,
) {
	if t.pendingToolCount() == 0 {
		return
	}
	start := time.Now()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	deadlineCh := time.After(deadline)
	for {
		// Drain whatever's in the bus right now.
		for {
			select {
			case ev := <-t.sub.C:
				if ev.Type == EventInbox {
					if ev.ToolResult != nil {
						*toolResults = append(*toolResults, *ev.ToolResult)
					}
					if ev.Text != "" {
						*consumed = append(*consumed, ev.Text)
					}
					if len(ev.Parts) > 0 {
						*mediaParts = append(*mediaParts, ev.Parts...)
					}
					continue
				}
			case <-t.sub.Wake:
				continue
			default:
			}
			break
		}
		if t.pendingToolCount() == 0 {
			logMsg("RUN", fmt.Sprintf("[%s] pending tools drained in %s", t.threadID, time.Since(start)))
			return
		}
		select {
		case <-deadlineCh:
			logMsg("RUN", fmt.Sprintf("[%s] pending tool wait deadline (%s) — %d still in-flight, injecting placeholders", t.threadID, deadline, t.pendingToolCount()))
			return
		case <-poll.C:
			continue
		case <-t.quit:
			return
		}
	}
}

// injectPlaceholdersForPending synthesises a "⏳ in progress" ToolResult
// for every tool id still in pendingTools at the iteration boundary. This
// keeps each tool_use paired with a tool_result for API legality AND
// tells the model explicitly not to retry. When the real result later
// arrives from the goroutine, tools.go routes it through a distinct
// "late-result" text message (see late-result routing below) instead of
// appending a second ToolResult for the same id.
func (t *Thinker) injectPlaceholdersForPending(toolResults *[]ToolResult) {
	t.pendingTools.Range(func(k, v any) bool {
		id, ok := k.(string)
		if !ok || id == "" {
			return true
		}
		// Skip ids that already have a placeholder from an earlier
		// iteration — those are still in-flight, their placeholder is
		// already in the assistant/user message pair in history.
		if _, existed := t.placeholdersSent.Load(id); existed {
			return true
		}
		toolName, _ := v.(string)
		*toolResults = append(*toolResults, ToolResult{
			CallID:  id,
			Content: "⏳ In progress — this tool is still running from an earlier iteration. A [late-result] message will be delivered as soon as it completes. DO NOT call this tool again with the same arguments.",
		})
		t.placeholdersSent.Store(id, placeholderInfo{
			iteration:    t.iteration,
			toolName:     toolName,
			dispatchedAt: time.Now(),
		})
		return true
	})
}

// sweepStalePlaceholders emits a synthetic timeout late-result for any
// placeholder whose real goroutine never completed. Runs once per
// iteration; the default thresholds (5 minutes wall-clock or 20
// iterations) match the worst-case retry/backoff envelope of upstream
// MCP calls. Prevents placeholdersSent from growing unbounded when a
// tool genuinely hangs.
func (t *Thinker) sweepStalePlaceholders() {
	now := time.Now()
	var stale []string
	t.placeholdersSent.Range(func(k, v any) bool {
		id, ok1 := k.(string)
		info, ok2 := v.(placeholderInfo)
		if !ok1 || !ok2 {
			return true
		}
		if now.Sub(info.dispatchedAt) > 5*time.Minute || t.iteration-info.iteration > 20 {
			stale = append(stale, id)
			t.Inject(fmt.Sprintf("[late-result] Tool %s (call id=%s, dispatched iter %d) timed out after %s — no result ever arrived. Treat as failure.",
				info.toolName, id, info.iteration, now.Sub(info.dispatchedAt).Round(time.Second)))
		}
		return true
	})
	for _, id := range stale {
		t.placeholdersSent.Delete(id)
		// Don't delete from pendingTools — the goroutine may still
		// complete and we want its late-result path to fire naturally.
	}
}

func (t *Thinker) logAPI(ev APIEvent) {
	if t.apiNotify == nil || t.apiLog == nil {
		return
	}
	ev.Time = time.Now()
	if ev.ThreadID == "" {
		ev.ThreadID = t.threadID
	}
	t.apiMu.Lock()
	*t.apiLog = append(*t.apiLog, ev)
	if len(*t.apiLog) > 1000 {
		*t.apiLog = (*t.apiLog)[len(*t.apiLog)-500:]
	}
	t.apiMu.Unlock()
	select {
	case t.apiNotify <- struct{}{}:
	default:
	}
}

func (t *Thinker) APIEvents(since int) ([]APIEvent, int) {
	t.apiMu.RLock()
	defer t.apiMu.RUnlock()
	if since >= len(*t.apiLog) {
		return nil, len(*t.apiLog)
	}
	events := make([]APIEvent, len(*t.apiLog)-since)
	copy(events, (*t.apiLog)[since:])
	return events, len(*t.apiLog)
}

// allowedTools returns the tool allowlist for this thinker. nil = all tools allowed.
func (t *Thinker) allowedTools() map[string]bool {
	return t.toolAllowlist
}

func (t *Thinker) ReloadDirective() {
	directive := t.config.GetDirective()
	t.messages[0] = Message{Role: "system", Content: buildSystemPrompt(directive, t.config.GetMode(), t.registry, "", t.mcpServers, nil, t.pool, t.mcpCatalog)}
	t.InjectConsole("Directive updated to: " + directive + "\n\nAdjust the system accordingly — spawn, kill, or reconfigure threads as needed.")
}

// Inject sends a message event to this thinker's bus subscription.
func (t *Thinker) Inject(msg string) {
	logMsg("INJECT", fmt.Sprintf("to=%s msg=%s", t.threadID, msg))
	t.bus.Publish(Event{Type: EventInbox, To: t.threadID, Text: msg})
}

// InjectConsole sends a console event to this thinker.
func (t *Thinker) InjectConsole(msg string) {
	t.bus.Publish(Event{Type: EventInbox, To: t.threadID, Text: "[console] " + msg})
}

// InjectWithParts sends a text event with media parts attached.
func (t *Thinker) InjectWithParts(text string, parts []ContentPart) {
	if text == "" {
		text = "[multimodal input]"
	}
	t.bus.Publish(Event{Type: EventInbox, To: t.threadID, Text: "[console] " + text, Parts: parts})
}

// parseMediaURLs splits a space-separated list of URLs into ContentParts.
// Classifies each URL as image or audio by extension.
func parseMediaURLs(urls string) []ContentPart {
	if urls == "" {
		return nil
	}
	var parts []ContentPart
	for _, u := range strings.Fields(urls) {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		ext := ""
		if idx := strings.LastIndex(u, "."); idx >= 0 {
			ext = strings.ToLower(u[idx+1:])
			if qIdx := strings.Index(ext, "?"); qIdx >= 0 {
				ext = ext[:qIdx]
			}
		}
		switch ext {
		case "mp3", "wav", "aac", "ogg", "flac", "aiff", "m4a":
			parts = append(parts, ContentPart{Type: "audio_url", AudioURL: &AudioURL{URL: u}})
		case "png", "jpg", "jpeg", "gif", "webp":
			parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: u}})
		default:
			// Unknown extension — treat as image (provider will attempt fetch)
			parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: u}})
		}
	}
	return parts
}

func (t *Thinker) TogglePause() {
	newState := !t.paused
	// Non-blocking send — channel is buffered(1), drain any stale value first
	select {
	case <-t.pause:
	default:
	}
	t.pause <- newState
	t.paused = newState
	// Pause/resume all child threads too
	if t.threads != nil {
		t.threads.PauseAll(newState)
	}
}

// Shutdown releases external resources held by the thinker: currently
// only the computer-use browser session. Safe to call multiple times.
// Used by the main signal handler so SIGTERM/SIGINT closes Chrome
// (local) or REQUEST_RELEASEs the session (Browserbase) instead of
// orphaning it when the server SIGKILLs core during instance stop.
func (t *Thinker) Shutdown() {
	if t == nil {
		return
	}
	if c := t.computer; c != nil {
		t.computer = nil
		_ = c.Close()
	}
}

// SetComputer attaches a computer use environment to this thinker.
// Registers computer_use as a tool in the registry for non-Anthropic providers.
func (t *Thinker) SetComputer(c computer.Computer) {
	t.computer = c
	if c != nil && t.registry != nil {
		def := computer.GetComputerToolDef(c.DisplaySize())
		// Register computer_use — screen interaction (no navigate)
		comp := c
		t.registry.Register(&ToolDef{
			Name:        def.Name,
			Description: def.Description,
			Syntax:      def.Syntax,
			Rules:       def.Rules,
			InputSchema: def.Parameters,
			Handler: func(args map[string]string) ToolResponse {
				text, screenshot, err := computer.HandleComputerAction(comp, args)
				if err != nil {
					return ToolResponse{Text: fmt.Sprintf("error: %v", err)}
				}
				return ToolResponse{Text: text, Image: screenshot}
			},
		})

		// Register browser_session — session lifecycle (open/close/resume/status)
		sessionDef := computer.GetSessionToolDef()
		t.registry.Register(&ToolDef{
			Name:        sessionDef.Name,
			Description: sessionDef.Description,
			Syntax:      sessionDef.Syntax,
			Rules:       sessionDef.Rules,
			InputSchema: sessionDef.Parameters,
			Handler: func(args map[string]string) ToolResponse {
				text, screenshot, err := computer.HandleSessionAction(comp, args)
				if err != nil {
					return ToolResponse{Text: fmt.Sprintf("error: %v", err)}
				}
				return ToolResponse{Text: text, Image: screenshot}
			},
		})
	}
}

func (t *Thinker) Stop() {
	select {
	case <-t.quit:
	default:
		close(t.quit)
	}
	// Clean up computer session
	if t.computer != nil {
		t.computer.Close()
	}
}

// isComputerUseTool returns true if the tool name is a computer use tool from any provider.
func isComputerUseTool(name string) bool {
	switch name {
	case "computer_use", "computer", "computer_use_2025", "computer_20250124":
		return true
	}
	// Gemini native Computer Use actions
	return computer.IsGeminiComputerAction(name)
}

// normalizeComputerAction converts provider-specific args to a computer.Action.
func normalizeComputerAction(args map[string]string) computer.Action {
	action := computer.Action{Type: computer.NormalizeActionType(args["action"])}

	// Parse coordinate — providers use different formats
	// Anthropic: coordinate=[x, y] as string; OpenAI: x=400, y=300
	if coord := args["coordinate"]; coord != "" {
		// Parse "[400, 300]" format
		coord = strings.Trim(coord, "[] ")
		parts := strings.Split(coord, ",")
		if len(parts) == 2 {
			fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &action.X)
			fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &action.Y)
		}
	}
	if x := args["x"]; x != "" {
		fmt.Sscanf(x, "%d", &action.X)
	}
	if y := args["y"]; y != "" {
		fmt.Sscanf(y, "%d", &action.Y)
	}

	action.Text = args["text"]
	action.Key = args["key"]
	action.Direction = args["direction"]
	action.URL = args["url"]

	if d := args["duration"]; d != "" {
		fmt.Sscanf(d, "%d", &action.Duration)
	}

	// Set-of-Mark label. Providers may stringify a JSON integer as
	// unquoted "1" or quoted "\"1\""; strip quotes and parse.
	if lbl := strings.Trim(strings.TrimSpace(args["label"]), `"`); lbl != "" {
		fmt.Sscanf(lbl, "%d", &action.Label)
	}

	return action
}

// executeComputerAction runs a computer_use action and injects the result as a proper ToolResult.
func (t *Thinker) executeComputerAction(ntc NativeToolCall) {
	if ntc.ID != "" {
		t.pendingTools.Store(ntc.ID, ntc.Name)
		defer t.pendingTools.Delete(ntc.ID)
	}
	logMsg("COMPUTER", fmt.Sprintf("action=%s args=%v", ntc.Name, ntc.Args))
	reason := ntc.Args["_reason"]
	delete(ntc.Args, "_reason")

	// Emit tool.call telemetry
	if t.telemetry != nil {
		t.telemetry.Emit("tool.call", t.threadID, ToolCallData{
			ID: ntc.ID, Name: ntc.Name, Args: ntc.Args, Reason: reason,
		})
	}
	start := time.Now()

	var screenshot []byte
	var err error
	var actionLabel string

	// Gemini native Computer Use actions (click_at, type_text_at, etc.)
	if computer.IsGeminiComputerAction(ntc.Name) {
		var text string
		text, screenshot, err = computer.HandleGeminiComputerAction(t.computer, ntc.Name, ntc.Args)
		_ = text
		actionLabel = ntc.Name
	} else {
		// Anthropic/generic computer_use (single tool with "action" arg)
		action := normalizeComputerAction(ntc.Args)
		actionLabel = action.Type
		screenshot, err = t.computer.Execute(action)
	}

	duration := time.Since(start)

	if err != nil {
		logMsg("COMPUTER", fmt.Sprintf("error (%dms): %v", duration.Milliseconds(), err))
		if t.telemetry != nil {
			t.telemetry.Emit("tool.result", t.threadID, ToolResultData{
				ID: ntc.ID, Name: ntc.Name, DurationMs: duration.Milliseconds(), Success: false, Result: err.Error(),
			})
		}
		// Inject as tool result with error
		t.bus.Publish(Event{
			Type: EventInbox, To: t.threadID,
			Text: fmt.Sprintf("[tool:computer_use] error: %v", err),
			ToolResult: &ToolResult{
				CallID:  ntc.ID,
				Content: fmt.Sprintf("Error: %v", err),
				IsError: true,
			},
		})
		t.bus.Publish(Event{Type: EventChunk, From: t.threadID,
			Text: "\n← computer_use: error: " + err.Error() + "\n", Iteration: t.iteration})
		return
	}

	logMsg("COMPUTER", fmt.Sprintf("done (%dms) screenshot=%d bytes", duration.Milliseconds(), len(screenshot)))
	if t.telemetry != nil {
		t.telemetry.Emit("tool.result", t.threadID, ToolResultData{
			ID: ntc.ID, Name: ntc.Name, DurationMs: duration.Milliseconds(), Success: true,
			Result: fmt.Sprintf("screenshot %d bytes", len(screenshot)),
		})
	}

	// Inject as tool result with screenshot image
	t.bus.Publish(Event{
		Type: EventInbox, To: t.threadID,
		Text: fmt.Sprintf("[tool:computer_use] success: %s completed, screenshot attached (%d bytes, %dms)", actionLabel, len(screenshot), duration.Milliseconds()),
		ToolResult: &ToolResult{
			CallID:  ntc.ID,
			Content: fmt.Sprintf("Success: %s action completed. A screenshot of the current screen is attached as an image. Examine it to see the result.", actionLabel),
			Image:   screenshot,
		},
	})

	t.bus.Publish(Event{Type: EventChunk, From: t.threadID,
		Text: fmt.Sprintf("\n← computer_use: screenshot (%d bytes, %dms)\n", len(screenshot), duration.Milliseconds()),
		Iteration: t.iteration})
}

func encodeBase64(data []byte) string {
	return base64Encode(data)
}
