package main

// Anonymous usage statistics (internal/telemetry, TELEMETRY.md) and the
// `recall telemetry` subcommand that shows them and turns them off.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Polign/recall/internal/telemetry"
)

// usage is the running process's reporter; nil when telemetry is off, which
// every method on it accepts.
var usage *telemetry.Reporter

// startUsage begins reporting for a long-running command. The first run on
// an install says so on stderr, which an MCP client logs.
func startUsage(mode, backend, embedSpec, extractSpec string, text bool) {
	usage = telemetry.Start(usageConfig(mode, backend, embedSpec, extractSpec, text), func(notice string) {
		fmt.Fprintln(os.Stderr, notice)
	})
}

func usageConfig(mode, backend, embedSpec, extractSpec string, text bool) telemetry.Config {
	return telemetry.Config{Version: version, Home: recallHome(), Mode: mode, Backend: backend, Embedder: embedSpec, Extractor: extractSpec, Text: text}
}

// countTool records one successful tool call. A remember that names what it
// replaced counts as a correction; the output is only searched for that key.
func countTool(name, out string) {
	if usage == nil {
		return
	}
	usage.Tool(name, name == "remember" && (strings.Contains(out, `"replaces":[`) || strings.Contains(out, `"replaced":[`)))
}

// cmdTelemetry shows whether usage statistics are sent and exactly what a
// heartbeat holds, and turns them off or on for this Recall home.
func cmdTelemetry(args []string, out io.Writer) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: recall telemetry [status|on|off]")
	}
	home := recallHome()
	switch action {
	case "off", "on":
		if err := telemetry.SetOff(home, action == "off"); err != nil {
			return fmt.Errorf("telemetry: %w", err)
		}
	case "status":
	default:
		return fmt.Errorf("usage: recall telemetry [status|on|off]")
	}
	if reason := telemetry.Disabled(version, home); reason != "" {
		fmt.Fprintf(out, "Usage statistics: off (%s)\n", reason)
	} else {
		fmt.Fprintf(out, "Usage statistics: on\nSent to: %s\n", telemetry.Endpoint)
	}
	if action != "status" {
		return nil
	}
	body, err := json.MarshalIndent(telemetry.Preview(usageConfig("mcp", "local", "", "", true)), "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "A heartbeat holds exactly these fields (shown for recall mcp on the local database, with zero counts; install_id is empty until the first one is sent):\n%s\nTurn off: recall telemetry off, RECALL_TELEMETRY=0 or DO_NOT_TRACK=1. Every field is described in TELEMETRY.md.\n", body)
	return nil
}
