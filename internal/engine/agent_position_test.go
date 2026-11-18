package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"
)

// creatingBackend is the fencing fake with create-only writes, checked and
// written under one lock as a server does.
type creatingBackend struct {
	*fencingBackend
	creates int
}

func newCreatingBackend() *creatingBackend {
	return &creatingBackend{fencingBackend: &fencingBackend{leasingBackend: newLeasingBackend()}}
}

func (b *creatingBackend) Create(_ context.Context, c, id string, v []float32, md map[string]any, f *Fence) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creates++
	if f != nil {
		b.lmu.Lock()
		l := b.leases[c+"/"+f.Lease]
		ok := l != nil && l.holder == f.Holder && !l.expires.IsZero() && l.epoch >= f.Epoch
		b.lmu.Unlock()
		if !ok {
			return fmt.Errorf("%w: write fenced", ErrLeaseLost)
		}
	}
	if _, exists := b.db(c).records[id]; exists {
		return fmt.Errorf("%w: %s", ErrRecordExists, id)
	}
	return b.db(c).Put(c, id, v, md)
}

// restore returns the Agent as a process restored from a snapshot taken now
// would hold it: its counters, its id sets and its lease as they are, with no
// keep-alive running. Writes through the original after this are what the
// snapshot does not know about.
func restore(a *Agent) *Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := &Agent{c: a.c, id: a.id, collection: a.collection, threshold: a.threshold, resumed: a.resumed, req: a.req,
		state: cloneWorkingState(a.state), turnSeq: a.turnSeq, actSeq: a.actSeq, outSeq: a.outSeq,
		messages: maps.Clone(a.messages), calls: maps.Clone(a.calls)}
	if l := a.lease; l != nil {
		l.mu.Lock()
		r.lease = &agentLease{lb: l.lb, collection: l.collection, name: l.name, holder: l.holder, ttl: l.ttl, ep: l.ep, granted: l.granted, grantedWall: l.grantedWall}
		l.mu.Unlock()
	}
	return r
}

func resumeAgent(t *testing.T, b Backend, id string) *Agent {
	t.Helper()
	a, err := newAgentClient(t, b).Resume(t.Context(), ResumeRequest{AgentID: id, NoKeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func turnBodies(t *testing.T, a *Agent) string {
	t.Helper()
	turns, err := a.recentTurns(t.Context(), 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, x := range turns {
		parts = append(parts, fmt.Sprintf("%d:%s", x.Seq, x.Content))
	}
	return strings.Join(parts, " ")
}

func record(t *testing.T, a *Agent, content string, opts ...WriteOption) Turn {
	t.Helper()
	turn, err := a.RecordTurn(t.Context(), Turn{Role: "user", Content: content}, opts...)
	if err != nil {
		t.Fatalf("record %s: %v", content, err)
	}
	return turn
}

// TestRestoredProcessCannotReplaceLaterTurns is Phase 0's check 10: turns A
// and B, a snapshot, C and D, a revert, then E. The restored process used to
// write E over C.
func TestRestoredProcessCannotReplaceLaterTurns(t *testing.T) {
	b := newCreatingBackend()
	a := resumeAgent(t, b, "worker")
	record(t, a, "A")
	record(t, a, "B")
	snap := restore(a)
	record(t, a, "C")
	record(t, a, "D")

	_, err := snap.RecordTurn(t.Context(), Turn{Role: "user", Content: "E"})
	var pe *PositionError
	if !errors.As(err, &pe) || !errors.Is(err, ErrPositionBehind) {
		t.Fatalf("restored write = %v, want ErrPositionBehind", err)
	}
	if pe.At != (Position{Turns: 2}) || pe.Position != (Position{Turns: 4}) {
		t.Fatalf("position error = at %s, records %s; want 2.0.0 and 4.0.0", pe.At, pe.Position)
	}
	if got := turnBodies(t, a); got != "1:A 2:B 3:C 4:D" {
		t.Fatalf("turns after a refused restored write = %s", got)
	}
	// The restored Agent caught up, so its next write goes after D.
	if turn := record(t, snap, "E"); turn.Seq != 5 {
		t.Fatalf("next write from the restored process = seq %d, want 5", turn.Seq)
	}
}

func TestRestoredProcessCannotReplaceLaterActionsOrState(t *testing.T) {
	ctx := t.Context()
	b := newCreatingBackend()
	a := resumeAgent(t, b, "mailer")
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "send the invoice"}); err != nil {
		t.Fatal(err)
	}
	snap := restore(a)
	act, err := a.BeginAction(ctx, "send_invoice", ActionExternal, `{"to":"maria"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EndAction(ctx, act.Seq, ActionEnd{Status: ActionDone, Result: "sent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "send the invoice", Progress: "sent"}); err != nil {
		t.Fatal(err)
	}

	if _, err := snap.BeginAction(ctx, "send_invoice", ActionExternal, `{"to":"maria"}`); !errors.Is(err, ErrPositionBehind) {
		t.Fatalf("restored begin = %v, want ErrPositionBehind", err)
	}
	got, err := a.action(ctx, 1)
	if err != nil || got.Status != ActionDone || got.Result != "sent" {
		t.Fatalf("action 1 after a restored begin = %+v, %v; want done", got, err)
	}
	// The restored Agent caught up with both records on the first refusal.
	if p := snap.Position(); p != (Position{Turns: 0, Actions: 1, State: 2}) {
		t.Fatalf("restored position after catching up = %s, want 0.1.2", p)
	}

	snap2 := restore(a)
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "close the ticket"}); err != nil {
		t.Fatal(err)
	}
	if _, err := snap2.UpdateWorkingState(ctx, WorkingState{Goal: "something stale"}); !errors.Is(err, ErrPositionBehind) {
		t.Fatalf("restored state update = %v, want ErrPositionBehind", err)
	}
	if ws := resumeAgentFresh(t, b, "mailer").WorkingState(); ws.Goal != "close the ticket" || ws.Version != 3 {
		t.Fatalf("working state after a restored update = %+v", ws)
	}
}

// resumeAgentFresh releases nothing: it reads the records as a new process
// would, without a lease.
func resumeAgentFresh(t *testing.T, b Backend, id string) *Agent {
	t.Helper()
	a, err := newAgentClient(t, b).Resume(t.Context(), ResumeRequest{AgentID: id, Deferred: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestOrphanWorkingStateCopyIsReplaced: a versioned copy whose head write
// failed is history nobody points at, not a sign the process is behind.
func TestOrphanWorkingStateCopyIsReplaced(t *testing.T) {
	ctx := t.Context()
	b := newCreatingBackend()
	a := resumeAgent(t, b, "noter")
	if err := b.Put(ctx, a.collection, a.recordID("ws", "1"), []float32{1}, map[string]any{"recall_agent": a.id, "record": recWorking, "body": `{"version":1}`}); err != nil {
		t.Fatal(err)
	}
	ws, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "g"})
	if err != nil || ws.Version != 1 {
		t.Fatalf("update over an orphan copy = %+v, %v", ws, err)
	}
}

func TestLargeTurnFromARestoredProcessKeepsTheLaterOutput(t *testing.T) {
	ctx := t.Context()
	b := newCreatingBackend()
	a := resumeAgent(t, b, "reader")
	a.threshold = 10
	snap := restore(a)
	first := record(t, a, strings.Repeat("first ", 40))
	if _, err := snap.RecordTurn(ctx, Turn{Role: "tool", Content: strings.Repeat("stale ", 40)}); !errors.Is(err, ErrPositionBehind) {
		t.Fatalf("restored large turn = %v, want ErrPositionBehind", err)
	}
	out, err := a.FetchOutput(ctx, first.OutputRef)
	if err != nil || !strings.HasPrefix(out.Content, "first") {
		t.Fatalf("output %s after the restored write = %q, %v", first.OutputRef, opening(out.Content, 20), err)
	}
}

func TestRepeatedMessageIDReturnsTheRecordedTurn(t *testing.T) {
	ctx := t.Context()
	b := newCreatingBackend()
	a := resumeAgent(t, b, "chat")
	first, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "hello", MessageID: "m-1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "hello, retried", MessageID: "m-1"})
	if err != nil || again.Seq != first.Seq || again.Content != "hello" {
		t.Fatalf("repeat = %+v, %v; want turn %d as recorded", again, err, first.Seq)
	}
	if p := a.Position(); p.Turns != 1 {
		t.Fatalf("turns after a repeat = %d, want 1", p.Turns)
	}

	// A new process knows the recent ids from its resume.
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	next := resumeAgent(t, b, "chat")
	if again, err := next.RecordTurn(ctx, Turn{Role: "user", Content: "hello", MessageID: "m-1"}); err != nil || again.Seq != 1 {
		t.Fatalf("repeat after a resume = %+v, %v", again, err)
	}

	// A restored process that does not know it wrote the turn finds it once
	// the create shows it is behind.
	snap := restore(next)
	snap.messages = nil
	record(t, next, "between")
	if _, err := next.RecordTurn(ctx, Turn{Role: "assistant", Content: "hi", MessageID: "m-2"}); err != nil {
		t.Fatal(err)
	}
	snap.turnSeq = 1
	got, err := snap.RecordTurn(ctx, Turn{Role: "assistant", Content: "hi", MessageID: "m-2"})
	if err != nil || got.Seq != 3 {
		t.Fatalf("replayed turn from a restored process = %+v, %v; want the recorded turn 3", got, err)
	}
}

func TestCallIDReturnsTheActionAlreadyBegun(t *testing.T) {
	ctx := t.Context()
	b := newCreatingBackend()
	a := resumeAgent(t, b, "payer")
	first, err := a.BeginAction(ctx, "charge", ActionIdempotent, `{"amount":5}`, CallID("call_1"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.BeginAction(ctx, "charge", ActionIdempotent, `{"amount":5}`, CallID("call_1"))
	if err != nil || again.Seq != first.Seq || again.Key != first.Key {
		t.Fatalf("repeated begin = %+v, %v; want action %d", again, err, first.Seq)
	}
	if _, err := a.EndAction(ctx, first.Seq, ActionEnd{Status: ActionDone, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	// A new process, with no recent ids, finds it in the records.
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	next := resumeAgent(t, b, "payer")
	got, err := next.BeginAction(ctx, "charge", ActionIdempotent, `{"amount":5}`, CallID("call_1"))
	if err != nil || got.Seq != first.Seq || got.Status != ActionDone || got.Result != "ok" {
		t.Fatalf("begin of a finished call after a resume = %+v, %v; want done", got, err)
	}
	if p := next.Position(); p.Actions != 1 {
		t.Fatalf("actions after repeats = %d, want 1", p.Actions)
	}
}

func TestRepeatedEndAction(t *testing.T) {
	ctx := t.Context()
	a := resumeAgent(t, newCreatingBackend(), "ender")
	act, err := a.BeginAction(ctx, "post", ActionExternal, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	end := ActionEnd{Status: ActionDone, Result: "posted"}
	if _, err := a.EndAction(ctx, act.Seq, end); err != nil {
		t.Fatal(err)
	}
	if got, err := a.EndAction(ctx, act.Seq, end); err != nil || got.Status != ActionDone {
		t.Fatalf("repeated end = %+v, %v; want the action back", got, err)
	}
	if _, err := a.EndAction(ctx, act.Seq, ActionEnd{Status: ActionFailed, Error: "boom"}); err == nil {
		t.Fatal("an end with a different outcome succeeded; the first outcome stands")
	}
}

func TestAtRefusesAWriteFromAnEarlierPosition(t *testing.T) {
	ctx := t.Context()
	// The plain fencing fake has no create-only writes: the position alone
	// catches a caller that is behind, as it does over HTTP.
	b := &fencingBackend{leasingBackend: newLeasingBackend()}
	a := resumeAgent(t, b, "remote")
	seen := a.Position()
	t1, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "one", MessageID: "u1"}, At(seen))
	if err != nil {
		t.Fatal(err)
	}
	// A retry of the same turn from the same position is the recorded turn.
	if again, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "one", MessageID: "u1"}, At(seen)); err != nil || again.Seq != t1.Seq {
		t.Fatalf("retried turn = %+v, %v", again, err)
	}
	// A new turn from the old position is refused.
	_, err = a.RecordTurn(ctx, Turn{Role: "user", Content: "two", MessageID: "u2"}, At(seen))
	var pe *PositionError
	if !errors.As(err, &pe) || pe.Ahead || pe.Position.Turns != 1 {
		t.Fatalf("turn from an old position = %v, want ErrPositionBehind at 1 turn", err)
	}
	if _, err := a.BeginAction(ctx, "x", ActionPure, "", At(seen)); !errors.Is(err, ErrPositionBehind) {
		t.Fatalf("begin from an old position = %v", err)
	}
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "g"}, At(seen)); !errors.Is(err, ErrPositionBehind) {
		t.Fatalf("state from an old position = %v", err)
	}
	if _, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "two"}, At(Position{Turns: 9})); !errors.Is(err, ErrPositionAhead) {
		t.Fatalf("turn from a position ahead = %v, want ErrPositionAhead", err)
	}
	if _, err := a.RecordTurn(ctx, Turn{Role: "user", Content: "two"}, At(a.Position())); err != nil {
		t.Fatalf("turn from the current position = %v", err)
	}
}

func TestSinceTellsARestoredProcessWhatItMissed(t *testing.T) {
	ctx := t.Context()
	b := newCreatingBackend()
	a := resumeAgent(t, b, "job")
	record(t, a, "start the job")
	from := a.Position()
	act, err := a.BeginAction(ctx, "send_invoice", ActionExternal, `{"to":"maria"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EndAction(ctx, act.Seq, ActionEnd{Status: ActionDone, Result: "invoice 42 sent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BeginAction(ctx, "post_receipt", ActionExternal, `{}`); err != nil {
		t.Fatal(err)
	}
	record(t, a, "invoice sent, posting the receipt")
	if _, err := a.UpdateWorkingState(ctx, WorkingState{Goal: "bill maria", Progress: "invoice sent"}); err != nil {
		t.Fatal(err)
	}

	rep, err := restore(a).Since(ctx, from, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.From != from || rep.Position != (Position{Turns: 2, Actions: 2, State: 1}) {
		t.Fatalf("since %s = position %s", rep.From, rep.Position)
	}
	if len(rep.Turns) != 1 || rep.Turns[0].Seq != 2 || len(rep.Actions) != 2 || rep.WorkingState == nil {
		t.Fatalf("since report = %d turns, %d actions, state %v", len(rep.Turns), len(rep.Actions), rep.WorkingState != nil)
	}
	for _, want := range []string{"do not do them again", "send_invoice", "invoice 42 sent", "post_receipt", "Not known whether this happened", "invoice sent, posting the receipt"} {
		if !strings.Contains(rep.Briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, rep.Briefing)
		}
	}

	if _, err := a.Since(ctx, Position{Turns: 99}, 0); !errors.Is(err, ErrPositionAhead) {
		t.Fatalf("since a position ahead = %v, want ErrPositionAhead", err)
	}
	if rep, err := a.Since(ctx, a.Position(), 0); err != nil || len(rep.Turns)+len(rep.Actions) != 0 || !strings.Contains(rep.Briefing, "nothing was recorded") {
		t.Fatalf("since now = %+v, %v", rep, err)
	}
}

// TestGuardRenewsAfterASuspendTheMonotonicClockMissed: a restored process's
// monotonic clock does not count the suspend, so the lease age also comes
// from the wall clock.
func TestGuardRenewsAfterASuspendTheMonotonicClockMissed(t *testing.T) {
	b := newCreatingBackend()
	a := resumeAgent(t, b, "sleeper")
	before := b.renews
	l := a.lease
	l.mu.Lock()
	l.grantedWall = l.grantedWall.Add(-l.ttl / 2)
	l.mu.Unlock()
	record(t, a, "after the suspend")
	if b.renews != before+1 {
		t.Fatalf("renews = %d, want %d: the wall clock showed the lease was due", b.renews, before+1)
	}
	if time.Since(l.granted) > time.Second {
		t.Fatal("renewal did not reset the grant time")
	}
}

func TestPositionParse(t *testing.T) {
	p, err := ParsePosition("41.7.12")
	if err != nil || p != (Position{Turns: 41, Actions: 7, State: 12}) || p.String() != "41.7.12" {
		t.Fatalf("parse = %+v, %v", p, err)
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "a.b.c", "-1.0.0"} {
		if _, err := ParsePosition(bad); err == nil {
			t.Errorf("ParsePosition(%q) succeeded", bad)
		}
	}
}
