package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Polign/recall"
)

// fakeMemoryDB is an in-memory recall.VectorDB, so the tool layer is tested
// for argument handling, dispatch, and output shape without an HTTP server.
type fakeMemoryDB struct {
	mu      sync.Mutex
	records map[string]recall.StoredVector
	order   []string
}

func newFakeMemoryDB() *fakeMemoryDB { return &fakeMemoryDB{records: map[string]recall.StoredVector{}} }

func (f *fakeMemoryDB) Put(_, id string, values []float32, md map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.records[id]; !ok {
		f.order = append(f.order, id)
	}
	f.records[id] = recall.StoredVector{ID: id, Values: values, Metadata: md}
	return nil
}

func (f *fakeMemoryDB) matching(filter map[string]any, limit int) []recall.StoredVector {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recall.StoredVector
	for _, id := range f.order {
		rec := f.records[id]
		ok := true
		for k, want := range filter {
			if fmt.Sprint(rec.Metadata[k]) != fmt.Sprint(want) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, rec)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (f *fakeMemoryDB) List(_ string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	got := f.matching(filter, limit)
	return got, len(f.matching(filter, 0)), nil
}

func (f *fakeMemoryDB) Search(_ string, _ []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	var out []recall.Hit
	for _, rec := range f.matching(filter, k) {
		out = append(out, recall.Hit{ID: rec.ID, Metadata: rec.Metadata})
	}
	return out, nil
}

// newMemoryMCP returns an MCP server with the memory tools live over a fake
// store, and no HTTP adapter behind it.
func newMemoryMCP(t *testing.T) *mcpServer {
	t.Helper()
	reg, err := recall.LoadRegistry([]byte(`{
		"prefers_editor": {"cardinality": "single", "value_type": "string", "description": "editor"},
		"likes":          {"cardinality": "multi",  "value_type": "string", "description": "likes"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	store := recall.NewStore(newFakeMemoryDB(), "memories", reg, func(string) []float32 { return []float32{1, 0, 0} })
	return &mcpServer{
		api:        &api{base: "http://unused"},
		write:      true,
		collection: "memories",
		memory:     &memoryRuntime{store: store},
	}
}

// step issues one tool call and waits for its result. serve dispatches every
// request concurrently, so a test that needs one call to land before the next
// must sequence them here, as a real client does by waiting for each reply.
func step(t *testing.T, s *mcpServer, name, args string) (string, bool) {
	t.Helper()
	resps := runMCP(t, s, call(1, name, args))
	return toolText(t, resps[1])
}

func call(id int, name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

func toolNames(t *testing.T, resp rpcResponse) []string {
	t.Helper()
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	resultOf(t, resp, &r)
	out := make([]string, len(r.Tools))
	for i, tool := range r.Tools {
		out[i] = tool.Name
	}
	return out
}

func TestMCPMemoryToolsAbsentWithoutARuntime(t *testing.T) {
	s := &mcpServer{api: &api{base: "http://unused"}, collection: "memories"}
	resps := runMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	for _, name := range toolNames(t, resps[1]) {
		if name == "remember" || name == "recall" {
			t.Fatalf("memory tool %q advertised with no registry or embedder to back it", name)
		}
	}
}

func TestMCPListsOnlyMemoryTools(t *testing.T) {
	resps := runMCP(t, newMemoryMCP(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	names := toolNames(t, resps[1])
	want := []string{"list_predicates", "remember", "recall", "forget", "explain", "memory_history"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestMCPRememberThenRecall(t *testing.T) {
	s := newMemoryMCP(t)
	stored, isErr := step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim","kind":"preference"}`)
	if isErr {
		t.Fatalf("remember failed: %s", stored)
	}
	// The model reads these names, so they must be the snake_case ones.
	for _, field := range []string{`"subject":"user"`, `"predicate":"prefers_editor"`, `"value":"neovim"`, `"observed_at":`, `"event_id":`} {
		if !strings.Contains(stored, field) {
			t.Errorf("remember result missing %s: %s", field, stored)
		}
	}
	if strings.Contains(stored, `"Subject"`) {
		t.Errorf("Go-style field name leaked to the model: %s", stored)
	}

	recalled, isErr := step(t, s, "recall", `{"subject":"user","predicate":"prefers_editor"}`)
	if isErr {
		t.Fatalf("recall failed: %s", recalled)
	}
	var beliefs []map[string]any
	if err := json.Unmarshal([]byte(recalled), &beliefs); err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0]["value"] != "neovim" {
		t.Fatalf("recalled %v, want neovim", beliefs)
	}
}

func TestMCPRememberReportsSupersession(t *testing.T) {
	s := newMemoryMCP(t)
	step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"vim"}`)
	second, _ := step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim"}`)
	if !strings.Contains(second, `"superseded":[`) || !strings.Contains(second, `"value":"vim"`) {
		t.Fatalf("second remember did not report displacing vim: %s", second)
	}
	// The answer is neovim alone, and it carries the correction: the vim it
	// replaced rides along under replaced rather than as a second belief.
	recalled, _ := step(t, s, "recall", `{"subject":"user","predicate":"prefers_editor"}`)
	var beliefs []recall.Belief
	if err := json.Unmarshal([]byte(recalled), &beliefs); err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "neovim" {
		t.Fatalf("recalled %s, want neovim alone", recalled)
	}
	if r := beliefs[0].Replaced; len(r) != 1 || r[0].Value != "vim" || r[0].Source != "user_stated" {
		t.Fatalf("neovim replaced %+v, want vim", r)
	}
}

func TestMCPRestatementIsAlreadyKnown(t *testing.T) {
	s := newMemoryMCP(t)
	step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim"}`)
	second, _ := step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim"}`)
	if !strings.Contains(second, `"already_known":true`) {
		t.Fatalf("restating a belief was not reported as already known: %s", second)
	}
}

func TestMCPHistoryKeepsTheSupersededStatement(t *testing.T) {
	s := newMemoryMCP(t)
	step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"vim"}`)
	step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim"}`)
	history, isErr := step(t, s, "memory_history", `{"subject":"user","predicate":"prefers_editor"}`)
	if isErr {
		t.Fatalf("history failed: %s", history)
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(history), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0]["value"] != "vim" || events[1]["value"] != "neovim" {
		t.Fatalf("history = %v, want vim then neovim", events)
	}
}

func TestMCPForgetWithdrawsAndRecallEmpties(t *testing.T) {
	s := newMemoryMCP(t)
	step(t, s, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim"}`)
	forgot, _ := step(t, s, "forget", `{"subject":"user","predicate":"prefers_editor"}`)
	if !strings.Contains(forgot, `"withdrawn":1`) {
		t.Fatalf("forget = %s, want withdrawn 1", forgot)
	}
	recalled, _ := step(t, s, "recall", `{"subject":"user","predicate":"prefers_editor"}`)
	if strings.TrimSpace(recalled) != "[]" {
		t.Fatalf("recall after forget = %s, want []", recalled)
	}
}

func TestMCPRecallRejectsABadInstant(t *testing.T) {
	resps := runMCP(t, newMemoryMCP(t),
		call(1, "recall", `{"subject":"user","predicate":"prefers_editor","as_of":"last tuesday"}`),
	)
	text, isErr := toolText(t, resps[1])
	if !isErr || !strings.Contains(text, "RFC3339") {
		t.Fatalf("bad as_of accepted or badly explained: isErr=%v %s", isErr, text)
	}
}

func TestMCPRecallNeedsAQueryOrASubject(t *testing.T) {
	resps := runMCP(t, newMemoryMCP(t), call(1, "recall", `{}`))
	text, isErr := toolText(t, resps[1])
	if !isErr {
		t.Fatalf("empty recall accepted: %s", text)
	}
}

func TestMCPUnregisteredPredicateIsAToolError(t *testing.T) {
	resps := runMCP(t, newMemoryMCP(t),
		call(1, "remember", `{"subject":"user","predicate":"editor_preference","value":"emacs"}`),
	)
	text, isErr := toolText(t, resps[1])
	if !isErr {
		t.Fatalf("unregistered predicate accepted: %s", text)
	}
	// The error names the valid set, which is how the model self-corrects.
	if !strings.Contains(text, "prefers_editor") {
		t.Fatalf("error does not list the registry: %s", text)
	}
}

func TestMCPListPredicates(t *testing.T) {
	resps := runMCP(t, newMemoryMCP(t), call(1, "list_predicates", `{}`))
	text, isErr := toolText(t, resps[1])
	if isErr {
		t.Fatalf("list_predicates failed: %s", text)
	}
	if !strings.Contains(text, `"predicate":"prefers_editor"`) || !strings.Contains(text, `"cardinality":"single"`) {
		t.Fatalf("registry not rendered: %s", text)
	}
}

func TestMCPInstructionsCarryTheRegistry(t *testing.T) {
	s := newMemoryMCP(t)
	if !strings.Contains(s.instructions(), "prefers_editor") {
		t.Fatalf("instructions do not tell the model what it can remember:\n%s", s.instructions())
	}
}

func TestMCPNoteKeepsWhatNoPredicateFits(t *testing.T) {
	s := newMemoryMCP(t)
	if !strings.Contains(s.instructions(), "predicate note") {
		t.Fatalf("instructions do not offer the note fallback:\n%s", s.instructions())
	}
	if text, isErr := step(t, s, "remember", `{"subject":"user","predicate":"shell","value":"fish"}`); !isErr || !strings.Contains(text, `"note"`) {
		t.Fatalf("refusal must point at note: %s", text)
	}
	if text, isErr := step(t, s, "remember", `{"subject":"user","predicate":"note","value":"uses fish as their shell"}`); isErr {
		t.Fatalf("note refused under a registry that does not list it: %s", text)
	}
	if text, _ := step(t, s, "recall", `{"subject":"user","predicate":"note"}`); !strings.Contains(text, "uses fish as their shell") {
		t.Fatalf("note not recalled: %s", text)
	}
}
