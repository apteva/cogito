package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type coreClient struct {
	base   string
	client *http.Client
}

func newCoreClient(addr string) *coreClient {
	return &coreClient{
		base:   "http://" + addr,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// health checks if core is reachable.
func (c *coreClient) health() error {
	resp, err := c.client.Get(c.base + "/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// status fetches core status.
func (c *coreClient) status() (map[string]any, error) {
	resp, err := c.client.Get(c.base + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

// threads fetches active threads.
func (c *coreClient) threads() ([]map[string]any, error) {
	resp, err := c.client.Get(c.base + "/threads")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

// sendEvent posts a message to the core event bus.
func (c *coreClient) sendEvent(message, threadID string) error {
	body, _ := json.Marshal(map[string]string{
		"message":   message,
		"thread_id": threadID,
	})
	resp, err := c.client.Post(c.base+"/event", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// pause toggles pause.
func (c *coreClient) pause() (bool, error) {
	resp, err := c.client.Post(c.base+"/pause", "application/json", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Paused bool `json:"paused"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Paused, nil
}

// approve sends tool approval.
func (c *coreClient) approve(approved bool) error {
	body, _ := json.Marshal(map[string]bool{"approved": approved})
	resp, err := c.client.Post(c.base+"/approve", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// getConfig fetches current config.
func (c *coreClient) getConfig() (map[string]any, error) {
	resp, err := c.client.Get(c.base + "/config")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

type mcpServerEntry struct {
	Name       string `json:"name"`
	URL        string `json:"url,omitempty"`
	Command    string `json:"command,omitempty"`
	Transport  string `json:"transport,omitempty"`
	MainAccess bool   `json:"main_access,omitempty"`
}

// connectMCP adds the CLI's MCP server to core config.
func (c *coreClient) connectMCP(name, url string) error {
	// Get current config to read existing mcp_servers
	cfg, err := c.getConfig()
	if err != nil {
		return fmt.Errorf("get config: %w", err)
	}

	// Build desired list: existing servers + our new one
	var servers []mcpServerEntry
	if raw, ok := cfg["mcp_servers"]; ok && raw != nil {
		data, _ := json.Marshal(raw)
		json.Unmarshal(data, &servers)
	}

	// Remove any stale entry with same name
	var clean []mcpServerEntry
	for _, s := range servers {
		if s.Name != name {
			clean = append(clean, s)
		}
	}
	clean = append(clean, mcpServerEntry{
		Name:       name,
		URL:        url,
		Transport:  "http",
		MainAccess: true,
	})

	body, _ := json.Marshal(map[string]any{"mcp_servers": clean})
	resp, err := c.client.Do(c.putRequest("/config", body))
	if err != nil {
		return fmt.Errorf("put config: %w", err)
	}
	resp.Body.Close()
	return nil
}

// disconnectMCP removes the CLI's MCP server from core config.
func (c *coreClient) disconnectMCP(name string) error {
	cfg, err := c.getConfig()
	if err != nil {
		return err
	}

	var servers []mcpServerEntry
	if raw, ok := cfg["mcp_servers"]; ok && raw != nil {
		data, _ := json.Marshal(raw)
		json.Unmarshal(data, &servers)
	}

	var clean []mcpServerEntry
	for _, s := range servers {
		if s.Name != name {
			clean = append(clean, s)
		}
	}

	body, _ := json.Marshal(map[string]any{"mcp_servers": clean})
	resp, err := c.client.Do(c.putRequest("/config", body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// setDirective updates the core directive via PUT /config.
func (c *coreClient) setDirective(directive string) error {
	body, _ := json.Marshal(map[string]string{"directive": directive})
	resp, err := c.client.Do(c.putRequest("/config", body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *coreClient) putRequest(path string, body []byte) *http.Request {
	req, _ := http.NewRequest("PUT", c.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// streamEvents opens SSE connection and sends events to a channel.
func (c *coreClient) streamEvents(ch chan<- map[string]any, done <-chan struct{}) {
	client := &http.Client{Timeout: 0} // no timeout for SSE
	resp, err := client.Get(c.base + "/events")
	if err != nil {
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	var line []byte
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			line = append(line, buf[:n]...)
			// Process complete SSE lines
			for {
				idx := bytes.IndexByte(line, '\n')
				if idx < 0 {
					break
				}
				raw := bytes.TrimSpace(line[:idx])
				line = line[idx+1:]
				if bytes.HasPrefix(raw, []byte("data: ")) {
					var ev map[string]any
					if json.Unmarshal(raw[6:], &ev) == nil {
						ch <- ev
					}
				}
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}

// streamToolChunks listens to SSE events and extracts streaming text from
// cli_respond tool argument chunks, sending them to the TUI as streamChunkMsg.
func streamToolChunks(client *coreClient, p *tea.Program, done <-chan struct{}) {
	evCh := make(chan map[string]any, 256)
	go client.streamEvents(evCh, done)

	ext := &textExtractor{}

	for {
		select {
		case <-done:
			return
		case ev, ok := <-evCh:
			if !ok {
				return
			}
			typ, _ := ev["type"].(string)
			data, _ := ev["data"].(map[string]any)

			switch typ {
			case "llm.done":
				if data != nil {
					threadID, _ := ev["thread_id"].(string)
					if threadID == "" {
						threadID = "main"
					}
					msg, _ := data["message"].(string)
					if msg != "" {
						// Truncate to first 200 chars
						if len(msg) > 200 {
							msg = msg[:200] + "..."
						}
						p.Send(thoughtMsg{ThreadID: threadID, Text: msg})
					}
				}
			case "llm.tool_chunk":
				if data == nil {
					continue
				}
				tool, _ := data["tool"].(string)
				chunk, _ := data["chunk"].(string)
				if tool != "channels_respond" || chunk == "" {
					continue
				}

				// Extract new text from the incremental JSON
				newText := ext.feed(chunk)
				if newText != "" {
					p.Send(streamChunkMsg(newText))
				}
			}
		}
	}
}

// textExtractor incrementally extracts the "text" value from streaming JSON
// fragments like: {"te  →  xt": "H  →  ello  →  "}
// Once it finds "text":" it emits everything after that as text,
// handling escaped characters. Handles escape sequences split across chunks.
type textExtractor struct {
	buf        strings.Builder
	inText     bool // we've found "text":" and are extracting
	emitted    int  // how many bytes of the text value we've already emitted
	pendingEsc bool // previous chunk ended with a backslash
}

func (e *textExtractor) feed(chunk string) string {
	e.buf.WriteString(chunk)
	s := e.buf.String()

	if !e.inText {
		// Look for "text":"  or  "text": " in accumulated buffer
		idx := strings.Index(s, `"text":"`)
		start := 0
		if idx >= 0 {
			start = idx + 8 // len(`"text":"`)
		} else {
			idx = strings.Index(s, `"text": "`)
			if idx >= 0 {
				start = idx + 9 // len(`"text": "`)
			}
		}
		if idx < 0 {
			return ""
		}
		e.inText = true
		e.emitted = start
	}

	// Extract text from emitted position, handling escape sequences
	var result strings.Builder
	i := e.emitted
	for i < len(s) {
		ch := s[i]
		if e.pendingEsc {
			e.pendingEsc = false
			switch ch {
			case '"':
				result.WriteByte('"')
			case '\\':
				result.WriteByte('\\')
			case 'n':
				result.WriteByte('\n')
			case 'r':
				result.WriteByte('\r')
			case 't':
				result.WriteByte('\t')
			case '/':
				result.WriteByte('/')
			default:
				result.WriteByte('\\')
				result.WriteByte(ch)
			}
			i++
			continue
		}
		if ch == '\\' {
			if i+1 >= len(s) {
				// Backslash at end of buffer — wait for next chunk
				e.pendingEsc = true
				i++
				continue
			}
			next := s[i+1]
			switch next {
			case '"':
				result.WriteByte('"')
			case '\\':
				result.WriteByte('\\')
			case 'n':
				result.WriteByte('\n')
			case 'r':
				result.WriteByte('\r')
			case 't':
				result.WriteByte('\t')
			case '/':
				result.WriteByte('/')
			default:
				result.WriteByte('\\')
				result.WriteByte(next)
			}
			i += 2
			continue
		}
		if ch == '"' {
			// End of string value — reset for next tool call
			e.buf.Reset()
			e.inText = false
			e.emitted = 0
			e.pendingEsc = false
			break
		}
		result.WriteByte(ch)
		i++
	}
	e.emitted = i

	return result.String()
}
