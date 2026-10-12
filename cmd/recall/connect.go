package main

// The `recall connect` subcommand: an MCP stdio client's way to a remote
// `recall serve`. An agent harness that speaks MCP over stdio starts this
// instead of `recall mcp`; it forwards every message to the server's /mcp
// endpoint and writes the answers back. Nothing is configured inside the
// workload beyond where the server is and one credential.
//
// The credential is a Recall key, or a projected Kubernetes service account
// token that is exchanged for one at start and renewed before it expires. A
// 401 part-way through (a revoked key, a server that forgot it) triggers one
// fresh exchange and a retry, so a restarted server is not a dead session.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultRecallTokenFile is where the sandbox manifests project the token.
const defaultRecallTokenFile = "/var/run/secrets/recall/token" //nolint:gosec // a path, not a credential

// credential supplies the bearer for each request and refreshes it.
type credential interface {
	// bearer returns the current key, obtaining or renewing one if needed.
	bearer(ctx context.Context) (string, error)
	// refresh discards the current key after the server refused it and
	// obtains a new one; it reports false when nothing can be done.
	refresh(ctx context.Context) bool
}

// staticKey is a key handed to the process; it cannot be refreshed.
type staticKey string

func (k staticKey) bearer(context.Context) (string, error) { return string(k), nil }
func (staticKey) refresh(context.Context) bool             { return false }

// exchangedKey is a key obtained from a projected token at
// /v1/auth/kubernetes and renewed at /v1/keys/renew.
type exchangedKey struct {
	server    string
	tokenFile string
	client    *http.Client
	now       func() time.Time

	mu        sync.Mutex
	key       string
	expiresAt time.Time
	ttl       time.Duration
}

func newExchangedKey(server, tokenFile string, client *http.Client) *exchangedKey {
	return &exchangedKey{server: server, tokenFile: tokenFile, client: client, now: time.Now}
}

func (e *exchangedKey) bearer(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.key == "":
		return e.exchangeLocked(ctx)
	case e.ttl > 0 && e.now().After(e.expiresAt.Add(-e.ttl/2)):
		// Past half the lifetime: renew, and fall back to a fresh exchange
		// when the server will not (the key may have expired or been
		// revoked while this process was idle).
		if err := e.renewLocked(ctx); err != nil {
			return e.exchangeLocked(ctx)
		}
	}
	return e.key, nil
}

func (e *exchangedKey) refresh(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.key = ""
	_, err := e.exchangeLocked(ctx)
	return err == nil
}

func (e *exchangedKey) exchangeLocked(ctx context.Context) (string, error) {
	token, err := readFileTrimmed(e.tokenFile)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"token": string(token)})
	var view struct {
		Key       string `json:"key"`
		ExpiresAt string `json:"expires_at"`
		TTL       string `json:"ttl"`
	}
	if err := postJSON(ctx, e.client, e.server+"/v1/auth/kubernetes", "", body, &view); err != nil {
		return "", fmt.Errorf("exchange: %w", err)
	}
	if view.Key == "" {
		return "", errors.New("exchange: the server returned no key")
	}
	e.key = view.Key
	e.expiresAt, e.ttl = time.Time{}, 0
	if view.TTL != "" {
		if d, err := time.ParseDuration(view.TTL); err == nil {
			e.ttl = d
		}
	}
	if t, err := time.Parse(time.RFC3339, view.ExpiresAt); err == nil {
		e.expiresAt = t
	}
	return e.key, nil
}

func (e *exchangedKey) renewLocked(ctx context.Context) error {
	var view struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := postJSON(ctx, e.client, e.server+"/v1/keys/renew", e.key, nil, &view); err != nil {
		return err
	}
	if t, err := time.Parse(time.RFC3339, view.ExpiresAt); err == nil {
		e.expiresAt = t
	}
	return nil
}

// httpStatusError is a non-2xx answer from the server.
type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("server answered %d: %s", e.status, strings.TrimSpace(e.body))
}

// postJSON posts a body with an optional bearer and decodes a JSON answer.
func postJSON(ctx context.Context, client *http.Client, url, bearer string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{status: resp.StatusCode, body: string(raw)}
	}
	if out == nil || resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// bridge forwards JSON-RPC between a stdio pair and a server's /mcp.
type bridge struct {
	server string
	cred   credential
	client *http.Client

	mu  sync.Mutex
	out io.Writer
}

// forward sends one message and returns the server's answer, retrying once
// with a fresh credential after a 401.
func (b *bridge) forward(ctx context.Context, msg json.RawMessage) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		key, err := b.cred.bearer(ctx)
		if err != nil {
			return nil, err
		}
		var out json.RawMessage
		err = postJSON(ctx, b.client, b.server+"/mcp", key, msg, &out)
		var se *httpStatusError
		if errors.As(err, &se) && se.status == http.StatusUnauthorized && attempt == 0 && b.cred.refresh(ctx) {
			continue
		}
		return out, err
	}
}

// run reads requests from r until it closes, answering on the encoder.
func (b *bridge) run(ctx context.Context, r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			b.reply(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: err.Error()}})
			return fmt.Errorf("read: %w", err)
		}
		var head struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(raw, &head)
		wg.Add(1)
		go func(raw json.RawMessage, id json.RawMessage) {
			defer wg.Done()
			out, err := b.forward(ctx, raw)
			if len(id) == 0 {
				// A notification wants no answer; a failure to deliver one is
				// worth a line on stderr and nothing more.
				if err != nil {
					log.Printf("connect: notification: %v", err)
				}
				return
			}
			if err != nil {
				code := -32000
				var se *httpStatusError
				if errors.As(err, &se) && se.status == http.StatusUnauthorized {
					code = -32001
				}
				b.reply(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: err.Error()}})
				return
			}
			b.write(append(bytes.TrimSpace(out), '\n'))
		}(raw, head.ID)
	}
}

func (b *bridge) reply(resp rpcResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		log.Printf("connect: encode: %v", err)
		return
	}
	b.write(append(raw, '\n'))
}

// write puts one line on the output; lines never interleave.
func (b *bridge) write(line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.out.Write(line); err != nil {
		log.Printf("connect: write: %v", err)
	}
}

// cmdConnect runs the bridge on stdin and stdout.
func cmdConnect(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	server := fs.String("server", os.Getenv("RECALL_URL"), "the recall serve URL, such as http://recall.recall.svc:8080 (default $RECALL_URL)")
	key := fs.String("key", os.Getenv("RECALL_API_KEY"), "a Recall key (default $RECALL_API_KEY)")
	tokenFile := fs.String("token-file", os.Getenv("RECALL_TOKEN_FILE"), "a projected Kubernetes service account token to exchange for a key (default $RECALL_TOKEN_FILE, then "+defaultRecallTokenFile+" when it exists)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *server == "" {
		return errors.New("connect: name the server with -server or RECALL_URL")
	}
	*server = strings.TrimRight(*server, "/")
	client := &http.Client{Timeout: 120 * time.Second}
	var cred credential
	switch {
	case *key != "":
		cred = staticKey(*key)
	case *tokenFile != "":
		cred = newExchangedKey(*server, *tokenFile, client)
	default:
		if _, err := os.Stat(defaultRecallTokenFile); err != nil {
			return errors.New("connect: give a key with -key or RECALL_API_KEY, or a token with -token-file or RECALL_TOKEN_FILE")
		}
		cred = newExchangedKey(*server, defaultRecallTokenFile, client)
	}
	b := newBridge(*server, cred, client, stdout)
	// Obtain the credential before the first message, so a misconfiguration
	// fails at start rather than inside the first tool call.
	if _, err := cred.bearer(context.Background()); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return b.run(context.Background(), stdin)
}

func newBridge(server string, cred credential, client *http.Client, stdout io.Writer) *bridge {
	return &bridge{server: server, cred: cred, client: client, out: stdout}
}
