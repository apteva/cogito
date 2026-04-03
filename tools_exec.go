package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func execTool(args map[string]string) string {
	command := args["command"]
	if command == "" {
		return "error: missing command argument"
	}

	// Parse timeout (default 30s, max 300s)
	timeout := 30 * time.Second
	if t, ok := args["timeout"]; ok {
		if secs, err := strconv.Atoi(t); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
			if timeout > 300*time.Second {
				timeout = 300 * time.Second
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)

	// Optional working directory
	if dir, ok := args["dir"]; ok && dir != "" {
		cmd.Dir = dir
	}

	output, err := cmd.CombinedOutput()
	result := string(output)

	if ctx.Err() == context.DeadlineExceeded {
		result = strings.TrimSpace(result)
		if result != "" {
			result += "\n"
		}
		result += fmt.Sprintf("[timeout after %ds]", int(timeout.Seconds()))
	} else if err != nil {
		result = strings.TrimSpace(result)
		if result != "" {
			result += "\n"
		}
		result += fmt.Sprintf("[exit: %v]", err)
	}

	// Truncate
	if utf8.RuneCountInString(result) > maxToolResultLen {
		runes := []rune(result)
		result = string(runes[:maxToolResultLen]) + "\n[truncated]"
	}

	if result == "" {
		return "(no output)"
	}

	return result
}
