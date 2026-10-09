// Command recall serves typed, correctable agent memory over MCP, kept in a
// polign_db instance it reaches through the HTTP API, or in Qdrant with
// -backend qdrant.
//
//	recall mcp [-write] [-agent] [...]   serve the memory tools over stdio
//	recall setup [-local | -url URL]     configure Recall, optionally install the Claude plugin
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
	base := flag.String("url", "", "database base URL (default $POLIGN_URL, or $QDRANT_URL with -backend qdrant; for polign without either, recall mcp uses what recall setup configured)")
	key := flag.String("key", "", "API key sent on every request (default $POLIGN_API_KEY, or $QDRANT_API_KEY with -backend qdrant)")
	backend := flag.String("backend", envOr("RECALL_BACKEND", "polign"), "storage backend: polign or qdrant (default $RECALL_BACKEND, then polign)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: recall [global flags] <command> [options]\n\nCommands: mcp, setup, doctor, skill\nAgent guide: recall skill (offline)\n\nGlobal flags:")
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
	if *backend != "polign" && *backend != "qdrant" {
		log.Fatalf("unknown -backend %q (want polign or qdrant)", *backend)
	}
	url := *base
	if url == "" && *backend == "qdrant" {
		url = os.Getenv("QDRANT_URL")
	} else if url == "" {
		url = os.Getenv("POLIGN_URL")
	}
	if *key == "" && *backend == "qdrant" {
		*key = os.Getenv("QDRANT_API_KEY")
	} else if *key == "" {
		*key = os.Getenv("POLIGN_API_KEY")
	}
	c := &api{base: strings.TrimRight(url, "/"), key: *key, backend: *backend}
	cmd, args := flag.Arg(0), flag.Args()[1:]
	var err error
	switch cmd {
	case "mcp":
		err = cmdMCP(c, url != "", args)
	case "setup", "doctor":
		err = cmdRecall(append([]string{cmd}, args...), os.Stdout)
	case "skill":
		err = cmdSkill(args, os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q (want mcp, setup, doctor, skill)", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// api is a minimal JSON client for the server's HTTP transport. The key, when
// set, rides on every request. backend names the database behind base: empty
// or "polign" for polign_db, "qdrant" for Qdrant, which only the memory
// runtime reaches, through its own client.
type api struct {
	base    string
	key     string
	backend string
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
