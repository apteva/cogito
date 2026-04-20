package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	aptcomputer "github.com/apteva/computer"
)

type APIServer struct {
	thinker   *Thinker
	startTime time.Time
	apiKey    string // if set, all endpoints except /health require auth
}

// apiAuth wraps a handler with API key authentication.
func (a *APIServer) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.apiKey != "" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+a.apiKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func startAPI(thinker *Thinker, addr string) error {
	api := &APIServer{
		thinker:   thinker,
		startTime: time.Now(),
		apiKey:    os.Getenv("APTEVA_API_KEY"),
	}
	if api.apiKey != "" {
		logMsg("API", "API key auth enabled")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.health) // always open
	mux.HandleFunc("/status", api.apiAuth(api.status))
	mux.HandleFunc("/threads", api.apiAuth(api.threads))
	mux.HandleFunc("/threads/", api.apiAuth(api.threadAction))
	mux.HandleFunc("/events", api.apiAuth(api.events))
	mux.HandleFunc("/pause", api.apiAuth(api.pause))
	mux.HandleFunc("/event", api.apiAuth(api.postEvent))
	mux.HandleFunc("/config", api.apiAuth(api.config))
	// Memory inspection/editing. GET lists, DELETE + PUT target a
	// single entry by zero-based index (matching memory_scan output
	// so UI indices line up with the agent's internal view).
	mux.HandleFunc("/memory", api.apiAuth(api.memoryList))
	mux.HandleFunc("/memory/", api.apiAuth(api.memoryItem))
	mux.Handle("/", http.FileServer(http.Dir("web")))
	return http.ListenAndServe(addr, mux)
}

func (a *APIServer) health(w http.ResponseWriter, r *http.Request) {
	logMsg("API", "GET /health")
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *APIServer) status(w http.ResponseWriter, r *http.Request) {
	logMsg("API", "GET /status")
	elapsed := time.Since(a.startTime)

	writeJSON(w, map[string]any{
		"uptime_seconds": int(elapsed.Seconds()),
		"iteration":      a.thinker.iteration,
		"rate":           formatSleep(a.thinker.agentSleep),
		"model":          a.thinker.model.String(),
		"threads":        a.thinker.threads.Count() + 1, // +1 for main
		"memories":       a.thinker.memory.Count(),
		"paused":         a.thinker.paused,
		"mode":           a.thinker.config.GetMode(),
	})
}

type threadJSON struct {
	ID        string   `json:"id"`
	ParentID  string   `json:"parent_id,omitempty"`
	Depth     int      `json:"depth"`
	Directive string   `json:"directive,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	MCPNames  []string `json:"mcp_names,omitempty"`
	Iteration int      `json:"iteration"`
	Rate      string   `json:"rate"`
	Model     string   `json:"model"`
	Age       string   `json:"age"`
}

func (a *APIServer) threads(w http.ResponseWriter, r *http.Request) {
	logMsg("API", "GET /threads")
	// Main's MCP list = only the servers whose tools are actually live on
	// main's registry (t.mcpServers). Cataloged servers (t.mcpCatalog) are
	// deliberately excluded — main can't call them directly, so listing
	// them under main would mislead the user into thinking the agent is
	// using them. Sub-threads that spawn with mcp="X" are the ones that
	// actually use catalog entries, and those appear in their own rows
	// via tm.List() below with their own MCPNames populated.
	var mainMCPs []string
	for _, srv := range a.thinker.mcpServers {
		mainMCPs = append(mainMCPs, srv.GetName())
	}
	// Always include main
	out := []threadJSON{{
		ID:        "main",
		Directive: a.thinker.config.GetDirective(),
		MCPNames:  mainMCPs,
		Iteration: a.thinker.iteration,
		Rate:      a.thinker.rate.String(),
		Model:     a.thinker.model.String(),
		Age:       formatAge(time.Since(a.startTime)),
	}}

	// Recursively collect all threads (including sub-threads of leaders)
	var collectThreads func(tm *ThreadManager)
	collectThreads = func(tm *ThreadManager) {
		for _, t := range tm.List() {
			out = append(out, threadJSON{
				ID:        t.ID,
				ParentID:  t.ParentID,
				Depth:     t.Depth,
				Directive: t.Directive,
				Tools:     t.Tools,
				MCPNames:  t.MCPNames,
				Iteration: t.Iteration,
				Rate:      t.Rate.String(),
				Model:     t.Model.String(),
				Age:       formatAge(time.Since(t.Started)),
			})
			// Recurse into children
			if t.SubThreads > 0 {
				tm.mu.RLock()
				if thread, ok := tm.threads[t.ID]; ok && thread.Children != nil {
					tm.mu.RUnlock()
					collectThreads(thread.Children)
				} else {
					tm.mu.RUnlock()
				}
			}
		}
	}
	collectThreads(a.thinker.threads)
	writeJSON(w, out)
}

func (a *APIServer) threadAction(w http.ResponseWriter, r *http.Request) {
	// Extract thread ID from path: /threads/{id}
	id := strings.TrimPrefix(r.URL.Path, "/threads/")
	if id == "" {
		http.Error(w, "thread ID required", http.StatusBadRequest)
		return
	}
	logMsg("API", fmt.Sprintf("%s /threads/%s", r.Method, id))

	switch r.Method {
	case http.MethodDelete:
		if id == "main" {
			http.Error(w, "cannot kill main thread", http.StatusBadRequest)
			return
		}
		a.thinker.threads.Kill(id)
		a.thinker.config.RemoveThread(id)
		writeJSON(w, map[string]string{"status": "killed", "id": id})
	default:
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
	}
}

func (a *APIServer) events(w http.ResponseWriter, r *http.Request) {
	logMsg("API", "GET /events (SSE connect)")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	tel := a.thinker.telemetry

	// Skip to current position — only stream new events, no history replay
	_, cursor := tel.Events(0)

	// Stream new events as they arrive
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tel.notify:
			newEvents, newCursor := tel.Events(cursor)
			cursor = newCursor
			for _, ev := range newEvents {
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", data)
				// Flush each event immediately for real-time streaming
				flusher.Flush()
			}
		}
	}
}

func (a *APIServer) pause(w http.ResponseWriter, r *http.Request) {
	logMsg("API", "POST /pause")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	a.thinker.TogglePause()
	paused := a.thinker.paused
	if paused {
		a.thinker.telemetry.Emit("instance.paused", "main", map[string]string{"status": "paused"})
	} else {
		a.thinker.telemetry.Emit("instance.resumed", "main", map[string]string{"status": "running"})
	}
	writeJSON(w, map[string]bool{"paused": paused})
}

func (a *APIServer) postEvent(w http.ResponseWriter, r *http.Request) {
	logMsg("API", "POST /event")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message  json.RawMessage `json:"message"`
		ThreadID string          `json:"thread_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Parse message: string or []ContentPart
	var text string
	var parts []ContentPart

	if err := json.Unmarshal(body.Message, &text); err != nil {
		// Try array of content parts
		if err := json.Unmarshal(body.Message, &parts); err != nil {
			http.Error(w, "message must be a string or array of content parts", http.StatusBadRequest)
			return
		}
		// Extract text from parts for the event bus
		for _, p := range parts {
			if p.Type == "text" {
				text = p.Text
				break
			}
		}
	}

	if text == "" && len(parts) == 0 {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	threadID := body.ThreadID
	if threadID == "" {
		threadID = "main"
	}

	if len(parts) > 0 {
		// Multimodal: publish event with parts directly on the bus
		a.thinker.bus.Publish(Event{Type: EventInbox, To: threadID, Text: "[console] " + text, Parts: parts})
	} else if threadID != "main" {
		a.thinker.bus.Publish(Event{Type: EventInbox, To: threadID, Text: text})
	} else {
		a.thinker.InjectConsole(text)
	}

	writeJSON(w, map[string]string{"status": "injected", "thread_id": threadID})
}

func (a *APIServer) config(w http.ResponseWriter, r *http.Request) {
	logMsg("API", fmt.Sprintf("%s /config", r.Method))
	switch r.Method {
	case http.MethodGet:
		// Build live provider info
		var providerInfo map[string]any
		if a.thinker.provider != nil {
			models := a.thinker.provider.Models()
			providerInfo = map[string]any{
				"name": a.thinker.provider.Name(),
				"models": map[string]string{
					"large": models[ModelLarge],
					"small": models[ModelSmall],
				},
			}
		}
		// Build live computer info
		var computerInfo map[string]any
		if a.thinker.computer != nil {
			d := a.thinker.computer.DisplaySize()
			computerInfo = map[string]any{
				"connected": true,
				"display":   map[string]int{"width": d.Width, "height": d.Height},
			}
			if a.thinker.config.Computer != nil {
				computerInfo["type"] = a.thinker.config.Computer.Type
			}
		}
		// Build live MCP server info. Include every MCP in the persisted
		// config — both main-access (live connection in mcpServers) and
		// cataloged entries (connected on-demand per thread) — so the
		// dashboard sees the same list the user configured. Fall back to
		// config.mcp_servers as the source of truth; overlay "connected"
		// for anything currently alive in mcpServers.
		liveNames := make(map[string]bool, len(a.thinker.mcpServers))
		for _, srv := range a.thinker.mcpServers {
			liveNames[srv.GetName()] = true
		}
		var mcpInfo []map[string]any
		for _, cfg := range a.thinker.config.GetMCPServers() {
			entry := map[string]any{
				"name":        cfg.Name,
				"connected":   liveNames[cfg.Name],
				"main_access": cfg.MainAccess,
			}
			if cfg.Transport != "" {
				entry["transport"] = cfg.Transport
			}
			if cfg.URL != "" {
				entry["url"] = cfg.URL
			}
			if cfg.Command != "" {
				entry["command"] = cfg.Command
			}
			mcpInfo = append(mcpInfo, entry)
		}

		writeJSON(w, map[string]any{
			"directive":   a.thinker.config.GetDirective(),
			"mode":        a.thinker.config.GetMode(),
			"provider":    providerInfo,
			"providers":   a.thinker.config.GetProviders(),
			"computer":    computerInfo,
			"mcp_servers": mcpInfo,
		})
	case http.MethodPut:
		var body struct {
			Directive  string            `json:"directive,omitempty"`
			Mode       RunMode           `json:"mode,omitempty"`
			Provider   *ProviderConfig   `json:"provider,omitempty"`
			Providers  []ProviderConfig  `json:"providers,omitempty"`
			Computer    *ComputerConfig   `json:"computer,omitempty"`
			MCPServers  []MCPServerConfig `json:"mcp_servers,omitempty"`
			Reset      *struct {
				History bool `json:"history,omitempty"`
				Memory  bool `json:"memory,omitempty"`
				Threads bool `json:"threads,omitempty"`
			} `json:"reset,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.Directive != "" {
			a.thinker.config.SetDirective(body.Directive)
			a.thinker.ReloadDirective()
		}
		if body.Mode == ModeAutonomous || body.Mode == ModeCautious || body.Mode == ModeLearn {
			a.thinker.config.SetMode(body.Mode)
			if a.thinker.telemetry != nil {
				a.thinker.telemetry.Emit("mode.changed", "main", map[string]string{"mode": string(body.Mode)})
			}
		}
		logMsg("API", fmt.Sprintf("PUT /config: providers=%d provider=%v", len(body.Providers), body.Provider != nil))
		if len(body.Providers) > 0 {
			// Rebuild provider pool from new config
			logMsg("API", fmt.Sprintf("rebuilding pool with %d providers", len(body.Providers)))
			oldDefault := ""
			if a.thinker.provider != nil {
				oldDefault = a.thinker.provider.Name()
			}
			a.thinker.config.mu.Lock()
			a.thinker.config.Providers = body.Providers
			a.thinker.config.mu.Unlock()
			a.thinker.config.Save()
			pool, err := buildProviderPool(a.thinker.config)
			if err == nil && pool != nil {
				a.thinker.pool = pool
				a.thinker.provider = pool.Default()
				// Clear conversation history if provider changed (tool IDs are incompatible across providers)
				if a.thinker.provider.Name() != oldDefault {
					a.thinker.messages = a.thinker.messages[:1] // keep system prompt only
				}
			}
		}
		if body.Provider != nil {
			// Hot-swap provider if name changed
			if body.Provider.Name != "" {
				newProvider := createProviderByName(body.Provider.Name)
				if newProvider != nil {
					if body.Provider.Models != nil {
						applyModelOverrides(newProvider, body.Provider.Models)
					}
					a.thinker.provider = newProvider
					a.thinker.config.SetProvider(body.Provider)
				} else {
					http.Error(w, fmt.Sprintf("provider %q not available (missing API key?)", body.Provider.Name), http.StatusBadRequest)
					return
				}
			} else if body.Provider.Models != nil {
				// Just update models on current provider
				applyModelOverrides(a.thinker.provider, body.Provider.Models)
				// Merge into config
				for tier, modelID := range body.Provider.Models {
					a.thinker.config.SetProviderModel(tier, modelID)
				}
			}
		}
		if body.Computer != nil {
			// Hot-connect or disconnect computer environment
			if body.Computer.Type == "" {
				// Disconnect
				if a.thinker.computer != nil {
					a.thinker.computer.Close()
					a.thinker.computer = nil
				}
				a.thinker.config.mu.Lock()
				a.thinker.config.Computer = nil
				a.thinker.config.mu.Unlock()
				a.thinker.config.Save()
			} else {
				// Pick a provider-aware viewport default when the caller
				// didn't specify one. Anthropic's computer-use tool was
				// trained on 1024×768 and Anthropic's docs specifically
				// recommend keeping the screenshot at that exact size for
				// best click accuracy. For non-Anthropic providers (Kimi,
				// Gemini, etc.) we use 1600×800 — exact 2:1 widescreen,
				// wide enough for desktop layouts but small enough to
				// keep screenshot token counts modest on non-native
				// vision models.
				width, height := body.Computer.Width, body.Computer.Height
				if width == 0 || height == 0 {
					if a.thinker.provider != nil && a.thinker.provider.Name() == "anthropic" {
						width, height = 1024, 768
					} else {
						width, height = 1600, 800
					}
				}
				comp, err := aptcomputer.New(aptcomputer.Config{
					Type:      body.Computer.Type,
					URL:       body.Computer.URL,
					APIKey:    body.Computer.APIKey,
					ProjectID: body.Computer.ProjectID,
					Width:     width,
					Height:    height,
				})
				if err != nil {
					http.Error(w, fmt.Sprintf("computer: %v", err), http.StatusBadRequest)
					return
				}
				// Close old session if any
				if a.thinker.computer != nil {
					a.thinker.computer.Close()
				}
				a.thinker.SetComputer(comp)
				a.thinker.config.mu.Lock()
				a.thinker.config.Computer = body.Computer
				a.thinker.config.mu.Unlock()
				a.thinker.config.Save()
			}
		}
		if body.MCPServers != nil {
			a.reconcileMCP(body.MCPServers)
			// DO NOT rebuild t.mcpCatalog here — reconcileMCP already
			// manages it correctly (populates for non-main-access
			// servers in the connect pass at reconcileMCP:690, prunes
			// removed entries in the prune pass). The old code here
			// wiped the catalog and rebuilt it ONLY from t.mcpServers
			// (the main-access list), which had the effect of deleting
			// every catalog entry on every PUT /config — meaning an
			// agent whose catalog MCPs were attached at runtime never
			// saw them in its system prompt.
			//
			// Rebuild the system prompt so the updated `[AVAILABLE MCP
			// SERVERS]` block reaches the LLM on its next iteration.
			// Without this, the agent's system prompt stays frozen at
			// the boot-time state and new catalog MCPs attached via
			// dashboard are invisible to main. Use rebuildPrompt (which
			// is set up at thinker init) so all the pieces — directive,
			// core docs, providers, threads, MCPs — are consistent.
			if a.thinker.rebuildPrompt != nil {
				a.thinker.messages[0] = Message{
					Role:    "system",
					Content: a.thinker.rebuildPrompt(""),
				}
			}
		}
		if body.Reset != nil {
			logMsg("API", fmt.Sprintf("PUT /config reset: history=%v memory=%v threads=%v", body.Reset.History, body.Reset.Memory, body.Reset.Threads))
			if body.Reset.Threads {
				a.thinker.threads.KillAll()
				a.thinker.config.ClearThreads()
			}
			if body.Reset.History {
				if a.thinker.session != nil {
					a.thinker.session.Delete()
					a.thinker.session = NewSession(".", "main")
				}
				// Clear thread histories
				os.RemoveAll("history")
				os.MkdirAll("history", 0755)
			}
			if body.Reset.Memory && a.thinker.memory != nil {
				os.Remove(a.thinker.memory.path)
				a.thinker.memory.mu.Lock()
				a.thinker.memory.entries = nil
				a.thinker.memory.mu.Unlock()
			}
			// Reset message context to just system prompt
			if body.Reset.History {
				a.thinker.messages = a.thinker.messages[:1]
			}
		}
		writeJSON(w, map[string]string{"status": "updated"})
	default:
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
	}
}

// reconcileMCP diffs the desired MCP server list against the live state,
// connecting new servers, disconnecting removed ones, and replacing servers
// whose connection details (URL, command, args, transport, main_access)
// changed.
//
// Two kinds of "live" state are considered:
//   - t.mcpServers — servers with main_access=true, open connections, fully
//     registered tools in the instance registry.
//   - t.mcpCatalog — servers with main_access=false, no open connection.
//     Only the name and tool count are cached for the main thread's system
//     prompt; sub-threads open their own connections on demand at spawn.
//
// Matching reconciler behavior to instance-start semantics means:
//   - main_access=false entries get their tools listed once, the connection
//     closed, and only an mcpCatalog row kept.
//   - main_access=true entries get fully registered.
// systemMCPNames are entries injected by the server at spawn time
// (apteva-server gateway + per-instance channels MCP). The dashboard's GET
// /config returns them as summaries (name + connected only) because they
// carry dynamic URLs that aren't user-editable. If a client later PUTs the
// mcp_servers list back, those system entries come back in stripped form —
// reconcile must leave them untouched so we don't disconnect them and then
// fail to reconnect from incomplete configs. Users attach/detach only
// *their* MCP servers through the dashboard; system entries stay put.
var systemMCPNames = map[string]bool{
	"apteva-server":   true,
	"channels":        true,
	"apteva-channels": true,
}

func (a *APIServer) reconcileMCP(desired []MCPServerConfig) {
	// Strip system entries from the desired list so reconcile ignores them.
	filtered := make([]MCPServerConfig, 0, len(desired))
	for _, c := range desired {
		if systemMCPNames[c.Name] {
			continue
		}
		filtered = append(filtered, c)
	}
	desired = filtered

	names := make([]string, len(desired))
	for i, c := range desired {
		names[i] = c.Name
	}
	logMsg("API", fmt.Sprintf("reconcileMCP: %d desired servers (system entries preserved): %v", len(desired), names))
	t := a.thinker

	// Current config map lets us detect when the URL/command/args/transport
	// or main_access changed between reconciles — any change forces a
	// detach-then-reattach.
	currentCfg := make(map[string]MCPServerConfig)
	if t.config != nil {
		for _, c := range t.config.GetMCPServers() {
			currentCfg[c.Name] = c
		}
	}

	// Index desired by name
	want := make(map[string]MCPServerConfig, len(desired))
	for _, cfg := range desired {
		want[cfg.Name] = cfg
	}

	// For each server name currently known to exist, decide whether it
	// stays as-is, gets removed, or gets replaced (close + reconnect).
	// Replacement happens when the desired config differs from the current
	// one in any connection-level field.
	changed := func(old, new MCPServerConfig) bool {
		if old.URL != new.URL || old.Command != new.Command || old.Transport != new.Transport {
			return true
		}
		if old.MainAccess != new.MainAccess {
			return true
		}
		if len(old.Args) != len(new.Args) {
			return true
		}
		for i := range old.Args {
			if old.Args[i] != new.Args[i] {
				return true
			}
		}
		if len(old.Env) != len(new.Env) {
			return true
		}
		for k, v := range old.Env {
			if new.Env[k] != v {
				return true
			}
		}
		return false
	}

	// Disconnect live (main_access) servers that are either absent from
	// desired or whose config changed. System entries are never touched —
	// they're not user-editable and the desired list doesn't include them.
	var kept []MCPConn
	for _, srv := range t.mcpServers {
		name := srv.GetName()
		if systemMCPNames[name] {
			kept = append(kept, srv)
			continue
		}
		desiredCfg, stillWant := want[name]
		if stillWant && !changed(currentCfg[name], desiredCfg) {
			kept = append(kept, srv)
			continue
		}
		// Disconnect: either removed or replaced.
		srv.Close()
		t.config.RemoveMCPServer(name)
		t.registry.RemoveByMCPServer(name)
		if t.telemetry != nil {
			t.telemetry.Emit("mcp.disconnected", "api", map[string]string{"name": name})
		}
	}
	t.mcpServers = kept

	// Prune cataloged (non-main_access) servers that are either absent from
	// desired or whose config changed. Catalog entries have no live
	// connection, so we just rebuild the slice. System entries are never
	// touched.
	var newCatalog []MCPServerInfo
	for _, info := range t.mcpCatalog {
		if systemMCPNames[info.Name] {
			newCatalog = append(newCatalog, info)
			continue
		}
		desiredCfg, stillWant := want[info.Name]
		if stillWant && !changed(currentCfg[info.Name], desiredCfg) && !desiredCfg.MainAccess {
			newCatalog = append(newCatalog, info)
			continue
		}
		// Dropped or replaced — remove from config. Nothing to close.
		if !stillWant {
			t.config.RemoveMCPServer(info.Name)
			if t.telemetry != nil {
				t.telemetry.Emit("mcp.disconnected", "api", map[string]string{"name": info.Name})
			}
		}
	}
	t.mcpCatalog = newCatalog

	// Index what's now live after the prune pass so the connect loop
	// doesn't reprocess servers that survived untouched.
	live := make(map[string]bool, len(kept)+len(newCatalog))
	for _, srv := range kept {
		live[srv.GetName()] = true
	}
	for _, info := range newCatalog {
		live[info.Name] = true
	}

	// Connect new / replaced servers.
	for _, cfg := range desired {
		if live[cfg.Name] {
			continue
		}
		srv, err := connectAnyMCP(cfg)
		if err != nil {
			logMsg("MCP-RECONCILE", fmt.Sprintf("%s: connect error: %v", cfg.Name, err))
			continue
		}
		tools, err := srv.ListTools()
		if err != nil {
			srv.Close()
			logMsg("MCP-RECONCILE", fmt.Sprintf("%s: list tools error: %v", cfg.Name, err))
			continue
		}

		if cfg.MainAccess {
			// Full registration — tools become callable by main and threads
			// with matching allowlists. Connection stays open for the life
			// of the instance.
			t.mcpServers = append(t.mcpServers, srv)
			for _, tool := range tools {
				fullName := cfg.Name + "_" + tool.Name
				syntax := buildMCPSyntax(fullName, tool.InputSchema)
				t.registry.Register(&ToolDef{
					Name:        fullName,
					Description: fmt.Sprintf("[%s] %s", cfg.Name, tool.Description),
					Syntax:      syntax,
					Rules:       fmt.Sprintf("Provided by MCP server '%s'.", cfg.Name),
					Handler:     mcpProxyHandler(srv, tool.Name, t.blobs),
					InputSchema: tool.InputSchema,
					MCP:         !cfg.MainAccess,
					MCPServer:   cfg.Name,
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
				}(cfg.Name, tools)
			}
		} else {
			// Catalog mode — discover tool count, then close the connection.
			// Sub-threads open their own sessions on demand at spawn time
			// (see thread.go:308-374).
			t.mcpCatalog = append(t.mcpCatalog, MCPServerInfo{Name: cfg.Name, ToolCount: len(tools)})
			srv.Close()
			logMsg("MCP-RECONCILE", fmt.Sprintf("%s: cataloged (%d tools, threads connect on demand)", cfg.Name, len(tools)))
		}

		t.config.SaveMCPServer(cfg)
		if t.telemetry != nil {
			t.telemetry.Emit("mcp.connected", "api", map[string]string{
				"name":  cfg.Name,
				"tools": fmt.Sprintf("%d", len(tools)),
				"mode":  map[bool]string{true: "main", false: "catalog"}[cfg.MainAccess],
			})
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// memoryListItem is the UI-facing projection of a MemoryEntry.
// Embeddings are omitted (useless to the UI, ~6KB per entry), and
// the index is attached so callers can DELETE/PUT without guessing.
// tag is a best-effort extraction of the leading bracketed marker
// ("[preference]", "[correction]", etc.) the remember-tool guidance
// asks the agent to use — the UI uses it to color-group rows.
type memoryListItem struct {
	Index     int       `json:"index"`
	Text      string    `json:"text"`
	Tag       string    `json:"tag,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	Session   string    `json:"session,omitempty"`
	Time      time.Time `json:"time"`
}

func extractTag(text string) string {
	s := strings.TrimSpace(text)
	if len(s) < 3 || s[0] != '[' {
		return ""
	}
	end := strings.IndexByte(s, ']')
	if end <= 1 {
		return ""
	}
	return s[1:end]
}

// GET /memory — return every memory entry in store order.
func (a *APIServer) memoryList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if a.thinker.memory == nil {
		writeJSON(w, []memoryListItem{})
		return
	}
	entries := a.thinker.memory.List()
	out := make([]memoryListItem, len(entries))
	for i, e := range entries {
		out[i] = memoryListItem{
			Index:     i,
			Text:      e.Text,
			Tag:       extractTag(e.Text),
			Namespace: e.Namespace,
			Session:   e.Session,
			Time:      e.Time,
		}
	}
	writeJSON(w, out)
}

// /memory/{index} — DELETE prunes, PUT rewrites + recomputes embedding.
func (a *APIServer) memoryItem(w http.ResponseWriter, r *http.Request) {
	if a.thinker.memory == nil {
		http.Error(w, "memory store not initialized", http.StatusServiceUnavailable)
		return
	}
	idxStr := strings.TrimPrefix(r.URL.Path, "/memory/")
	idxStr = strings.TrimSuffix(idxStr, "/")
	if idxStr == "" {
		http.Error(w, "index required", http.StatusBadRequest)
		return
	}
	var idx int
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
		http.Error(w, "invalid index", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		// Delete is range-checked internally; silent no-op on bad index
		// matches the memory_prune tool's behavior so the UX is the same.
		a.thinker.memory.Delete(idx)
		writeJSON(w, map[string]any{"ok": true, "count": a.thinker.memory.Count()})

	case http.MethodPut:
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		text := strings.TrimSpace(body.Text)
		if text == "" {
			http.Error(w, "text required", http.StatusBadRequest)
			return
		}
		if err := a.thinker.memory.Update(idx, text); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
