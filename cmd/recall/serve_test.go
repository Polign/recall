package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Polign/recall"
)

// multiDB is a backend that keeps one fake database per collection, so a
// test can tell whether two namespaces share records.
type multiDB struct {
	mu sync.Mutex
	by map[string]*fakeMemoryDB
}

func newMultiDB() *multiDB { return &multiDB{by: map[string]*fakeMemoryDB{}} }

func (m *multiDB) db(c string) *fakeMemoryDB {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.by[c]; ok {
		return d
	}
	d := newFakeMemoryDB()
	m.by[c] = d
	return d
}

func (m *multiDB) Put(_ context.Context, c, id string, v []float32, md map[string]any) error {
	return m.db(c).Put(c, id, v, md)
}

func (m *multiDB) List(_ context.Context, c string, f map[string]any, n int) ([]recall.StoredVector, int, error) {
	return m.db(c).List(c, f, n)
}

func (m *multiDB) Search(_ context.Context, c string, v []float32, k int, f map[string]any) ([]recall.Hit, error) {
	return m.db(c).Search(c, v, k, f)
}

// staticAuth maps credentials straight to principals.
type staticAuth map[string]principal

func (a staticAuth) Authenticate(_ context.Context, credential string) (principal, error) {
	p, ok := a[credential]
	if !ok {
		return principal{}, errUnauthenticated
	}
	return p, nil
}

func newTestFrontDoor(t *testing.T, auth authenticator) *httptest.Server {
	t.Helper()
	tenants := newTenantServers(serveConfig{backend: newMultiDB(), registry: recall.DefaultRegistry(), embedder: recall.LexicalEmbedder{}, base: "memories", agents: true})
	srv := httptest.NewServer(newFrontDoor(auth, tenants))
	t.Cleanup(srv.Close)
	return srv
}

// post sends a JSON body with a bearer key and returns the status and body.
func post(t *testing.T, url, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// rpc sends one JSON-RPC call to /mcp and returns the tool's text and
// whether the tool reported an error.
func rpc(t *testing.T, srv *httptest.Server, key, name, args string) (string, bool) {
	t.Helper()
	status, body := post(t, srv.URL+"/mcp", key, call(7, name, args))
	if status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", name, status, body)
	}
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("%s: %v in %s", name, err, body)
	}
	if resp.Error != nil {
		t.Fatalf("%s: protocol error %v", name, resp.Error)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("%s: no content in %s", name, body)
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}

func TestServeRefusesWithoutAKnownKey(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_a": {Namespace: "acme"}})
	for _, key := range []string{"", "rcl_wrong"} {
		status, body := post(t, srv.URL+"/v1/recall", key, `{}`)
		if status != http.StatusUnauthorized || !strings.Contains(body, "unauthenticated") {
			t.Errorf("key %q: status %d body %s", key, status, body)
		}
	}
	// A lowercase scheme is still bearer authentication.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/predicates", strings.NewReader("{}"))
	req.Header.Set("Authorization", "bearer rcl_a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("lowercase bearer: status %d", resp.StatusCode)
	}
	// Health needs no key.
	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: status %d", resp.StatusCode)
	}
}

func TestServeMCPOverHTTP(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_a": {Namespace: "acme"}})
	status, body := post(t, srv.URL+"/mcp", "rcl_a", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if status != http.StatusOK || !strings.Contains(body, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/mcp", "rcl_a", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted || body != "" {
		t.Fatalf("notification: %d %q", status, body)
	}
	status, body = post(t, srv.URL+"/mcp", "rcl_a", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if status != http.StatusOK || !strings.Contains(body, `"name":"remember"`) || !strings.Contains(body, `"name":"memory_history"`) {
		t.Fatalf("tools/list: %d %s", status, body)
	}
	if strings.Contains(body, `"name":"agent_resume"`) {
		t.Fatal("tools/list: resume tools served on a backend without leases")
	}
	if out, isErr := rpc(t, srv, "rcl_a", "remember", `{"subject":"user","predicate":"prefers_editor","value":"vim"}`); isErr {
		t.Fatalf("remember: %s", out)
	}
	out, isErr := rpc(t, srv, "rcl_a", "recall", `{"subject":"user","predicate":"prefers_editor"}`)
	if isErr || !strings.Contains(out, `"vim"`) {
		t.Fatalf("recall: %v %s", isErr, out)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer rcl_a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp: status %d, want 405", resp.StatusCode)
	}
	status, body = post(t, srv.URL+"/mcp", "rcl_a", `not json`)
	if status != http.StatusBadRequest || !strings.Contains(body, fmt.Sprint(codeParse)) {
		t.Errorf("malformed: %d %s", status, body)
	}
}

func TestServeRESTVerbs(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_a": {Namespace: "acme"}})
	status, body := post(t, srv.URL+"/v1/remember", "rcl_a", `{"subject":"user","predicate":"prefers_editor","value":"vim"}`)
	if status != http.StatusOK {
		t.Fatalf("remember: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/remember", "rcl_a", `{"subject":"user","predicate":"prefers_editor","value":"helix"}`)
	if status != http.StatusOK || !strings.Contains(body, `"vim"`) {
		t.Fatalf("second remember should report what it replaced: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/recall", "rcl_a", `{"subject":"user","predicate":"prefers_editor"}`)
	if status != http.StatusOK || !strings.Contains(body, `"helix"`) || !strings.Contains(body, `"replaced"`) {
		t.Fatalf("recall: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/history", "rcl_a", `{"subject":"user","predicate":"prefers_editor"}`)
	if status != http.StatusOK || strings.Count(body, `"prefers_editor"`) < 2 {
		t.Fatalf("history: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/predicates", "rcl_a", ``)
	if status != http.StatusOK || !strings.Contains(body, `prefers_editor`) {
		t.Fatalf("predicates with an empty body: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/forget", "rcl_a", `{"subject":"user"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid_argument") && !strings.Contains(body, "tool_error") {
		t.Fatalf("forget without a predicate: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/nothing", "rcl_a", `{}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown verb: %d %s", status, body)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/recall", nil)
	req.Header.Set("Authorization", "Bearer rcl_a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET verb: status %d, want 405", resp.StatusCode)
	}
}

func TestServeNamespacesAreIsolated(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_a": {Namespace: "acme"}, "rcl_b": {Namespace: "acme:agent-7"}})
	if status, body := post(t, srv.URL+"/v1/remember", "rcl_a", `{"subject":"user","predicate":"prefers_editor","value":"vim"}`); status != http.StatusOK {
		t.Fatalf("remember: %d %s", status, body)
	}
	status, body := post(t, srv.URL+"/v1/recall", "rcl_b", `{"subject":"user","predicate":"prefers_editor"}`)
	if status != http.StatusOK || strings.Contains(body, "vim") {
		t.Fatalf("the other namespace sees the record: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/recall", "rcl_a", `{"subject":"user","predicate":"prefers_editor"}`)
	if status != http.StatusOK || !strings.Contains(body, "vim") {
		t.Fatalf("own namespace: %d %s", status, body)
	}
}

func TestServeRefusesAnotherIdentity(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_a": {Namespace: "acme", Agent: "sandbox-1", Session: "sandbox-1"}})
	// Repeating the credential's own identity is fine, and the fields are
	// stripped before the tool sees them.
	status, body := post(t, srv.URL+"/v1/remember", "rcl_a", `{"namespace":"acme","session":"sandbox-1","subject":"user","predicate":"prefers_editor","value":"vim"}`)
	if status != http.StatusOK {
		t.Fatalf("own identity: %d %s", status, body)
	}
	for _, body := range []string{`{"namespace":"globex","subject":"user","predicate":"prefers_editor"}`, `{"session":"sandbox-2","subject":"user","predicate":"prefers_editor"}`} {
		status, out := post(t, srv.URL+"/v1/recall", "rcl_a", body)
		if status != http.StatusForbidden || !strings.Contains(out, "namespace_mismatch") {
			t.Errorf("%s: %d %s", body, status, out)
		}
	}
	// Over MCP the refusal is a tool result, so the model reads it.
	out, isErr := rpc(t, srv, "rcl_a", "recall", `{"namespace":"globex","subject":"user","predicate":"prefers_editor"}`)
	if !isErr || !strings.Contains(out, "namespace_mismatch") {
		t.Errorf("mcp mismatch: %v %s", isErr, out)
	}
	// A credential fixed to one agent cannot act as another.
	out, isErr = rpc(t, srv, "rcl_a", "agent_resume", `{"agent_id":"sandbox-2"}`)
	if !isErr || !strings.Contains(out, "namespace_mismatch") {
		t.Errorf("agent mismatch: %v %s", isErr, out)
	}
	if status, _ := post(t, srv.URL+"/v1/recall", "rcl_a", `{"namespace":7}`); status != http.StatusBadRequest {
		t.Errorf("non-string namespace: %d", status)
	}
}

func TestNamespaceCollection(t *testing.T) {
	a, b := namespaceCollection("m", "a.b"), namespaceCollection("m", "a_b")
	if a == b {
		t.Fatalf("a.b and a_b share %s", a)
	}
	for _, ns := range []string{"acme", "Acme:agent-7", "x.y.z"} {
		c := namespaceCollection("memories", ns)
		if !strings.HasPrefix(c, "memories__") {
			t.Errorf("%s: %s", ns, c)
		}
		for i := 0; i < len(c); i++ {
			ch := c[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
				t.Errorf("%s: byte %q in %s", ns, ch, c)
			}
		}
	}
	if namespaceCollection("m", "acme") != namespaceCollection("m", "acme") {
		t.Error("not deterministic")
	}
}

func TestLoadFileKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys")
	if err := os.WriteFile(path, []byte("# operators\nrcl_1_aaa acme\n\nrcl_2_bbb acme:agent-7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := loadFileKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := keys.Authenticate(context.Background(), "rcl_2_bbb")
	if err != nil || p.Namespace != "acme:agent-7" {
		t.Fatalf("got %+v, %v", p, err)
	}
	if _, err := keys.Authenticate(context.Background(), "rcl_3_ccc"); err == nil {
		t.Fatal("unknown key accepted")
	}
	for _, bad := range []string{"rcl_1_aaa\n", "rcl_1_aaa :acme\n", "\n# nothing\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFileKeys(path); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
