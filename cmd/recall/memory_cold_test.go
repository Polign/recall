package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// pageCap is polign_db's HTTP page size limit (service.MaxPageSize). A
// history one longer than it proves recall pages through a whole history.
const pageCap = 1000

// Exercise MCP -> Recall -> HTTP -> a cold-first polign-server, including a
// history larger than the HTTP page cap and restarts of the server process on
// the same store, as a black box.
func TestMemoryColdLifecycle(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	registry := newMemoryMCP(t).memory.store.Registry()
	connect := func(base string) *mcpServer {
		client := &api{base: base}
		factory := func(ctx context.Context) *recall.Store {
			return recall.NewStore(&memoryDB{api: client, ctx: ctx}, "memories", registry, func(string) []float32 { return []float32{1, 0, 0} })
		}
		return &mcpServer{api: client, write: true, collection: "memories", memory: &memoryRuntime{store: factory(ctx), newStore: factory}}
	}
	tool := func(s *mcpServer, name, args string) string {
		t.Helper()
		text, bad := step(t, s, name, args)
		if bad {
			t.Fatalf("%s: %s", name, text)
		}
		return text
	}
	const selector = `"subject":"user","predicate":"prefers_editor"`
	assertRecall := func(s *mcpServer, asOf, want string) {
		t.Helper()
		args := "{" + selector
		if asOf != "" {
			args += fmt.Sprintf(`,"as_of":%q`, asOf)
		}
		text := tool(s, "recall", args+"}")
		var got []recall.Belief
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatal(err)
		}
		if want == "" {
			if len(got) != 0 {
				t.Fatalf("recall revived forgotten belief: %s", text)
			}
			return
		}
		if len(got) != 1 || got[0].Value != want {
			t.Fatalf("recall = %s, want %s", text, want)
		}
	}

	stops := []func(){}
	start := func() *mcpServer {
		t.Helper()
		base, stop := startPolignStoppable(t, serverOptions{dir: dir, coldFirst: true})
		stops = append(stops, stop)
		return connect(base)
	}
	restart := func() *mcpServer {
		t.Helper()
		stops[len(stops)-1]() // a graceful stop persists what the write log holds
		return start()
	}

	mcp := start()
	// Seed a long supersession history without quadratic setup through Remember.
	seed := &memoryDB{api: mcp.api, ctx: ctx}
	begin := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= pageCap; i++ {
		md := map[string]any{"subject": "user", "predicate": "prefers_editor", "value": fmt.Sprintf("editor-%d", i),
			"kind": "fact", "confidence": 1.0, "source": "user_stated", "observed_at": begin.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)}
		if err := seed.Put("memories", fmt.Sprintf("seed-%04d", i), []float32{1, 0, 0}, md); err != nil {
			t.Fatal(err)
		}
	}
	text := tool(mcp, "remember", "{"+selector+`,"value":"vim"}`)
	var remembered recall.RememberResult
	if err := json.Unmarshal([]byte(text), &remembered); err != nil {
		t.Fatal(err)
	}
	asOf := remembered.Stored.ObservedAt.Format(time.RFC3339Nano)
	assertRecall(mcp, "", "vim")

	mcp = restart()
	assertRecall(mcp, "", "vim")
	tool(mcp, "remember", "{"+selector+`,"value":"vim"}`) // restating holds, and adds no second belief
	assertRecall(mcp, "", "vim")
	tool(mcp, "forget", "{"+selector+"}")
	assertRecall(mcp, "", "")
	assertRecall(mcp, asOf, "vim")

	mcp = restart() // the retraction survives a restart
	assertRecall(mcp, "", "")
	assertRecall(mcp, asOf, "vim")
	var history []recall.Event
	if err := json.Unmarshal([]byte(tool(mcp, "memory_history", "{"+selector+"}")), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != pageCap+1+2 {
		t.Fatalf("history = %d events, want %d", len(history), pageCap+1+2)
	}
}
