package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// leasingBackend is the locked fake plus an in-memory lease chain with the
// server's semantics: acquire fails while an epoch is live, renew claims the
// next epoch and fails once someone else has.
type leasingBackend struct {
	mu          sync.Mutex
	collections map[string]*fakeDB

	lmu    sync.Mutex
	leases map[string]*fakeLease
	renews int
}

type fakeLease struct {
	epoch   uint64
	holder  string
	expires time.Time
}

func newLeasingBackend() *leasingBackend {
	return &leasingBackend{collections: map[string]*fakeDB{}, leases: map[string]*fakeLease{}}
}

// db returns the collection's own fake: unlike lockedBackend, collections
// here are separate, as they are in a real database.
func (b *leasingBackend) db(c string) *fakeDB {
	if b.collections[c] == nil {
		b.collections[c] = newFakeDB()
	}
	return b.collections[c]
}

func (b *leasingBackend) Put(_ context.Context, c, id string, v []float32, md map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.db(c).Put(c, id, v, md)
}

func (b *leasingBackend) List(_ context.Context, c string, f map[string]any, n int) ([]StoredVector, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.db(c).List(c, f, n)
}

func (b *leasingBackend) Search(_ context.Context, c string, v []float32, n int, f map[string]any) ([]Hit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.db(c).Search(c, v, n, f)
}

func (b *leasingBackend) AcquireLease(_ context.Context, c, name, holder string, ttl time.Duration) (LeaseGrant, error) {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	l := b.leases[c+"/"+name]
	if l != nil && time.Now().Before(l.expires) {
		return LeaseGrant{}, &LeaseHeldError{Holder: l.holder, Expires: l.expires}
	}
	next := &fakeLease{epoch: 1, holder: holder, expires: time.Now().Add(ttl)}
	if l != nil {
		next.epoch = l.epoch + 1
	}
	b.leases[c+"/"+name] = next
	return LeaseGrant{Epoch: next.epoch, Holder: holder, TTL: ttl}, nil
}

func (b *leasingBackend) RenewLease(_ context.Context, c, name, holder string, epoch uint64, ttl time.Duration) (LeaseGrant, error) {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	b.renews++
	l := b.leases[c+"/"+name]
	if l == nil || l.epoch != epoch {
		return LeaseGrant{}, ErrLeaseLost
	}
	l.epoch++
	l.expires = time.Now().Add(ttl)
	return LeaseGrant{Epoch: l.epoch, Holder: holder, TTL: ttl}, nil
}

func (b *leasingBackend) ReleaseLease(_ context.Context, c, name, _ string, epoch uint64) error {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	if l := b.leases[c+"/"+name]; l != nil && l.epoch == epoch {
		l.epoch++
		l.expires = time.Time{}
	}
	return nil
}

// expire makes the named lease takeable, as if its holder had died.
func (b *leasingBackend) expire(c, name string) {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	b.leases[c+"/"+name].expires = time.Time{}
}

func newAgentClient(t *testing.T, backend Backend) *Client {
	t.Helper()
	c, err := NewClient(Config{Backend: backend, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResumeFreshAgent(t *testing.T) {
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	a, err := c.Resume(t.Context(), ResumeRequest{AgentID: "coder-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release(t.Context()) }()
	rc := a.Resumed()
	if !rc.Fresh || rc.WorkingState != nil || rc.Epoch != 1 || a.Collection() != "memory_agents" {
		t.Fatalf("fresh resume = %+v", rc)
	}
	if !strings.Contains(rc.Briefing, "starting for the first time") {
		t.Fatalf("briefing = %q", rc.Briefing)
	}
}

func TestResumeRebuildsWhatTheLastProcessWrote(t *testing.T) {
	ctx := t.Context()
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	if _, err := c.Remember(ctx, RememberRequest{Subject: "repo", Predicate: "prefers_test_framework", Value: "pytest"}); err != nil {
		t.Fatal(err)
	}

	a, err := c.Resume(ctx, ResumeRequest{AgentID: "coder-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "migrate billing to v2 API", Plan: []string{"find call sites", "migrate", "run tests"},
		Focus: "which test framework the repo uses", Step: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPointer(ctx, Pointer{Name: "wip", Type: PointerGitRef, Fields: map[string]string{"repo": "github.com/acme/billing", "branch": "agent/coder-1/wip", "sha": "abc123"}}); err != nil {
		t.Fatal(err)
	}
	// More turns than one page, and most of them after the last working
	// state update: resume must find them without the note's help.
	for i := 1; i <= 150; i++ {
		if _, err := a.RecordTurn(ctx, Turn{Role: "assistant", Content: fmt.Sprintf("step %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Milestone(ctx, "call sites found", "42 call sites listed"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "keep going", MessageID: "msg-151"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "late"}); !errors.Is(err, ErrAgentClosed) {
		t.Fatalf("write after release err = %v, want ErrAgentClosed", err)
	}

	next, err := c.Resume(ctx, ResumeRequest{AgentID: "coder-1"})
	if err != nil {
		t.Fatalf("resume after release: %v", err)
	}
	defer func() { _ = next.Release(ctx) }()
	rc := next.Resumed()
	if rc.Fresh || rc.WorkingState == nil || rc.WorkingState.Version != 2 || rc.WorkingState.LastMilestone != "call sites found" {
		t.Fatalf("working state = %+v", rc.WorkingState)
	}
	if rc.TurnSeq != 151 || len(rc.RecentTurns) == 0 || rc.RecentTurns[len(rc.RecentTurns)-1].MessageID != "msg-151" {
		t.Fatalf("turn seq %d, last turns %+v", rc.TurnSeq, rc.RecentTurns)
	}
	if len(rc.Pointers) != 1 || rc.Pointers[0].Fields["sha"] != "abc123" {
		t.Fatalf("pointers = %+v", rc.Pointers)
	}
	if len(rc.Memories) == 0 || rc.Memories[0].Value != "pytest" {
		t.Fatalf("memories = %+v", rc.Memories)
	}
	for _, want := range []string{"migrate billing to v2 API", "agent/coder-1/wip", "prefers_test_framework: pytest", "[user #151] keep going"} {
		if !strings.Contains(rc.Briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, rc.Briefing)
		}
	}
	// New turns continue the sequence rather than overwrite it.
	turn, err := next.RecordTurn(ctx, Turn{Role: "assistant", Content: "resumed"})
	if err != nil || turn.Seq != 152 {
		t.Fatalf("next turn = %+v, %v", turn, err)
	}
	history, err := next.WorkingStateHistory(ctx, 10)
	if err != nil || len(history) != 2 || history[0].Version != 2 || history[1].Version != 1 {
		t.Fatalf("history = %+v, %v", history, err)
	}
}

func TestLargeTurnIsStoredAsOutput(t *testing.T) {
	ctx := t.Context()
	c := newAgentClient(t, newLeasingBackend())
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "a", OutputThreshold: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release(ctx) }()
	big := strings.Repeat("row,value\n", 200)
	turn, err := a.RecordTurn(ctx, Turn{Role: "tool", Name: "sql", Content: big})
	if err != nil {
		t.Fatal(err)
	}
	if turn.OutputRef == "" || len(turn.Content) >= len(big) {
		t.Fatalf("turn kept the whole output: ref %q, %d bytes", turn.OutputRef, len(turn.Content))
	}
	out, err := a.FetchOutput(ctx, turn.OutputRef)
	if err != nil || out.Content != big || out.Tool != "sql" {
		t.Fatalf("fetch = %d bytes, tool %q, %v", len(out.Content), out.Tool, err)
	}
}

func TestLeaseKeepsOneProcessPerAgent(t *testing.T) {
	ctx := t.Context()
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "a", LeaseTTL: 30 * time.Second, NoKeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, ResumeRequest{AgentID: "a"}); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second resume err = %v, want ErrLeaseHeld", err)
	}

	// The first process stalls past its TTL and a second one takes over.
	b.expire("memory_agents", "a")
	rival, err := c.Resume(ctx, ResumeRequest{AgentID: "a", NoKeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rival.Release(ctx) }()
	// The stalled process wakes up. Its next write must renew first, and the
	// renewal finds the chain moved on.
	a.lease.mu.Lock()
	a.lease.granted = time.Now().Add(-time.Minute)
	a.lease.mu.Unlock()
	if _, err := a.RecordTurn(ctx, Turn{Role: "assistant", Content: "zombie"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("zombie write err = %v, want ErrLeaseLost", err)
	}
	turns, err := rival.RecentTurns(ctx, 10)
	if err != nil || len(turns) != 0 {
		t.Fatalf("zombie turn landed: %+v, %v", turns, err)
	}
}

func TestResumeNeedsLeasesUnlessToldOtherwise(t *testing.T) {
	c := newAgentClient(t, newLockedBackend())
	if _, err := c.Resume(t.Context(), ResumeRequest{AgentID: "a"}); !errors.Is(err, ErrLeaseUnsupported) {
		t.Fatalf("err = %v, want ErrLeaseUnsupported", err)
	}
	a, err := c.Resume(t.Context(), ResumeRequest{AgentID: "a", Unleased: true})
	if err != nil || a.Epoch() != 0 {
		t.Fatalf("unleased resume = %v", err)
	}
	if _, err := c.Resume(t.Context(), ResumeRequest{AgentID: "a", Collection: "memory", Unleased: true}); err == nil {
		t.Fatal("resume accepted the memory collection for agent records")
	}
}

func TestBudgetKeepsWorkingStateAndDropsOldTurns(t *testing.T) {
	ctx := t.Context()
	c := newAgentClient(t, newLeasingBackend())
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "a goal that must survive any budget"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := a.RecordTurn(ctx, Turn{Role: "assistant", Content: strings.Repeat(fmt.Sprintf("turn %d ", i), 20)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Release(ctx)
	next, err := c.Resume(ctx, ResumeRequest{AgentID: "a", TokenBudget: 600})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Release(ctx) }()
	rc := next.Resumed()
	if !strings.Contains(rc.Briefing, "a goal that must survive any budget") {
		t.Fatal("working state dropped")
	}
	if rc.Tokens > 600 || rc.Omitted.Turns == 0 || len(rc.RecentTurns) == 0 {
		t.Fatalf("tokens %d, kept %d turns, omitted %d", rc.Tokens, len(rc.RecentTurns), rc.Omitted.Turns)
	}
	if last := rc.RecentTurns[len(rc.RecentTurns)-1]; last.Seq != 40 {
		t.Fatalf("budget kept turn %d last, want the newest", last.Seq)
	}
}

func TestPointerValidationAndRemoval(t *testing.T) {
	ctx := t.Context()
	c := newAgentClient(t, newLeasingBackend())
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release(ctx) }()
	if _, err := a.SetPointer(ctx, Pointer{Name: "x", Type: PointerObject}); err == nil {
		t.Fatal("object pointer without a uri accepted")
	}
	if _, err := a.SetPointer(ctx, Pointer{Name: "x", Type: "tarball", Fields: map[string]string{"uri": "s3://b/k"}}); err == nil {
		t.Fatal("unknown pointer type accepted")
	}
	if _, err := a.SetPointer(ctx, Pointer{Name: "report", Type: PointerObject, Fields: map[string]string{"uri": "s3://b/report-v2.md"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPointer(ctx, Pointer{Name: "report", Type: PointerObject, Fields: map[string]string{"uri": "s3://b/report-v3.md"}}); err != nil {
		t.Fatal(err)
	}
	ps, err := a.Pointers(ctx)
	if err != nil || len(ps) != 1 || ps[0].Fields["uri"] != "s3://b/report-v3.md" {
		t.Fatalf("pointers = %+v, %v", ps, err)
	}
	if err := a.RemovePointer(ctx, "report"); err != nil {
		t.Fatal(err)
	}
	if ps, err := a.Pointers(ctx); err != nil || len(ps) != 0 {
		t.Fatalf("after removal = %+v, %v", ps, err)
	}
}

func TestDeferredResumeReadsNowAndWritesAfterTheLease(t *testing.T) {
	ctx := t.Context()
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	old, err := c.Resume(ctx, ResumeRequest{AgentID: "call-1", NoKeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.UpdateWorkingState(ctx, WorkingState{Goal: "rebook the flight"}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.RecordTurn(ctx, Turn{Role: "user", Content: "the 9am one please"}); err != nil {
		t.Fatal(err)
	}

	// The old worker has not released: a normal resume would have to wait.
	next, err := c.Resume(ctx, ResumeRequest{AgentID: "call-1", Deferred: true, NoKeepAlive: true})
	if err != nil {
		t.Fatalf("deferred resume while held: %v", err)
	}
	defer func() { _ = next.Release(ctx) }()
	rc := next.Resumed()
	if rc.LeaseHeld || rc.WorkingState == nil || rc.WorkingState.Goal != "rebook the flight" || rc.TurnSeq != 1 {
		t.Fatalf("deferred context = %+v", rc)
	}
	if _, err := next.RecordTurn(ctx, Turn{Role: "assistant", Content: "sorry, we got cut off"}); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("write before acquire err = %v, want ErrLeaseNotHeld", err)
	}
	if err := next.AcquireLease(ctx); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("acquire while held err = %v, want ErrLeaseHeld", err)
	}

	// The old worker got one more turn out before it died.
	if _, err := old.RecordTurn(ctx, Turn{Role: "assistant", Content: "booking the 9am"}); err != nil {
		t.Fatal(err)
	}
	b.expire("memory_agents", "call-1")

	if err := next.AcquireLease(ctx); err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	if !next.LeaseHeld() || next.Epoch() < 2 {
		t.Fatalf("held %v epoch %d", next.LeaseHeld(), next.Epoch())
	}
	turn, err := next.RecordTurn(ctx, Turn{Role: "assistant", Content: "sorry, we got cut off"})
	if err != nil || turn.Seq != 3 {
		t.Fatalf("first write after acquire = %+v, %v; want seq 3, after the old worker's last turn", turn, err)
	}
	turns, err := next.RecentTurns(ctx, 10)
	if err != nil || len(turns) != 3 || turns[1].Content != "booking the 9am" {
		t.Fatalf("turns = %+v, %v", turns, err)
	}
}
