package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// postHeaders is postWith that also returns the answer's headers.
func postHeaders(t *testing.T, url, key, body string, headers map[string]string) (int, http.Header, string) {
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
	return resp.StatusCode, resp.Header, readAll(t, resp)
}

// An Agent Substrate actor is reverted to an earlier snapshot: its writes
// carry the position it last saw, a replay of what it already did is the
// recorded result, and anything new from the old position is refused until
// it catches up.
func TestServeStateAPIPositions(t *testing.T) {
	srv := newStateServer(t, staticAuth{"rcl_actor": {Namespace: "ate:job-1", Agent: "job-1", Substrate: true}})
	agent := srv.URL + "/v1/agent/"
	at := func(p string) map[string]string { return map[string]string{"If-Position": p} }
	call := func(verb, body string, headers map[string]string) (int, http.Header, string) {
		t.Helper()
		return postHeaders(t, agent+verb, "rcl_actor", body, headers)
	}

	status, h, body := call("resume", `{"agent_id":"job-1"}`, nil)
	if status != http.StatusOK || h.Get("Recall-Position") != "0.0.0" || !strings.Contains(body, `"position":{"turns":0,"actions":0,"state":0}`) {
		t.Fatalf("resume: %d %s %s", status, h.Get("Recall-Position"), body)
	}
	if status, _, body := call("record_turn", `{"agent_id":"job-1","role":"user","content":"bill maria"}`, nil); status != http.StatusBadRequest || !strings.Contains(body, "If-Position") {
		t.Fatalf("actor write without at: %d %s", status, body)
	}
	if status, _, body := call("record_turn", `{"agent_id":"job-1","role":"user","content":"bill maria","at":"0.0.1"}`, at("0.0.0")); status != http.StatusBadRequest || !strings.Contains(body, "disagree") {
		t.Fatalf("at and If-Position disagreeing: %d %s", status, body)
	}

	// The job, as the actor runs it before the snapshot it is reverted to.
	turn := `{"agent_id":"job-1","role":"user","content":"bill maria","message_id":"job-user-1"}`
	status, h, body = call("record_turn", turn, at("0.0.0"))
	if status != http.StatusOK || h.Get("Recall-Position") != "1.0.0" {
		t.Fatalf("turn: %d %s %s", status, h.Get("Recall-Position"), body)
	}
	begin := `{"agent_id":"job-1","tool":"send_invoice","class":"external","call_id":"call-1"}`
	status, _, body = call("begin_action", begin, at("1.0.0"))
	if status != http.StatusOK || !strings.Contains(body, `"seq":1`) {
		t.Fatalf("begin: %d %s", status, body)
	}
	if status, h, body := call("end_action", `{"agent_id":"job-1","seq":1,"status":"done","result":"invoice 42"}`, at("1.1.0")); status != http.StatusOK || h.Get("Recall-Position") != "1.1.0" {
		t.Fatalf("end: %d %s", status, body)
	}

	// Reverted to 0.0.0, the actor runs the job again. The ids make the
	// replay the recorded results, so the invoice is not sent twice.
	if status, _, body := call("record_turn", turn, at("0.0.0")); status != http.StatusOK || !strings.Contains(body, `"seq":1`) {
		t.Fatalf("replayed turn: %d %s", status, body)
	}
	status, _, body = call("begin_action", begin, at("0.0.0"))
	if status != http.StatusOK || !strings.Contains(body, `"status":"done"`) || !strings.Contains(body, "invoice 42") {
		t.Fatalf("replayed begin: %d %s, want the done action", status, body)
	}
	// Something it did not do before is refused from the old position.
	status, h, body = call("record_turn", `{"agent_id":"job-1","role":"assistant","content":"sending the invoice now","message_id":"new-1"}`, at("0.0.0"))
	var refused struct {
		Error    struct{ Code string }
		Position struct{ Turns, Actions int64 }
	}
	if status != http.StatusConflict || json.Unmarshal([]byte(body), &refused) != nil || refused.Error.Code != "position_behind" || refused.Position.Turns != 1 || refused.Position.Actions != 1 || h.Get("Recall-Position") != "1.1.0" {
		t.Fatalf("new turn from an old position: %d %s", status, body)
	}

	// It catches up and carries on from the records.
	status, h, body = call("since", `{"agent_id":"job-1","at":"0.0.0"}`, nil)
	if status != http.StatusOK || !strings.Contains(body, "send_invoice") || !strings.Contains(body, "do not do them again") || h.Get("Recall-Position") != "1.1.0" {
		t.Fatalf("since: %d %s", status, body)
	}
	if status, _, body := call("since", `{"agent_id":"job-1"}`, nil); status != http.StatusBadRequest {
		t.Fatalf("since without at: %d %s", status, body)
	}
	if status, _, body := call("record_turn", `{"agent_id":"job-1","role":"assistant","content":"the invoice was already sent","message_id":"new-1"}`, at("1.1.0")); status != http.StatusOK || !strings.Contains(body, `"seq":2`) {
		t.Fatalf("turn after catching up: %d %s", status, body)
	}
	if status, _, body := call("recent_turns", `{"agent_id":"job-1","at":"2.1.0"}`, nil); status != http.StatusBadRequest {
		t.Fatalf("a read with at: %d %s", status, body)
	}
}

// A bound credential never has to resume: after a server restart or an idle
// release, its next call resumes the agent in place, and a second resume
// answers with the context instead of already_resumed. A plain key still
// has to resume.
func TestServeStateAPIBoundCallerResumesInPlace(t *testing.T) {
	auth := staticAuth{"rcl_actor": {Namespace: "ate:a1", Agent: "a1", Substrate: true}, "rcl_plain": {Namespace: "plain"}}
	backend, err := recall.OpenBackend("polign", recall.BackendOptions{URL: polignServer(t)})
	if err != nil {
		t.Fatal(err)
	}
	tenants := newTenantServers(serveConfig{backend: backend, registry: recall.DefaultRegistry(), embedder: recall.LexicalEmbedder{}, base: "memories", agents: true, agentIdle: time.Hour})
	t.Cleanup(tenants.releaseAll)
	srv := newTestServerWith(t, auth, tenants, nil)
	agent := srv.URL + "/v1/agent/"
	at := map[string]string{"If-Position": "0.0.0"}

	// No resume first: the write resumes the agent.
	if status, body := postWith(t, agent+"update_working_state", "rcl_actor", `{"agent_id":"a1","goal":"renewals"}`, at); status != http.StatusOK {
		t.Fatalf("first write with no resume: %d %s", status, body)
	}
	if status, body := post(t, agent+"resume", "rcl_actor", `{"agent_id":"a1"}`); status != http.StatusOK || !strings.Contains(body, "renewals") {
		t.Fatalf("resume of a held agent: %d %s", status, body)
	}

	// Idle past -agent-idle: released, and the next call takes it back.
	s, err := tenants.get("ate:a1")
	if err != nil {
		t.Fatal(err)
	}
	s.agents.releaseIdle(time.Now().Add(time.Minute))
	s.agents.mu.Lock()
	held := s.agents.agents["a1"] != nil
	s.agents.mu.Unlock()
	if held {
		t.Fatal("idle agent still held")
	}
	if status, body := postWith(t, agent+"record_turn", "rcl_actor", `{"agent_id":"a1","role":"user","content":"next"}`, map[string]string{"If-Position": "0.0.1"}); status != http.StatusOK {
		t.Fatalf("write after an idle release: %d %s", status, body)
	}

	if status, body := post(t, agent+"record_turn", "rcl_plain", `{"agent_id":"p1","role":"user","content":"x","epoch":1}`); status != http.StatusConflict || !strings.Contains(body, "not_resumed") {
		t.Fatalf("plain key with no resume: %d %s", status, body)
	}
}
