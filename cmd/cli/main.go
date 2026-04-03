package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const mcpName = "channels"

func main() {
	addr := flag.String("addr", "localhost:3210", "core API address")
	themeName := flag.String("theme", "orange", "color theme: orange, amber, white")
	noSpawn := flag.Bool("no-spawn", false, "don't auto-start core, connect to existing instance")
	coreBin := flag.String("core-bin", "", "path to apteva-core binary (default: auto-detect)")
	flag.Parse()

	th, ok := themes[*themeName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown theme: %s (available: orange, amber, white)\n", *themeName)
		os.Exit(1)
	}

	client := newCoreClient(*addr)

	// Try connecting to existing core, or spawn one
	var coreProc *exec.Cmd
	if err := client.health(); err != nil {
		if *noSpawn {
			fmt.Fprintf(os.Stderr, "cannot reach core at %s: %v\n", *addr, err)
			os.Exit(1)
		}

		// Auto-start core
		bin := findCoreBinary(*coreBin)
		if bin == "" {
			fmt.Fprintf(os.Stderr, "cannot find apteva-core binary (use --core-bin to specify)\n")
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "starting core: %s\n", bin)
		coreProc = exec.Command(bin, "--headless")
		coreProc.Dir = filepath.Dir(bin)
		coreProc.Env = append(os.Environ(), "NO_TUI=1")
		coreProc.Stdout = nil
		coreProc.Stderr = nil

		if err := coreProc.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start core: %v\n", err)
			os.Exit(1)
		}

		// Wait for core to become healthy
		if err := waitForHealth(client, 15*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "core did not start in time: %v\n", err)
			coreProc.Process.Kill()
			os.Exit(1)
		}
	}

	// Channel registry — CLI is always the first channel
	registry := NewChannelRegistry()

	// CLI channel: TUI communication pipes
	cliRespond := make(chan string, 64)
	cliAskCh := make(chan string, 1)
	cliAskReply := make(chan string, 1)
	cliStatusCh := make(chan statusUpdate, 16)
	cliChannel := NewCLIChannel(cliRespond, cliAskCh, cliAskReply, cliStatusCh)
	registry.Register(cliChannel)

	// Start unified MCP server
	mcp, err := newMCPServer(registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp server: %v\n", err)
		os.Exit(1)
	}
	go mcp.serve()

	// Register MCP with core via PUT /config
	if err := client.connectMCP(mcpName, mcp.url()); err != nil {
		fmt.Fprintf(os.Stderr, "register mcp: %v\n", err)
		mcp.close()
		os.Exit(1)
	}

	// Notify core
	client.sendEvent("[cli] root user connected. RULES: 1) Reply to ALL [cli] messages using channels_respond(channel=\"cli\"). 2) When the user asks you to do something, IMMEDIATELY acknowledge what you will do BEFORE doing it, then follow up with the result. 3) Never leave a message unanswered. Greet them now.", "main")

	// SSE done signal
	sseDone := make(chan struct{})

	// Cleanup on exit
	cleanup := func() {
		close(sseDone)
		registry.CloseAll()
		client.sendEvent("[cli] root user disconnected from terminal", "main")
		client.disconnectMCP(mcpName)
		mcp.close()
		if coreProc != nil {
			coreProc.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- coreProc.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				coreProc.Process.Kill()
			}
		}
	}

	// Handle signals
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cleanup()
		os.Exit(0)
	}()

	// Run TUI — pass CLI channels directly for the TUI to listen on
	m := newTUI(th, mcp, client, registry)
	// Inject CLI channel pipes into the TUI model
	m.cliRespond = cliRespond
	m.cliAskCh = cliAskCh
	m.cliAskReply = cliAskReply
	m.cliStatusCh = cliStatusCh

	p := tea.NewProgram(m, tea.WithAltScreen())

	// Start SSE listener for tool chunk streaming
	go streamToolChunks(client, p, sseDone)

	go func() {
		p.Send(connectedMsg{})
	}()

	if _, err := p.Run(); err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}

	cleanup()
}

// findCoreBinary locates the apteva-core binary.
func findCoreBinary(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}

	// Check relative to this binary
	self, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(self)
		candidates := []string{
			filepath.Join(dir, "apteva-core"),
			filepath.Join(dir, "..", "..", "apteva-core"),
			filepath.Join(dir, "core"),
			filepath.Join(dir, "..", "..", "core"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				abs, _ := filepath.Abs(c)
				return abs
			}
		}
	}

	// Check current directory
	for _, name := range []string{"apteva-core", "core"} {
		if _, err := os.Stat(name); err == nil {
			abs, _ := filepath.Abs(name)
			return abs
		}
	}

	// Check PATH
	if p, err := exec.LookPath("apteva-core"); err == nil {
		return p
	}

	return ""
}

// waitForHealth polls the core health endpoint until it responds or times out.
func waitForHealth(client *coreClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := client.health(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}
