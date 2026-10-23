// Command recall serves typed, correctable agent memory over MCP, kept in a
// local polign_db it runs itself when nothing names a database, or in any
// backend registered in backends.go: -backend says which, -url where, -key
// the credential (see database.go).
//
//	recall mcp [-write] [-agent] [...]   serve the memory tools over stdio
//	recall setup [-backend B -url URL]   save where memory lives, optionally install the Claude plugin
//	recall explain <question>            say why each matching belief is held
//	recall history <question>            show how a belief changed, statement by statement
//	recall ask [-as-of T] [question]     say what is believed, now or at an earlier instant
//	recall diff -from T [-to T]          show what changed between two instants
//	recall reembed -embed-model <spec>   copy memory into a collection searched by meaning
//	recall doctor                        check a configured Recall end to end
//	recall skill                         print the agent guide
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// version is stamped via -ldflags "-X main.version=..." (goreleaser).
var version = "dev"

func main() {
	log.SetFlags(0)
	global := addDatabaseFlags(flag.CommandLine)
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: recall [flags] <command> [options]\n\nCommands: mcp, connect, serve, key, explain, history, ask, diff, reembed, setup, doctor, skill\nAgent guide: recall skill (offline)\n\nThe database flags below go before the command or after it:")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	cmd, args := flag.Arg(0), flag.Args()[1:]
	var err error
	switch cmd {
	case "mcp":
		err = cmdMCP(global, args)
	case "serve":
		err = cmdServe(global, args)
	case "connect":
		err = cmdConnect(args, os.Stdin, os.Stdout)
	case "key":
		err = cmdKey(global, args, os.Stdout)
	case "explain":
		err = cmdExplain(global, args, os.Stdout)
	case "history":
		err = cmdHistory(global, args, os.Stdout)
	case "ask":
		err = cmdAsk(global, args, os.Stdout)
	case "diff":
		err = cmdDiff(global, args, os.Stdout)
	case "reembed":
		err = cmdReembed(global, args, os.Stdout)
	case "setup", "doctor":
		err = cmdRecall(global, append([]string{cmd}, args...), os.Stdout)
	case "skill":
		err = cmdSkill(args, os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q (want mcp, connect, serve, key, explain, history, ask, diff, reembed, setup, doctor, skill)", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// setupBackend is the backend the memory runtime opens when an api names
// none, which only tests do.
const setupBackend = "polign"

// api is a minimal JSON client for the server's HTTP transport. The key, when
// set, rides on every request. backend is the registered backend name the
// memory runtime opens base with, and params that backend's options; only a
// polign_db is reached through api itself.
type api struct {
	base    string
	key     string
	backend string
	params  map[string]string
}

// apiError is a non-2xx response. It carries the status code so a caller can
// react to one in particular, and the server's own message, which is more
// use than the status on its own.
type apiError struct {
	Code    int
	Status  string
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return e.Status + ": " + e.Message
}

func (c *api) do(method, path string, body, out any) error {
	return c.doCtx(context.Background(), method, path, body, out)
}

// doCtx is do with a caller-supplied context, for the long-lived commands
// (mcp) that have one to propagate.
func (c *api) doCtx(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &apiError{Code: resp.StatusCode, Status: resp.Status, Message: e.Error}
		}
		return &apiError{Code: resp.StatusCode, Status: resp.Status, Message: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// --- data commands -------------------------------------------------------

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
