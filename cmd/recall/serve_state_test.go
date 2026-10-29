package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

// postWith is post with extra headers.
func postWith(t *testing.T, url, key, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

func epochOf(t *testing.T, body string) uint64 {
	t.Helper()
	var v struct {
		Epoch uint64 `json:"epoch"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Epoch == 0 {
		t.Fatalf("no epoch in %s", body)
	}
	return v.Epoch
}

// Without leases there is no State API, and the HTTP surface says so.
func TestServeStateAPIUnsupportedWithoutLeases(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{"rcl_a": {Namespace: "acme"}})
	status, body := post(t, srv.URL+"/v1/agent/resume", "rcl_a", `{"agent_id":"run-1"}`)
	if status != http.StatusNotImplemented || !strings.Contains(body, "unsupported") {
		t.Fatalf("resume without leases: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/agent/nothing", "rcl_a", `{}`)
	if status != http.StatusNotFound || !strings.Contains(body, "resume") {
		t.Fatalf("unknown verb: %d %s", status, body)
	}
}

func newStateServer(t *testing.T, auth authenticator) *httptest.Server {
	t.Helper()
	backend, err := recall.OpenBackend("polign", recall.BackendOptions{URL: polignServer(t)})
	if err != nil {
		t.Fatal(err)
	}
	tenants := newTenantServers(serveConfig{backend: backend, registry: recall.DefaultRegistry(), embedder: recall.LexicalEmbedder{}, base: "memories", agents: true})
	t.Cleanup(tenants.releaseAll)
	srv := httptest.NewServer(newFrontDoor(auth, tenants, nil))
	t.Cleanup(srv.Close)
	return srv
}

// A plain key resumes over HTTP, gets the epoch, and must present it on
// every write. A later resume issues a new epoch and the old one is refused.
func TestServeStateAPIOverHTTP(t *testing.T) {
	srv := newStateServer(t, staticAuth{"rcl_a": {Namespace: "acme"}})
	agent := srv.URL + "/v1/agent/"

	status, body := post(t, agent+"resume", "rcl_a", `{"agent_id":"run-1","lease_ttl_seconds":30}`)
	if status != http.StatusOK || !strings.Contains(body, `"fresh":true`) {
		t.Fatalf("resume: %d %s", status, body)
	}
	epoch := epochOf(t, body)

	status, body = post(t, agent+"update_working_state", "rcl_a", `{"agent_id":"run-1","goal":"ship 0.24"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid_argument") || !strings.Contains(body, "If-Lease") {
		t.Fatalf("write without an epoch: %d %s", status, body)
	}
	status, body = post(t, agent+"update_working_state", "rcl_a", `{"agent_id":"run-1","goal":"ship 0.24","epoch":`+jsonUint(epoch+1)+`}`)
	if status != http.StatusConflict || !strings.Contains(body, "lease_lost") {
		t.Fatalf("write with a wrong epoch: %d %s", status, body)
	}
	status, body = post(t, agent+"update_working_state", "rcl_a", `{"agent_id":"run-1","goal":"ship 0.24","epoch":`+jsonUint(epoch)+`}`)
	if status != http.StatusOK || !strings.Contains(body, "ship 0.24") {
		t.Fatalf("write with the epoch in the body: %d %s", status, body)
	}
	status, body = postWith(t, agent+"record_turn", "rcl_a", `{"agent_id":"run-1","role":"user","content":"start"}`, map[string]string{"If-Lease": "run-1:" + jsonUint(epoch)})
	if status != http.StatusOK || !strings.Contains(body, `"seq":1`) {
		t.Fatalf("write with If-Lease: %d %s", status, body)
	}
	status, body = postWith(t, agent+"record_turn", "rcl_a", `{"agent_id":"run-1","role":"user","content":"x"}`, map[string]string{"If-Lease": "run-2:" + jsonUint(epoch)})
	if status != http.StatusBadRequest {
		t.Fatalf("If-Lease for another agent: %d %s", status, body)
	}
	// Reads need no epoch.
	status, body = post(t, agent+"recent_turns", "rcl_a", `{"agent_id":"run-1"}`)
	if status != http.StatusOK || !strings.Contains(body, `"start"`) {
		t.Fatalf("recent_turns: %d %s", status, body)
	}
	status, body = post(t, agent+"begin_action", "rcl_a", `{"agent_id":"run-1","tool":"send_email","class":"external","epoch":`+jsonUint(epoch)+`}`)
	if status != http.StatusOK || !strings.Contains(body, `"key"`) {
		t.Fatalf("begin_action: %d %s", status, body)
	}
	status, body = post(t, agent+"pending_actions", "rcl_a", `{"agent_id":"run-1"}`)
	if status != http.StatusOK || !strings.Contains(body, "send_email") {
		t.Fatalf("pending_actions: %d %s", status, body)
	}

	// A second stateless caller with the same key: resume is refused until
	// the agent is released; after release and resume the epoch moves on.
	status, body = post(t, agent+"resume", "rcl_a", `{"agent_id":"run-1"}`)
	if status != http.StatusConflict || !strings.Contains(body, "already_resumed") {
		t.Fatalf("second resume: %d %s", status, body)
	}
	status, body = post(t, agent+"release", "rcl_a", `{"agent_id":"run-1","epoch":`+jsonUint(epoch)+`}`)
	if status != http.StatusOK || !strings.Contains(body, `"released":true`) {
		t.Fatalf("release: %d %s", status, body)
	}
	status, body = post(t, agent+"update_working_state", "rcl_a", `{"agent_id":"run-1","progress":"late","epoch":`+jsonUint(epoch)+`}`)
	if status != http.StatusConflict || !strings.Contains(body, "not_resumed") {
		t.Fatalf("write after release: %d %s", status, body)
	}
	status, body = post(t, agent+"resume", "rcl_a", `{"agent_id":"run-1"}`)
	if status != http.StatusOK || !strings.Contains(body, "ship 0.24") || !strings.Contains(body, "send_email") {
		t.Fatalf("resume again: %d %s", status, body)
	}
	next := epochOf(t, body)
	if next == epoch {
		t.Fatalf("a new resume issued the same epoch %d", epoch)
	}
	status, body = post(t, agent+"update_working_state", "rcl_a", `{"agent_id":"run-1","progress":"stale","epoch":`+jsonUint(epoch)+`}`)
	if status != http.StatusConflict || !strings.Contains(body, "lease_lost") {
		t.Fatalf("write with the old epoch: %d %s", status, body)
	}
	status, body = post(t, agent+"update_working_state", "rcl_a", `{"agent_id":"run-1","progress":"current","epoch":`+jsonUint(next)+`}`)
	if status != http.StatusOK {
		t.Fatalf("write with the new epoch: %d %s", status, body)
	}
	// The same epoch is honoured on /mcp when given, and refused when stale.
	if out, isErr := rpcWith(t, srv, "rcl_a", "agent_milestone", `{"agent_id":"run-1","name":"half way","epoch":`+jsonUint(next)+`}`, nil); isErr {
		t.Fatalf("mcp with the epoch: %s", out)
	}
	if out, isErr := rpcWith(t, srv, "rcl_a", "agent_milestone", `{"agent_id":"run-1","name":"x"}`, map[string]string{"If-Lease": "run-1:" + jsonUint(epoch)}); !isErr || !strings.Contains(out, "lease_lost") {
		t.Fatalf("mcp with a stale If-Lease: %v %s", isErr, out)
	}
}

// A key bound to the agent, as Kubernetes mode mints, holds the lease by
// itself: its writes need no epoch.
func TestServeStateAPIBoundKeyNeedsNoEpoch(t *testing.T) {
	srv := newStateServer(t, staticAuth{"rcl_pod": {Namespace: "agents:run-1", Agent: "run-1", Session: "pod-a"}})
	agent := srv.URL + "/v1/agent/"
	if status, body := post(t, agent+"resume", "rcl_pod", `{"agent_id":"run-1"}`); status != http.StatusOK {
		t.Fatalf("resume: %d %s", status, body)
	}
	if status, body := post(t, agent+"update_working_state", "rcl_pod", `{"agent_id":"run-1","goal":"renewals"}`); status != http.StatusOK {
		t.Fatalf("write without an epoch: %d %s", status, body)
	}
	if status, body := post(t, agent+"resume", "rcl_pod", `{"agent_id":"run-2"}`); status != http.StatusForbidden || !strings.Contains(body, "namespace_mismatch") {
		t.Fatalf("another agent: %d %s", status, body)
	}
	if status, body := post(t, agent+"release", "rcl_pod", `{"agent_id":"run-1"}`); status != http.StatusOK {
		t.Fatalf("release: %d %s", status, body)
	}
}

func jsonUint(n uint64) string { return strconv.FormatUint(n, 10) }

// rpcWith is rpc with extra headers.
func rpcWith(t *testing.T, srv *httptest.Server, key, name, args string, headers map[string]string) (string, bool) {
	t.Helper()
	status, body := postWith(t, srv.URL+"/mcp", key, call(7, name, args), headers)
	if status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", name, status, body)
	}
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Result.Content) == 0 {
		t.Fatalf("%s: %v in %s", name, err, body)
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}
