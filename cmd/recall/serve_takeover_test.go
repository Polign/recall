package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

// A pod that resumed an agent and died without releasing it leaves the agent
// held by recall serve, which keeps its lease alive. The claim's next pod
// carries a key for the same agent and a new session; its resume takes the
// agent over, and the old pod's key can no longer write through it.
func TestServeNewSessionTakesOverAnAbandonedAgent(t *testing.T) {
	backend, err := recall.OpenBackend("polign", recall.BackendOptions{URL: polignServer(t)})
	if err != nil {
		t.Fatal(err)
	}
	tenants := newTenantServers(serveConfig{backend: backend, registry: recall.DefaultRegistry(), embedder: recall.LexicalEmbedder{}, base: "memories", agents: true})
	t.Cleanup(tenants.releaseAll)
	auth := staticAuth{
		"rcl_pod1": {Namespace: "agents:run-1", Agent: "run-1", Session: "pool-aaaaa"},
		"rcl_pod2": {Namespace: "agents:run-1", Agent: "run-1", Session: "pool-bbbbb"},
	}
	srv := httptest.NewServer(newFrontDoor(auth, tenants, nil))
	t.Cleanup(srv.Close)

	if out, isErr := rpc(t, srv, "rcl_pod1", "agent_resume", `{"agent_id":"run-1","lease_ttl_seconds":30}`); isErr {
		t.Fatalf("first resume: %s", out)
	}
	if out, isErr := rpc(t, srv, "rcl_pod1", "update_working_state", `{"agent_id":"run-1","goal":"send the renewal notice","progress":"draft written"}`); isErr {
		t.Fatalf("working state: %s", out)
	}
	if out, isErr := rpc(t, srv, "rcl_pod1", "begin_action", `{"agent_id":"run-1","tool":"send_email","class":"external","args":"{\"to\":\"ops@example.com\"}"}`); isErr {
		t.Fatalf("begin_action: %s", out)
	}
	// pool-aaaaa dies here: no end_action, no release.

	// The same session resuming again is still refused, as before.
	if out, isErr := rpc(t, srv, "rcl_pod1", "agent_resume", `{"agent_id":"run-1"}`); !isErr || !strings.Contains(out, "already resumed") {
		t.Fatalf("same session resumed twice: %v %s", isErr, out)
	}

	out, isErr := rpc(t, srv, "rcl_pod2", "agent_resume", `{"agent_id":"run-1"}`)
	if isErr {
		t.Fatalf("new pod's resume: %s", out)
	}
	for _, want := range []string{"send the renewal notice", "draft written", "send_email", "Unfinished actions"} {
		if !strings.Contains(out, want) {
			t.Errorf("resume briefing lacks %q: %s", want, out)
		}
	}

	// The old pod's key reaches the namespace but no longer the agent.
	for _, tool := range []string{"update_working_state", "agent_release"} {
		out, isErr := rpc(t, srv, "rcl_pod1", tool, `{"agent_id":"run-1","progress":"stale write"}`)
		if !isErr || !strings.Contains(out, `resumed by session "pool-bbbbb"`) {
			t.Errorf("%s from the old pod: %v %s", tool, isErr, out)
		}
	}
	if out, isErr := rpc(t, srv, "rcl_pod2", "update_working_state", `{"agent_id":"run-1","progress":"checked the outbox"}`); isErr {
		t.Fatalf("new pod's write: %s", out)
	}
	if out, isErr := rpc(t, srv, "rcl_pod2", "agent_release", `{"agent_id":"run-1"}`); isErr || !strings.Contains(out, `"released":true`) {
		t.Fatalf("release: %v %s", isErr, out)
	}
}
