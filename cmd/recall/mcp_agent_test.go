package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// newAgentMCP returns an MCP session with the agent tools live, backed by a
// real HTTP server, so the records and leases go through the same filters,
// typed metadata and lease routes a deployment uses.
func newAgentMCP(t *testing.T, url string) *mcpServer {
	t.Helper()
	mem, err := newMemoryRuntime(&api{base: url}, "memory", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return &mcpServer{collection: "memory", write: true, memory: mem, agents: newAgentRuntime()}
}

func callTool(t *testing.T, s *mcpServer, name string, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return s.runTool(t.Context(), name, raw)
}

func mustTool(t *testing.T, s *mcpServer, name string, args map[string]any) map[string]any {
	t.Helper()
	out, err := callTool(t, s, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%s returned %s", name, out)
	}
	return v
}

func TestAgentToolsResumeAcrossSessions(t *testing.T) {
	polignURL := polignServer(t)

	first := newAgentMCP(t, polignURL)
	rc := mustTool(t, first, "agent_resume", map[string]any{"agent_id": "coder-1"})
	if rc["fresh"] != true {
		t.Fatalf("first resume = %v", rc)
	}
	mustTool(t, first, "remember", map[string]any{"subject": "repo", "predicate": "prefers_test_framework", "value": "pytest"})
	mustTool(t, first, "update_working_state", map[string]any{"agent_id": "coder-1", "goal": "migrate billing to the v2 API", "focus": "the test framework", "plan": []string{"find call sites", "migrate"}})
	// A partial update keeps what it does not name.
	ws := mustTool(t, first, "update_working_state", map[string]any{"agent_id": "coder-1", "progress": "call sites listed"})
	if ws["goal"] != "migrate billing to the v2 API" || ws["version"].(float64) != 2 {
		t.Fatalf("merged working state = %v", ws)
	}
	for i := 1; i <= 70; i++ {
		mustTool(t, first, "record_turn", map[string]any{"agent_id": "coder-1", "role": "assistant", "content": fmt.Sprintf("step %d", i)})
	}
	mustTool(t, first, "set_pointer", map[string]any{
		"agent_id": "coder-1", "name": "wip", "type": "git_ref",
		"fields": map[string]string{"repo": "github.com/acme/billing", "branch": "agent/coder-1/wip", "sha": "abc123"},
	})
	mustTool(t, first, "set_pointer", map[string]any{"agent_id": "coder-1", "name": "scratch", "type": "object", "fields": map[string]string{"uri": "s3://b/scratch.tar"}})
	mustTool(t, first, "remove_pointer", map[string]any{"agent_id": "coder-1", "name": "scratch"})
	out := mustTool(t, first, "store_output", map[string]any{"agent_id": "coder-1", "tool": "grep", "content": strings.Repeat("billing.charge(\n", 50)})

	// A second process cannot take the agent while the first holds it.
	second := newAgentMCP(t, polignURL)
	if _, err := callTool(t, second, "agent_resume", map[string]any{"agent_id": "coder-1"}); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("concurrent resume err = %v, want held", err)
	}

	// The first session ends cleanly; the second takes over at once.
	first.agents.releaseAll()
	rc = mustTool(t, second, "agent_resume", map[string]any{"agent_id": "coder-1"})
	briefing, _ := rc["briefing"].(string)
	for _, want := range []string{"migrate billing to the v2 API", "call sites listed", "agent/coder-1/wip", "prefers_test_framework: pytest", "[assistant #70] step 70", out["ref"].(string)} {
		if !strings.Contains(briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, briefing)
		}
	}
	if strings.Contains(briefing, "scratch") {
		t.Errorf("removed pointer came back:\n%s", briefing)
	}
	if rc["turn_seq"].(float64) != 70 || rc["epoch"].(float64) < 2 {
		t.Fatalf("turn_seq %v epoch %v", rc["turn_seq"], rc["epoch"])
	}
	fetched := mustTool(t, second, "fetch_output", map[string]any{"agent_id": "coder-1", "ref": out["ref"]})
	if !strings.HasPrefix(fetched["content"].(string), "billing.charge(") {
		t.Fatalf("fetched = %v", fetched)
	}
	turn := mustTool(t, second, "record_turn", map[string]any{"agent_id": "coder-1", "role": "user", "content": "continue"})
	if turn["seq"].(float64) != 71 {
		t.Fatalf("next turn seq = %v", turn["seq"])
	}
	// The memory collection is untouched by agent records: exact recall
	// across every subject still decodes.
	if _, err := callTool(t, second, "recall", map[string]any{"predicate": "prefers_test_framework"}); err != nil {
		t.Fatalf("memory recall after agent writes: %v", err)
	}
}

func TestAgentToolsHiddenWithoutFlag(t *testing.T) {
	s := newMemoryMCP(t)
	for _, tool := range s.memoryTools() {
		if agentTools[tool.Name] {
			t.Fatalf("agent tool %q listed without -agent", tool.Name)
		}
	}
	if _, err := s.runTool(t.Context(), "agent_resume", json.RawMessage(`{"agent_id":"a"}`)); err == nil {
		t.Fatal("agent_resume ran without -agent")
	}
}

func TestAgentToolsDeferredResume(t *testing.T) {
	polignURL := polignServer(t)

	crashed := newAgentMCP(t, polignURL)
	mustTool(t, crashed, "agent_resume", map[string]any{"agent_id": "call-1", "lease_ttl_seconds": 5})
	mustTool(t, crashed, "update_working_state", map[string]any{"agent_id": "call-1", "goal": "rebook the flight"})

	// The crashed session never releases. A deferred resume still returns
	// the context at once.
	next := newAgentMCP(t, polignURL)
	rc := mustTool(t, next, "agent_resume", map[string]any{"agent_id": "call-1", "defer_lease": true})
	if rc["lease_held"] != false || !strings.Contains(rc["briefing"].(string), "rebook the flight") {
		t.Fatalf("deferred resume = %v", rc)
	}
	if _, err := callTool(t, next, "record_turn", map[string]any{"agent_id": "call-1", "role": "assistant", "content": "hi"}); err == nil || !strings.Contains(err.Error(), "not acquired") {
		t.Fatalf("write before acquire err = %v", err)
	}
	if _, err := callTool(t, next, "agent_acquire", map[string]any{"agent_id": "call-1"}); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("acquire while held err = %v", err)
	}
	crashed.agents.releaseAll()
	got := mustTool(t, next, "agent_acquire", map[string]any{"agent_id": "call-1"})
	if got["acquired"] != true {
		t.Fatalf("acquire = %v", got)
	}
	mustTool(t, next, "record_turn", map[string]any{"agent_id": "call-1", "role": "assistant", "content": "sorry, we got cut off"})
}
