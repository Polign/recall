package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lines splits bridge output into parsed JSON-RPC responses by id.
func lines(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	got := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		id, _ := json.Marshal(m["id"])
		got[string(id)] = m
	}
	return got
}

func runBridge(t *testing.T, b *bridge, input string) string {
	t.Helper()
	var out bytes.Buffer
	var mu sync.Mutex
	b.out = writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return out.Write(p) })
	if err := b.run(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String()
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestConnectBridgesStdioToTheServerWithAnExchangedToken(t *testing.T) {
	srv, _, keys := newKubeTest(t, "")
	var renews atomic.Int32
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/keys/renew" {
			renews.Add(1)
		}
		inner.ServeHTTP(w, r)
	})
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("pod-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cred := newExchangedKey(srv.URL, tokenFile, srv.Client())
	// The key store runs on a fixed test clock; the client must share it, or
	// on any later day every key looks past half its life and is replaced.
	cred.now = func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }
	b := newBridge(srv.URL, cred, srv.Client(), nil)

	// Messages in one burst are forwarded concurrently, as a client that
	// pipelines them would expect, so the write and the read that depends
	// on it go in separate rounds, as a real client sequences them.
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		call(2, "remember", `{"subject":"user","predicate":"prefers_editor","value":"vim"}`) + "\n"
	got := lines(t, runBridge(t, b, input))
	if len(got) != 2 {
		t.Fatalf("want 2 answers (the notification gets none), got %d: %v", len(got), got)
	}
	if !strings.Contains(fmtJSON(got["1"]), "protocolVersion") {
		t.Errorf("initialize: %v", got["1"])
	}
	got = lines(t, runBridge(t, b, call(3, "recall", `{"subject":"user","predicate":"prefers_editor"}`)+"\n"))
	if !strings.Contains(fmtJSON(got["3"]), `vim`) {
		t.Errorf("recall through the bridge: %v", got["3"])
	}
	// The key is the pod's own.
	recs, err := keys.list(context.Background())
	if err != nil || len(recs) != 1 || recs[0].Namespace != "agents:sandbox-1" {
		t.Fatalf("keys: %v %v", recs, err)
	}

	// A revoked key is refreshed by a new exchange on the next call.
	if err := keys.revoke(context.Background(), recs[0].ID); err != nil {
		t.Fatal(err)
	}
	got = lines(t, runBridge(t, b, call(4, "recall", `{"subject":"user","predicate":"prefers_editor"}`)+"\n"))
	if !strings.Contains(fmtJSON(got["4"]), "vim") {
		t.Fatalf("after revocation the bridge should re-exchange: %v", got["4"])
	}
	if recs, _ = keys.list(context.Background()); len(recs) != 2 {
		t.Fatalf("want a second key after refresh, have %d", len(recs))
	}

	// Past half the lifetime the bridge tries to renew; a workload key
	// refuses, so it exchanges the token again and gets a fresh key.
	cred.now = func() time.Time { return cred.expiresAt.Add(-time.Minute) }
	if _, err := cred.bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if renews.Load() != 1 {
		t.Fatalf("renewal attempts: %d, want 1", renews.Load())
	}
	if recs, _ = keys.list(context.Background()); len(recs) != 3 {
		t.Fatalf("want a third key from the re-exchange, have %d", len(recs))
	}
}

func TestConnectReportsARefusedKeyAsAnError(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_ok": {Namespace: "acme"}})
	b := newBridge(srv.URL, staticKey("rcl_bogus"), srv.Client(), nil)
	got := lines(t, runBridge(t, b, call(1, "recall", `{}`)+"\n"))
	errObj, _ := got["1"]["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32001 || !strings.Contains(errObj["message"].(string), "401") {
		t.Fatalf("want a -32001 error, got %v", got["1"])
	}
	b = newBridge(srv.URL, staticKey("rcl_ok"), srv.Client(), nil)
	got = lines(t, runBridge(t, b, call(1, "list_predicates", `{}`)+"\n"))
	if _, ok := got["1"]["result"]; !ok {
		t.Fatalf("static key: %v", got["1"])
	}
}

func TestConnectNeedsAServerAndACredential(t *testing.T) {
	if err := cmdConnect([]string{}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "RECALL_URL") {
		t.Fatalf("no server: %v", err)
	}
	if err := cmdConnect([]string{"-server", "http://127.0.0.1:1"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "RECALL_API_KEY") {
		t.Fatalf("no credential: %v", err)
	}
}

func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
