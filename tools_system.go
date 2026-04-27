package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// registerSystemTools adds the unconscious-only tool surface for memory
// consolidation. Memory v2: main has zero memory tools. Only the
// unconscious thread (allowlisted to these by name) writes.
//
// Eight tools, in cognitive order:
//
//   review_history     — read recent main-thread activity since the
//                        last consolidation cycle.
//   memory_search      — fuzzy lookup of existing active memories
//                        (used to detect "have we already remembered
//                        this?" before writing).
//   memory_list        — paginated dump of active memories with ids
//                        + tags. Used at the start of a cycle for an
//                        overview, and to find drop/supersede targets.
//   memory_remember    — append a new memory.
//   memory_supersede   — replace an old memory with a new one + a
//                        reason. Old line stays on disk; recall skips it.
//   memory_drop        — tombstone a memory by id with a reason.
//
//   skill_write        — kept from the previous system; the unconscious
//                        also extracts reusable patterns into skills/.
//
// All inputs are validated; errors return a clear message the LLM can
// recover from.
func registerSystemTools(registry *ToolRegistry, memory *MemoryStore) {

	// ---- review_history ---------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "review_history",
		Description: "Read recent main-thread activity since your last consolidation cycle. Returns user messages, assistant turns, and tool results in chronological order. The raw material from which to extract memories.",
		Syntax:      `[[review_history limit="50"]]`,
		Rules:       "limit (optional) caps how many entries to return; default 50. Read this BEFORE deciding what to remember/supersede/drop — fresh material is the input to your judgment.",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			limit := 50
			if v := args["limit"]; v != "" {
				_, _ = fmt.Sscanf(v, "%d", &limit)
				if limit <= 0 || limit > 500 {
					limit = 50
				}
			}
			path := filepath.Join("history", "main.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				return ToolResponse{Text: fmt.Sprintf("(no history yet: %v)", err)}
			}
			lines := strings.Split(string(data), "\n")
			// Take last `limit` non-empty lines so the unconscious sees
			// recent activity even when the file is huge.
			var kept []string
			for i := len(lines) - 1; i >= 0 && len(kept) < limit; i-- {
				if strings.TrimSpace(lines[i]) == "" {
					continue
				}
				kept = append([]string{lines[i]}, kept...)
			}
			return ToolResponse{Text: strings.Join(kept, "\n")}
		},
	})

	// ---- memory_search ---------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "memory_search",
		Description: "Search active memories by relevance to a query. Returns id, content, tags, weight, age. Use BEFORE remember to check if a similar memory already exists.",
		Syntax:      `[[memory_search query="user's deployment preferences" limit="10"]]`,
		Rules:       "Active memories only (tombstoned/superseded are skipped). Embedding-based when a backend is configured, lexical otherwise. Always returns ids you can later supersede or drop.",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			if memory == nil {
				return ToolResponse{Text: "error: no memory store"}
			}
			query := args["query"]
			if query == "" {
				return ToolResponse{Text: "error: query required"}
			}
			limit := 10
			if v := args["limit"]; v != "" {
				_, _ = fmt.Sscanf(v, "%d", &limit)
				if limit <= 0 || limit > 100 {
					limit = 10
				}
			}
			results := memory.Search(query, limit)
			out := make([]map[string]any, 0, len(results))
			for _, r := range results {
				out = append(out, map[string]any{
					"id":      r.ID,
					"content": r.Content,
					"tags":    r.Tags,
					"weight":  r.Weight,
					"age":     formatAge(time.Since(r.TS)),
				})
			}
			body, _ := json.MarshalIndent(map[string]any{
				"matches": out,
				"count":   len(out),
			}, "", "  ")
			return ToolResponse{Text: string(body)}
		},
	})

	// ---- memory_list -----------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "memory_list",
		Description: "List currently-active memories. Returns ids, content, tags, weight, age. Use to get an overview before deciding what to consolidate.",
		Syntax:      `[[memory_list limit="50"]]`,
		Rules:       "Active memories only. limit defaults to 50. Output is in insertion order (oldest first); use memory_search for relevance-based lookup.",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			if memory == nil {
				return ToolResponse{Text: "error: no memory store"}
			}
			limit := 50
			if v := args["limit"]; v != "" {
				_, _ = fmt.Sscanf(v, "%d", &limit)
				if limit <= 0 || limit > 500 {
					limit = 50
				}
			}
			active := memory.Active()
			if len(active) > limit {
				active = active[len(active)-limit:]
			}
			out := make([]map[string]any, 0, len(active))
			for _, r := range active {
				out = append(out, map[string]any{
					"id":      r.ID,
					"content": r.Content,
					"tags":    r.Tags,
					"weight":  r.Weight,
					"age":     formatAge(time.Since(r.TS)),
				})
			}
			body, _ := json.MarshalIndent(map[string]any{
				"total":    memory.Count(),
				"returned": len(out),
				"active":   out,
			}, "", "  ")
			return ToolResponse{Text: string(body)}
		},
	})

	// ---- memory_remember -------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "memory_remember",
		Description: "Append a new memory. content is the statement to remember. tags are free-form labels (you choose what dimensions matter — common ones: identity, preference, decision, person, project, skill). weight (0.0–1.0) is your confidence + importance estimate.",
		Syntax:      `[[memory_remember content="Marco prefers terse replies for known topics, verbose for new ones" tags="preference,communication-style" weight="0.85"]]`,
		Rules:       "ALWAYS memory_search first — don't remember a duplicate. If a similar memory exists with stale wording, use memory_supersede instead. Weight high (0.8–0.95) for user-stated facts, medium (0.5–0.75) for inferred patterns, low (0.2–0.4) for uncertain hunches you'll let decay if not confirmed.",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			if memory == nil {
				return ToolResponse{Text: "error: no memory store"}
			}
			content := strings.TrimSpace(args["content"])
			if content == "" {
				return ToolResponse{Text: "error: content required"}
			}
			tags := splitCSV(args["tags"])
			weight := 0.7
			if v := args["weight"]; v != "" {
				_, _ = fmt.Sscanf(v, "%f", &weight)
			}
			id, err := memory.Remember(content, tags, weight)
			if err != nil {
				return ToolResponse{Text: fmt.Sprintf("error: %v", err)}
			}
			return ToolResponse{Text: fmt.Sprintf("remembered: id=%s w=%.2f tags=%v", id, weight, tags)}
		},
	})

	// ---- memory_supersede ------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "memory_supersede",
		Description: "Replace an existing memory with a new one. Old memory's id is tombstoned (audit trail preserved on disk); future recall returns only the new one. Use when wording was stale, a fact changed, or several memories should collapse into one.",
		Syntax:      `[[memory_supersede old_id="0193abc..." content="Marco prefers terse replies, even for new topics (corrected 2026-04-26)" tags="preference,communication-style" weight="0.9" reason="more precise after explicit correction"]]`,
		Rules:       "old_id from memory_search/memory_list. reason is REQUIRED — it goes into the audit log. Tags and weight default to the new memory's choice (typically same or higher than the old).",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			if memory == nil {
				return ToolResponse{Text: "error: no memory store"}
			}
			oldID := args["old_id"]
			content := strings.TrimSpace(args["content"])
			reason := strings.TrimSpace(args["reason"])
			if oldID == "" || content == "" || reason == "" {
				return ToolResponse{Text: "error: old_id, content, and reason all required"}
			}
			tags := splitCSV(args["tags"])
			weight := 0.7
			if v := args["weight"]; v != "" {
				_, _ = fmt.Sscanf(v, "%f", &weight)
			}
			newID, err := memory.Supersede(oldID, content, tags, weight, reason)
			if err != nil {
				return ToolResponse{Text: fmt.Sprintf("error: %v", err)}
			}
			return ToolResponse{Text: fmt.Sprintf("superseded %s with %s", oldID, newID)}
		},
	})

	// ---- memory_drop -----------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "memory_drop",
		Description: "Tombstone a memory. Use for: tasks that are done, ephemera that snuck in, fabrications you noticed, PII the user asked to forget.",
		Syntax:      `[[memory_drop id="0193abc..." reason="task completed 2026-04-25"]]`,
		Rules:       "id from memory_search/memory_list. reason is REQUIRED — silent drops aren't allowed; the operator deserves an audit trail. Tombstone records stay on disk; recall just skips them.",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			if memory == nil {
				return ToolResponse{Text: "error: no memory store"}
			}
			id := args["id"]
			reason := strings.TrimSpace(args["reason"])
			if id == "" || reason == "" {
				return ToolResponse{Text: "error: id and reason both required"}
			}
			if err := memory.Drop(id, reason); err != nil {
				return ToolResponse{Text: fmt.Sprintf("error: %v", err)}
			}
			return ToolResponse{Text: fmt.Sprintf("dropped %s (%s)", id, reason)}
		},
	})

	// ---- skill_write -----------------------------------------------------
	registry.Register(&ToolDef{
		Name:        "skill_write",
		Description: "Write or overwrite a skill file. Skills are loaded into all thread prompts automatically. Use for procedural knowledge — recurring workflows, debugging recipes, code-style cheat sheets.",
		Syntax:      `[[skill_write name="content-workflow" content="## Content Workflow\n1. List articles\n2. Find gaps\n3. Draft article"]]`,
		Rules:       "Name becomes the filename (skills/{name}.md). Content is markdown. Overwrites if exists. Different from memories: skills are HOW (procedure); memories are WHAT (facts/preferences/decisions).",
		Core:        true,
		SystemOnly:  true,
		Handler: func(args map[string]string) ToolResponse {
			name := args["name"]
			content := args["content"]
			if name == "" || content == "" {
				return ToolResponse{Text: "error: name and content required"}
			}
			name = strings.ReplaceAll(name, "/", "-")
			name = strings.ReplaceAll(name, "..", "")
			dir := "skills"
			os.MkdirAll(dir, 0755)
			path := filepath.Join(dir, name+".md")
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				return ToolResponse{Text: fmt.Sprintf("error: %v", err)}
			}
			return ToolResponse{Text: fmt.Sprintf("skill written: %s (%d bytes)", path, len(content))}
		},
	})
}

// splitCSV — small helper for tag parsing. "a,b,c" → []string{"a","b","c"}.
// Trims whitespace, drops empties.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
