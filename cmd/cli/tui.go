package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Messages
type respondMsg string
type askMsg string
type statusMsg statusUpdate
type connectedMsg struct{}
type tickMsg time.Time
type streamChunkMsg string  // incremental text from tool arg streaming
type toolReasonMsg string   // _reason from a tool call, for spinner display

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type tuiModel struct {
	th          theme
	mcp         *mcpServer
	client      *coreClient
	registry    *ChannelRegistry
	input       textinput.Model
	lines       []styledLine // scrollback buffer
	scrollOff   int
	width       int
	height      int
	connected   bool
	waiting     bool // waiting for core to cli_respond
	asking      bool // core asked a question via cli_ask
	streaming   bool // currently receiving streamed tool chunks
	streamLine  int  // index into lines for the active streaming line
	spinnerTick  int    // animation frame counter
	toolReason   string // latest _reason from tool call, shown in spinner
	statusLine   string
	statusLevel  string
	startTime    time.Time
	pollCounter  int // counts ticks for periodic polling

	// Live side panel data
	sideStatus    *sideData
	lastPollTick  int
	thoughts      map[string]*threadThought // latest thought per thread

	// CLI channel pipes (set by main after construction)
	cliRespond  chan string
	cliAskCh    chan string
	cliAskReply chan string
	cliStatusCh chan statusUpdate

	// Gateways
	telegramGW *TelegramGateway

	// Modal overlay — display or input
	modal        bool
	modalTitle   string
	modalLines   []string
	modalScroll  int
	modalInput   bool                        // modal has an input field
	modalPrompt  string                      // input label
	modalOnSubmit func(value string) tea.Cmd // callback when input submitted
}

// sideData holds live data for the side panel.
type sideData struct {
	Status    string
	Uptime    string
	Iteration int
	Rate      string
	Model     string
	Mode      string
	Threads   []sideThread
	Memories  int
	Directive string
}

type sideThread struct {
	ID   string
	Rate string
	Iter int
}

type sideDataMsg *sideData

type threadThought struct {
	Text string
	Time time.Time
}

type thoughtMsg struct {
	ThreadID string
	Text     string
}

type connectResultMsg struct {
	gateway string
	botName string
	err     error
	gw      *TelegramGateway
}

type modalMsg struct {
	title string
	text  string
}

type styledLine struct {
	text  string
	style string // "input", "output", "dim", "warn", "alert", "system"
	ts    time.Time
}

func newTUI(th theme, mcp *mcpServer, client *coreClient, registry *ChannelRegistry) tuiModel {
	ti := textinput.New()
	ti.Placeholder = ""
	ti.CharLimit = 1000
	ti.Prompt = ""
	ti.Focus()
	ti.TextStyle = lipgloss.NewStyle().Foreground(th.Primary)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(th.Accent)

	return tuiModel{
		th:        th,
		mcp:       mcp,
		registry:  registry,
		thoughts:  make(map[string]*threadThought),
		client:    client,
		input:     ti,
		startTime: time.Now(),
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		listenRespond(m.cliRespond),
		listenAsk(m.cliAskCh),
		listenStatus(m.cliStatusCh),
		tickEvery(),
		pollSideData(m.client),
	)
}

func listenRespond(ch chan string) tea.Cmd {
	return func() tea.Msg {
		return respondMsg(<-ch)
	}
}

func listenAsk(ch chan string) tea.Cmd {
	return func() tea.Msg {
		return askMsg(<-ch)
	}
}

func listenStatus(ch chan statusUpdate) tea.Cmd {
	return func() tea.Msg {
		return statusMsg(<-ch)
	}
}

func tickEvery() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func pollSideData(client *coreClient) tea.Cmd {
	return func() tea.Msg {
		sd := &sideData{}

		if st, err := client.status(); err == nil {
			uptime, _ := st["uptime_seconds"].(float64)
			iter, _ := st["iteration"].(float64)
			rate, _ := st["rate"].(string)
			model, _ := st["model"].(string)
			mode, _ := st["mode"].(string)
			paused, _ := st["paused"].(bool)
			memories, _ := st["memories"].(float64)
			sd.Uptime = formatDuration(time.Duration(uptime) * time.Second)
			sd.Iteration = int(iter)
			sd.Rate = rate
			sd.Model = model
			sd.Mode = mode
			sd.Memories = int(memories)
			if paused {
				sd.Status = "PAUSED"
			} else {
				sd.Status = "RUNNING"
			}
		}

		if threads, err := client.threads(); err == nil {
			for _, t := range threads {
				id, _ := t["id"].(string)
				rate, _ := t["rate"].(string)
				iter, _ := t["iteration"].(float64)
				sd.Threads = append(sd.Threads, sideThread{ID: id, Rate: rate, Iter: int(iter)})
			}
		}

		if cfg, err := client.getConfig(); err == nil {
			directive, _ := cfg["directive"].(string)
			sd.Directive = directive
		}

		return sideDataMsg(sd)
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Modal mode
		if m.modal {
			switch msg.String() {
			case "esc":
				m.closeModal()
				return m, nil
			case "enter":
				if m.modalInput && m.modalOnSubmit != nil {
					value := strings.TrimSpace(m.input.Value())
					m.input.SetValue("")
					m.input.Placeholder = ""
					cb := m.modalOnSubmit
					m.closeModal()
					if value != "" {
						return m, cb(value)
					}
					return m, nil
				}
			case "q":
				if !m.modalInput {
					m.closeModal()
					return m, nil
				}
			case "pgup", "up", "k":
				if !m.modalInput {
					if m.modalScroll > 0 {
						m.modalScroll--
					}
					return m, nil
				}
			case "pgdown", "down", "j":
				if !m.modalInput {
					m.modalScroll++
					return m, nil
				}
			}
			if m.modalInput {
				var inputCmd tea.Cmd
				m.input, inputCmd = m.input.Update(msg)
				return m, inputCmd
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			// no-op outside modal
		case "enter":
			return m.handleInput()
		case "pgup":
			if m.scrollOff < len(m.lines)-1 {
				m.scrollOff += 5
			}
			return m, nil
		case "pgdown":
			m.scrollOff -= 5
			if m.scrollOff < 0 {
				m.scrollOff = 0
			}
			return m, nil
		}

	case modalMsg:
		m.modal = true
		m.modalTitle = msg.title
		m.modalLines = strings.Split(msg.text, "\n")
		m.modalScroll = 0
		return m, nil

	case streamChunkMsg:
		text := string(msg)
		if !m.streaming {
			// Start a new streaming line — add spacing if there's content above
			m.streaming = true
			m.waiting = false
			m.toolReason = ""
			if len(m.lines) > 0 && m.lines[len(m.lines)-1].text != "" {
				m.addLine("", "dim")
			}
			m.addLine(text, "output")
			m.streamLine = len(m.lines) - 1
		} else {
			// Append to current streaming line, handling newlines
			lines := strings.Split(text, "\n")
			if m.streamLine >= 0 && m.streamLine < len(m.lines) {
				m.lines[m.streamLine].text += lines[0]
			}
			for _, extra := range lines[1:] {
				m.addLine(extra, "output")
				m.streamLine = len(m.lines) - 1
			}
		}
		m.scrollOff = 0

	case respondMsg:
		// Full response arrived via MCP tool call — if we were streaming, just finalize
		if m.streaming {
			m.streaming = false
			m.streamLine = -1
		} else {
			if len(m.lines) > 0 && m.lines[len(m.lines)-1].text != "" {
				m.addLine("", "dim")
			}
			m.addLine(string(msg), "output")
		}
		m.waiting = false
		m.toolReason = ""
		m.scrollOff = 0
		cmds = append(cmds, listenRespond(m.cliRespond))

	case askMsg:
		m.asking = true
		m.waiting = false
		m.addLine(string(msg), "output")
		m.scrollOff = 0
		cmds = append(cmds, listenAsk(m.cliAskCh))

	case statusMsg:
		m.statusLine = msg.Line
		m.statusLevel = msg.Level
		cmds = append(cmds, listenStatus(m.cliStatusCh))

	case connectedMsg:
		m.connected = true

	case connectResultMsg:
		if msg.err != nil {
			m.openModal("CONNECT ERROR", []string{"", "  " + msg.err.Error(), "", "  Press Esc to close."})
		} else {
			if msg.gw != nil {
				m.telegramGW = msg.gw
				m.registry.AddFactory(msg.gw.ChannelFactory())
			}
			m.openModal(strings.ToUpper(msg.gateway)+" CONNECTED", []string{"", fmt.Sprintf("  Bot @%s online.", msg.botName), "", "  Press Esc to close."})
			m.client.sendEvent(fmt.Sprintf("[%s] gateway connected. Bot @%s online. The agent can send messages to any telegram user who has started this bot using channels_respond(channel=\"%s:<chat_id>\"). When a user messages the bot, their chat_id appears in the event prefix.",
				msg.gateway, msg.botName, msg.gateway), "main")
		}
		return m, nil

	case toolReasonMsg:
		m.toolReason = string(msg)
		return m, nil

	case sideDataMsg:
		m.sideStatus = msg
		return m, nil

	case thoughtMsg:
		m.thoughts[msg.ThreadID] = &threadThought{Text: msg.Text, Time: time.Now()}
		return m, nil

	case tickMsg:
		m.spinnerTick++
		m.pollCounter++
		// Poll every ~3s (20 ticks * 150ms)
		if m.pollCounter-m.lastPollTick >= 20 {
			m.lastPollTick = m.pollCounter
			cmds = append(cmds, pollSideData(m.client))
		}
		cmds = append(cmds, tickEvery())

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = m.chatWidth() - 6
	}

	var inputCmd tea.Cmd
	m.input, inputCmd = m.input.Update(msg)
	cmds = append(cmds, inputCmd)

	return m, tea.Batch(cmds...)
}

func (m *tuiModel) chatWidth() int {
	return m.width * 2 / 3
}

func (m *tuiModel) sideWidth() int {
	return m.width - m.chatWidth() - 1 // -1 for the vertical border
}

func (m *tuiModel) handleInput() (tuiModel, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return *m, nil
	}
	m.input.SetValue("")

	// If answering a cli_ask question
	if m.asking {
		m.asking = false
		m.addLine("> "+text, "input")
		m.cliAskReply <- text
		return *m, nil
	}

	// Local commands
	if strings.HasPrefix(text, "/") {
		return m.handleCommand(text)
	}

	// Send to core — add spacing around user message
	if len(m.lines) > 0 {
		m.addLine("", "dim")
	}
	m.addLine("> "+text, "input")
	m.addLine("", "dim")
	m.waiting = true
	m.scrollOff = 0
	go m.client.sendEvent("[cli] "+text, "main")

	return *m, nil
}

func (m *tuiModel) handleCommand(text string) (tuiModel, tea.Cmd) {
	parts := strings.Fields(text)
	cmd := parts[0]

	switch cmd {
	case "/quit", "/exit":
		return *m, tea.Quit

	case "/clear":
		m.lines = nil
		m.scrollOff = 0

	case "/status":
		return *m, func() tea.Msg {
			st, err := m.client.status()
			if err != nil {
				return modalMsg{title: "STATUS", text: fmt.Sprintf("ERROR: %v", err)}
			}
			uptime, _ := st["uptime_seconds"].(float64)
			iter, _ := st["iteration"].(float64)
			rate, _ := st["rate"].(string)
			model, _ := st["model"].(string)
			threads, _ := st["threads"].(float64)
			memories, _ := st["memories"].(float64)
			mode, _ := st["mode"].(string)
			paused, _ := st["paused"].(bool)

			status := "RUNNING"
			if paused {
				status = "PAUSED"
			}

			return modalMsg{
				title: "STATUS",
				text: fmt.Sprintf(
					"  STATUS:     %s\n  UPTIME:     %s\n  ITERATION:  %.0f\n  RATE:       %s\n  MODEL:      %s\n  MODE:       %s\n  THREADS:    %.0f\n  MEMORY:     %.0f entries",
					status, formatDuration(time.Duration(uptime)*time.Second), iter, rate, model, mode, threads, memories,
				),
			}
		}

	case "/config":
		return *m, func() tea.Msg {
			cfg, err := m.client.getConfig()
			if err != nil {
				return modalMsg{title: "CONFIG", text: fmt.Sprintf("ERROR: %v", err)}
			}
			mode, _ := cfg["mode"].(string)
			directive, _ := cfg["directive"].(string)
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("  MODE:       %s\n\n", mode))
			sb.WriteString(fmt.Sprintf("  DIRECTIVE:\n  %s\n", directive))
			if prov, ok := cfg["provider"].(map[string]any); ok {
				name, _ := prov["name"].(string)
				sb.WriteString(fmt.Sprintf("\n  PROVIDER:   %s\n", name))
				if models, ok := prov["models"].(map[string]any); ok {
					for tier, id := range models {
						sb.WriteString(fmt.Sprintf("    %s: %v\n", tier, id))
					}
				}
			}
			if mcps, ok := cfg["mcp_servers"].([]any); ok && len(mcps) > 0 {
				sb.WriteString(fmt.Sprintf("\n  MCP SERVERS: %d\n", len(mcps)))
				for _, raw := range mcps {
					if entry, ok := raw.(map[string]any); ok {
						name, _ := entry["name"].(string)
						sb.WriteString(fmt.Sprintf("    - %s\n", name))
					}
				}
			}
			return modalMsg{title: "CONFIG", text: sb.String()}
		}

	case "/directive":
		rest := strings.TrimSpace(strings.TrimPrefix(text, "/directive"))
		if rest == "" {
			return *m, func() tea.Msg {
				cfg, err := m.client.getConfig()
				if err != nil {
					return modalMsg{title: "DIRECTIVE", text: fmt.Sprintf("ERROR: %v", err)}
				}
				directive, _ := cfg["directive"].(string)
				return modalMsg{title: "DIRECTIVE", text: "  " + directive}
			}
		}
		return *m, func() tea.Msg {
			if err := m.client.setDirective(rest); err != nil {
				return modalMsg{title: "DIRECTIVE", text: fmt.Sprintf("ERROR: %v", err)}
			}
			return modalMsg{title: "DIRECTIVE", text: "  Updated."}
		}

	case "/threads":
		return *m, func() tea.Msg {
			threads, err := m.client.threads()
			if err != nil {
				return modalMsg{title: "THREADS", text: fmt.Sprintf("ERROR: %v", err)}
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("  %d active\n\n", len(threads)))
			for _, t := range threads {
				id, _ := t["id"].(string)
				rate, _ := t["rate"].(string)
				model, _ := t["model"].(string)
				age, _ := t["age"].(string)
				iter, _ := t["iteration"].(float64)
				sb.WriteString(fmt.Sprintf("  %-12s  iter=%.0f  rate=%s  model=%s  age=%s\n", id, iter, rate, model, age))
			}
			return modalMsg{title: "THREADS", text: sb.String()}
		}

	case "/pause":
		return *m, func() tea.Msg {
			paused, err := m.client.pause()
			if err != nil {
				return modalMsg{title: "PAUSE", text: fmt.Sprintf("ERROR: %v", err)}
			}
			if paused {
				return modalMsg{title: "PAUSE", text: "  Core paused."}
			}
			return modalMsg{title: "PAUSE", text: "  Core resumed."}
		}

	case "/connect":
		rest := strings.TrimSpace(strings.TrimPrefix(text, "/connect"))
		if rest == "" {
			m.addLine("Usage: /connect <gateway>", "warn")
			m.addLine("Available: telegram", "dim")
			return *m, nil
		}
		switch rest {
		case "telegram":
			if m.telegramGW != nil {
				m.openModal("CONNECT", []string{"", "  Telegram already connected.", "", "  Press Esc to close."})
				return *m, nil
			}
			reg := m.registry
			cli := m.client
			m.openInputModal(
				"CONNECT TELEGRAM",
				[]string{
					"",
					"  Get a bot token from @BotFather on Telegram.",
					"  Paste it below and press Enter.",
					"",
				},
				"Bot token",
				func(token string) tea.Cmd {
					return func() tea.Msg {
						gw := NewTelegramGateway(token, reg, cli)
						botName, err := gw.Start()
						if err != nil {
							return connectResultMsg{gateway: "telegram", err: err}
						}
						return connectResultMsg{gateway: "telegram", botName: botName, gw: gw}
					}
				},
			)
		default:
			m.openModal("CONNECT", []string{"", fmt.Sprintf("  Unknown gateway: %s", rest), "", "  Available: telegram", "", "  Press Esc to close."})
		}

	case "/disconnect":
		rest := strings.TrimSpace(strings.TrimPrefix(text, "/disconnect"))
		if rest == "" {
			m.addLine("Usage: /disconnect <gateway>", "warn")
			return *m, nil
		}
		switch rest {
		case "telegram":
			if m.telegramGW == nil {
				m.addLine("Telegram not connected.", "warn")
			} else {
				m.telegramGW.Stop()
				m.telegramGW = nil
				m.addLine("Telegram disconnected.", "output")
				m.client.sendEvent("[telegram] gateway disconnected", "main")
			}
		default:
			m.addLine(fmt.Sprintf("Unknown gateway: %s", rest), "warn")
		}

	case "/channels":
		channels := m.registry.List()
		m.modal = true
		m.modalTitle = fmt.Sprintf("CHANNELS (%d)", len(channels))
		m.modalLines = []string{""}
		for _, ch := range channels {
			m.modalLines = append(m.modalLines, fmt.Sprintf("  %-20s  connected", ch.ID()))
		}
		m.modalLines = append(m.modalLines, "", "  Press Esc to close.")
		m.modalScroll = 0

	case "/help":
		m.modal = true
		m.modalTitle = "HELP"
		m.modalLines = []string{
			"  /status              show core status",
			"  /config              show full config",
			"  /directive [text]    show or set directive",
			"  /threads             list active threads",
			"  /pause               toggle pause/resume",
			"  /connect <gateway>   connect a gateway (telegram)",
			"  /disconnect <gw>    disconnect a gateway",
			"  /channels            list connected channels",
			"  /clear               clear screen",
			"  /help                show this help",
			"  /quit                disconnect and exit",
			"",
			"  Everything else is sent to the agent.",
			"",
			"  Press Esc to close.",
		}
		m.modalScroll = 0

	default:
		m.addLine(fmt.Sprintf("UNKNOWN COMMAND: %s", cmd), "warn")
		m.addLine("Type /help for available commands.", "dim")
	}

	return *m, nil
}

func (m *tuiModel) closeModal() {
	m.modal = false
	m.modalLines = nil
	m.modalScroll = 0
	m.modalInput = false
	m.modalPrompt = ""
	m.modalOnSubmit = nil
	m.input.SetValue("")
	m.input.Placeholder = ""
}

func (m *tuiModel) openModal(title string, lines []string) {
	m.modal = true
	m.modalTitle = title
	m.modalLines = lines
	m.modalScroll = 0
	m.modalInput = false
}

func (m *tuiModel) openInputModal(title string, lines []string, prompt string, onSubmit func(string) tea.Cmd) {
	m.modal = true
	m.modalTitle = title
	m.modalLines = lines
	m.modalScroll = 0
	m.modalInput = true
	m.modalPrompt = prompt
	m.modalOnSubmit = onSubmit
	m.input.SetValue("")
	m.input.Placeholder = ""
}

func (m *tuiModel) connectGateway(target, token string) (tuiModel, tea.Cmd) {
	switch target {
	case "telegram":
		if m.telegramGW != nil {
			m.addLine("Telegram already connected.", "warn")
			return *m, nil
		}
		gw := NewTelegramGateway(token, m.registry, m.client)
		m.telegramGW = gw
		m.addLine("Connecting to Telegram...", "dim")
		return *m, func() tea.Msg {
			botName, err := gw.Start()
			if err != nil {
				m.telegramGW = nil
				return connectResultMsg{gateway: "telegram", err: err}
			}
			return connectResultMsg{gateway: "telegram", botName: botName}
		}
	default:
		m.addLine(fmt.Sprintf("Unknown gateway: %s", target), "warn")
		return *m, nil
	}
}

func (m *tuiModel) addLine(text string, style string) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		sl := styledLine{text: line, style: style}
		if i == 0 && style != "dim" && line != "" {
			sl.ts = time.Now()
		}
		m.lines = append(m.lines, sl)
	}
}

// truncateToWidth truncates a string to fit within maxWidth display cells.
func truncateToWidth(s string, maxWidth int) string {
	if lipgloss.Width(s) <= maxWidth {
		return s
	}
	// Truncate rune by rune
	var result []rune
	for _, r := range s {
		next := append(result, r)
		if lipgloss.Width(string(next)) > maxWidth {
			break
		}
		result = next
	}
	return string(result)
}

// wrapText wraps a string to fit within maxWidth display cells, breaking on spaces.
func wrapText(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{s}
	}
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if lipgloss.Width(line) <= maxWidth {
			result = append(result, line)
			continue
		}
		words := strings.Fields(line)
		if len(words) == 0 {
			result = append(result, "")
			continue
		}
		cur := words[0]
		// Truncate single words that are wider than maxWidth
		if lipgloss.Width(cur) > maxWidth {
			cur = truncateToWidth(cur, maxWidth)
		}
		for _, w := range words[1:] {
			test := cur + " " + w
			if lipgloss.Width(test) > maxWidth {
				result = append(result, cur)
				cur = w
				if lipgloss.Width(cur) > maxWidth {
					cur = truncateToWidth(cur, maxWidth)
				}
			} else {
				cur = test
			}
		}
		result = append(result, cur)
	}
	return result
}

func (m tuiModel) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	if m.modal {
		return m.renderModal()
	}

	primary := lipgloss.NewStyle().Foreground(m.th.Primary)
	dim := lipgloss.NewStyle().Foreground(m.th.Dim)
	accent := lipgloss.NewStyle().Foreground(m.th.Accent)
	faded := lipgloss.NewStyle().Foreground(m.th.Faded)
	warn := lipgloss.NewStyle().Foreground(m.th.Warn)
	alert := lipgloss.NewStyle().Foreground(m.th.Alert)

	chatW := m.chatWidth()
	sideW := m.sideWidth()
	innerChat := chatW - 4 // 2 padding each side

	// Layout: header(1) + separator(1) + content + separator(1) + input(1) = 4 chrome lines
	contentHeight := m.height - 4
	if contentHeight < 1 {
		contentHeight = 1
	}

	// ── Header ──
	connStatus := dim.Render("◉ DISCONNECTED")
	if m.connected {
		connStatus = accent.Render("◉ CORE LIVE")
	}
	title := primary.Bold(true).Render("APTEVA")
	headerPad := m.width - lipgloss.Width(title) - lipgloss.Width(connStatus)
	if headerPad < 1 {
		headerPad = 1
	}
	header := title + strings.Repeat(" ", headerPad) + connStatus
	sep := dim.Render(strings.Repeat("─", m.width))

	// ── Chat panel (left) ──
	// Wrap and collect visible lines
	var wrappedLines []styledLine
	for _, sl := range m.lines {
		wrapped := wrapText(sl.text, innerChat)
		for i, w := range wrapped {
			wl := styledLine{text: w, style: sl.style}
			if i == len(wrapped)-1 {
				wl.ts = sl.ts // timestamp on last wrapped line (most likely to have room)
			}
			wrappedLines = append(wrappedLines, wl)
		}
	}

	// Visible region
	chatContentH := contentHeight - 2 // -2 for status line + input separator inside chat
	if chatContentH < 1 {
		chatContentH = 1
	}

	start := len(wrappedLines) - chatContentH - m.scrollOff
	if start < 0 {
		start = 0
	}
	end := start + chatContentH
	if end > len(wrappedLines) {
		end = len(wrappedLines)
	}

	var chatLines []string
	for i := start; i < end; i++ {
		line := wrappedLines[i]
		var styled string
		switch line.style {
		case "input":
			styled = faded.Render(line.text)
		case "output":
			styled = renderMarkdown(line.text, primary, accent)
		case "dim", "system":
			styled = dim.Render(line.text)
		case "warn":
			styled = warn.Render(line.text)
		case "alert":
			styled = alert.Render(line.text)
		default:
			styled = line.text
		}
		// Right-aligned timestamp based on plain text width
		if !line.ts.IsZero() {
			tsStr := line.ts.Format("3:04")
			plainW := lipgloss.Width(line.text)
			pad := innerChat - plainW - len(tsStr)
			if pad >= 2 {
				styled = styled + strings.Repeat(" ", pad) + dim.Render(tsStr)
			}
		}
		chatLines = append(chatLines, styled)
	}

	// Spinner while waiting
	if m.waiting && len(chatLines) < chatContentH {
		frame := spinnerFrames[m.spinnerTick%len(spinnerFrames)]
		chatLines = append(chatLines, dim.Render(frame))
	}

	// Pad to fill
	for len(chatLines) < chatContentH {
		chatLines = append(chatLines, "")
	}

	// Status line inside chat
	statusText := m.statusLine
	if statusText == "" {
		statusText = "READY"
	}
	var statusStyled string
	switch m.statusLevel {
	case "warn":
		statusStyled = warn.Render(statusText)
	case "alert":
		statusStyled = alert.Render(statusText)
	default:
		statusStyled = dim.Render(statusText)
	}

	// Input line
	prompt := primary.Bold(true).Render("> ")
	inputLine := prompt + m.input.View()

	// Build chat column
	chatLines = append(chatLines, dim.Render(strings.Repeat("─", innerChat)))
	chatLines = append(chatLines, inputLine)

	chatPanel := lipgloss.NewStyle().
		Width(chatW).
		Padding(0, 2).
		Render(strings.Join(chatLines, "\n"))

	// ── Side panel (right) ──
	sideLines := m.renderSidePanel(sideW-2, contentHeight, dim, primary, accent, warn)
	sidePanel := lipgloss.NewStyle().
		Width(sideW).
		Padding(0, 1).
		Render(strings.Join(sideLines, "\n"))

	// ── Vertical border ──
	var borderLines []string
	for i := 0; i < contentHeight; i++ {
		borderLines = append(borderLines, dim.Render("│"))
	}
	border := strings.Join(borderLines, "\n")

	// ── Compose ──
	body := lipgloss.JoinHorizontal(lipgloss.Top, chatPanel, border, sidePanel)

	// Bottom status bar across full width
	bottomBar := dim.Render(strings.Repeat("─", chatW)) +
		dim.Render("┴") +
		dim.Render(strings.Repeat("─", sideW))
	_ = statusStyled

	return header + "\n" + sep + "\n" + body + "\n" + bottomBar + " " + statusStyled
}

func (m tuiModel) renderModal() string {
	primary := lipgloss.NewStyle().Foreground(m.th.Primary)
	dim := lipgloss.NewStyle().Foreground(m.th.Dim)
	accent := lipgloss.NewStyle().Foreground(m.th.Accent)

	// Modal box: centered, 60% width, up to 80% height
	boxW := m.width * 60 / 100
	if boxW < 40 {
		boxW = m.width - 4
	}
	innerW := boxW - 4 // 2 border + 2 padding
	boxH := m.height * 80 / 100
	if boxH < 5 {
		boxH = m.height - 2
	}
	// Reserve space for input row if needed
	extraRows := 0
	if m.modalInput {
		extraRows = 2 // separator + input line
	}
	innerH := boxH - 4 - extraRows // top border + title + bottom border + footer

	// Title bar
	titleText := " " + m.modalTitle + " "
	titleLen := lipgloss.Width(titleText)
	topBorder := "┌" + accent.Render(titleText) + dim.Render(strings.Repeat("─", max(0, innerW+2-titleLen))) + "┐"

	// Scrollable content
	totalLines := len(m.modalLines)
	scroll := m.modalScroll
	if scroll > totalLines-innerH {
		scroll = totalLines - innerH
	}
	if scroll < 0 {
		scroll = 0
	}
	endLine := scroll + innerH
	if endLine > totalLines {
		endLine = totalLines
	}

	var contentLines []string
	for i := scroll; i < endLine; i++ {
		line := m.modalLines[i]
		// Truncate if too wide (emoji-safe)
		if lipgloss.Width(line) > innerW {
			line = truncateToWidth(line, innerW-1) + "…"
		}
		pad := innerW - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		contentLines = append(contentLines, dim.Render("│ ")+primary.Render(line)+strings.Repeat(" ", pad)+dim.Render(" │"))
	}
	// Pad remaining height
	for len(contentLines) < innerH {
		contentLines = append(contentLines, dim.Render("│ ")+strings.Repeat(" ", innerW)+dim.Render(" │"))
	}

	// Input row inside modal
	if m.modalInput {
		contentLines = append(contentLines, dim.Render("│ ")+dim.Render(strings.Repeat("─", innerW))+dim.Render(" │"))
		label := accent.Render("  " + m.modalPrompt + ": ")
		inputView := m.input.View()
		inputLine := label + inputView
		inputW := lipgloss.Width(m.modalPrompt+": ") + 2 + lipgloss.Width(m.input.Value()) + 1
		inputPad := innerW - inputW
		if inputPad < 0 {
			inputPad = 0
		}
		_ = inputPad
		contentLines = append(contentLines, dim.Render("│ ")+inputLine+dim.Render(" │"))
	}

	// Footer
	footer := dim.Render("  esc to close")
	if m.modalInput {
		footer = dim.Render("  enter to submit · esc to cancel")
	} else if totalLines > innerH {
		footer += dim.Render(fmt.Sprintf("  ↑↓ to scroll (%d/%d)", scroll+1, totalLines))
	}
	bottomBorder := dim.Render("└"+strings.Repeat("─", innerW+2)+"┘")

	// Compose
	var lines []string
	// Vertical centering
	topPad := (m.height - boxH) / 2
	leftPad := (m.width - boxW - 2) / 2
	if leftPad < 0 {
		leftPad = 0
	}
	indent := strings.Repeat(" ", leftPad)

	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, indent+topBorder)
	for _, cl := range contentLines {
		lines = append(lines, indent+cl)
	}
	lines = append(lines, indent+bottomBorder)
	lines = append(lines, indent+footer)
	// Fill rest
	for len(lines) < m.height {
		lines = append(lines, "")
	}

	return strings.Join(lines[:m.height], "\n")
}

func (m tuiModel) renderSidePanel(w, h int, dim, primary, accent, warn lipgloss.Style) []string {
	var lines []string
	sd := m.sideStatus

	// Title
	lines = append(lines, accent.Bold(true).Render("SYSTEM"))
	lines = append(lines, dim.Render(strings.Repeat("─", w)))
	lines = append(lines, "")

	if sd != nil {
		// Status + Mode
		if sd.Status == "PAUSED" {
			lines = append(lines, dim.Render("STATUS  ")+warn.Render(sd.Status))
		} else {
			lines = append(lines, dim.Render("STATUS  ")+accent.Render(sd.Status))
		}
		lines = append(lines, dim.Render("MODE    ")+primary.Render(sd.Mode))
		lines = append(lines, dim.Render("UPTIME  ")+primary.Render(sd.Uptime))
		lines = append(lines, dim.Render("ITER    ")+primary.Render(fmt.Sprintf("%d", sd.Iteration)))
		lines = append(lines, dim.Render("RATE    ")+primary.Render(sd.Rate))
		lines = append(lines, dim.Render("MODEL   ")+primary.Render(sd.Model))
		lines = append(lines, dim.Render("MEMORY  ")+primary.Render(fmt.Sprintf("%d", sd.Memories)))
		lines = append(lines, "")

		// Directive (truncated)
		lines = append(lines, dim.Render(strings.Repeat("─", w)))
		lines = append(lines, accent.Bold(true).Render("DIRECTIVE"))
		lines = append(lines, "")
		directive := sd.Directive
		// Wrap directive to panel width
		for _, dl := range wrapText(directive, w) {
			lines = append(lines, dim.Render(dl))
		}
		lines = append(lines, "")

		// Threads + latest thoughts
		lines = append(lines, dim.Render(strings.Repeat("─", w)))
		lines = append(lines, accent.Bold(true).Render(fmt.Sprintf("THREADS (%d)", len(sd.Threads))))
		lines = append(lines, "")
		for _, t := range sd.Threads {
			label := primary.Render(fmt.Sprintf("%-10s", t.ID))
			info := dim.Render(fmt.Sprintf("#%d %s", t.Iter, t.Rate))
			lines = append(lines, label+info)

			// Show latest thought with decay
			if thought, ok := m.thoughts[t.ID]; ok {
				age := time.Since(thought.Time)
				if age < 2*time.Minute {
					text := thought.Text
					// Clean up: single line, truncate
					text = strings.ReplaceAll(text, "\n", " ")
					text = strings.Join(strings.Fields(text), " ")
					maxLen := w - 2
					if maxLen > 80 {
						maxLen = 80
					}
					if len(text) > maxLen {
						text = text[:maxLen-1] + "…"
					}
					// Decay: bright → dim based on age
					var thoughtStyle lipgloss.Style
					if age < 10*time.Second {
						thoughtStyle = lipgloss.NewStyle().Foreground(m.th.Dim).Italic(true)
					} else if age < 30*time.Second {
						thoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
					} else {
						thoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("237")).Italic(true)
					}
					lines = append(lines, thoughtStyle.Render("  "+text))
				}
			}
		}
	} else {
		lines = append(lines, dim.Render("loading..."))
	}

	// Pad to fill height
	for len(lines) < h {
		lines = append(lines, "")
	}

	return lines[:h]
}

// renderMarkdown applies basic markdown styling to a line of text.
// Handles **bold**, `code`, and # headers.
func renderMarkdown(text string, base, accent lipgloss.Style) string {
	trimmed := strings.TrimSpace(text)

	// Headers: # ## ###
	if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
		// Strip # prefix
		header := strings.TrimLeft(trimmed, "# ")
		return accent.Bold(true).Render(strings.ToUpper(header))
	}

	// Inline: process **bold** and `code`
	var result strings.Builder
	bold := base.Bold(true)
	code := lipgloss.NewStyle().Foreground(accent.GetForeground())
	i := 0
	for i < len(text) {
		// **bold**
		if i+1 < len(text) && text[i] == '*' && text[i+1] == '*' {
			end := strings.Index(text[i+2:], "**")
			if end >= 0 {
				result.WriteString(bold.Render(text[i+2 : i+2+end]))
				i = i + 2 + end + 2
				continue
			}
		}
		// `code`
		if text[i] == '`' {
			end := strings.IndexByte(text[i+1:], '`')
			if end >= 0 {
				result.WriteString(code.Render(text[i+1 : i+1+end]))
				i = i + 1 + end + 1
				continue
			}
		}
		result.WriteByte(text[i])
		i++
	}

	return base.Render(result.String())
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
