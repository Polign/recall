package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fencingBackend is the leasing fake with the server's write fence: a write
// lands only while the lease's newest epoch is still the writer's holder and
// not older than the epoch it presents.
type fencingBackend struct {
	*leasingBackend
	refused int
}

func (b *fencingBackend) PutFenced(ctx context.Context, c, id string, v []float32, md map[string]any, f Fence) error {
	b.lmu.Lock()
	l := b.leases[c+"/"+f.Lease]
	ok := l != nil && l.holder == f.Holder && !l.expires.IsZero() && l.epoch >= f.Epoch
	if !ok {
		b.refused++
	}
	b.lmu.Unlock()
	if !ok {
		return fmt.Errorf("%w: write fenced", ErrLeaseLost)
	}
	return b.Put(ctx, c, id, v, md)
}

// crash leaves the agent as a killed process would: lease not released, and
// takeable at once as if its TTL had run out.
func crash(b *leasingBackend, a *Agent) {
	a.lease.stopKeepAlive()
	b.expire(a.collection, a.id)
}

func TestCrashBetweenBeginAndEndLeavesTheActionOpen(t *testing.T) {
	ctx := t.Context()
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "mailer"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := a.BeginAction(ctx, "list_inbox", ActionPure, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EndAction(ctx, read.Seq, ActionEnd{Status: ActionDone, Result: "3 unread"}); err != nil {
		t.Fatal(err)
	}
	send, err := a.BeginAction(ctx, "send_email", ActionExternal, `{"to":"maria@example.com","subject":"Your car is ready"}`)
	if err != nil {
		t.Fatal(err)
	}
	if send.Seq != 2 || send.Key == "" || send.Status != ActionStarted {
		t.Fatalf("begin = %+v", send)
	}
	crash(b, a) // the mail may or may not have gone out

	next, err := c.Resume(ctx, ResumeRequest{AgentID: "mailer"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Release(ctx) }()
	rc := next.Resumed()
	if rc.Fresh || len(rc.PendingActions) != 1 || rc.PendingActions[0].Seq != 2 || rc.PendingActions[0].Key != send.Key {
		t.Fatalf("pending = %+v", rc.PendingActions)
	}
	for _, want := range []string{"## Unfinished actions", "send_email(", "Your car is ready", "[external, key " + send.Key, "never repeat it blindly"} {
		if !strings.Contains(rc.Briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, rc.Briefing)
		}
	}
	// The unfinished action comes before the working state and the turns.
	if strings.Index(rc.Briefing, "## Unfinished actions") > strings.Index(rc.Briefing+"## Working state", "## Working state") {
		t.Errorf("unfinished actions are not first:\n%s", rc.Briefing)
	}
	// New actions continue the sequence instead of reusing the open one's.
	more, err := next.BeginAction(ctx, "list_inbox", ActionPure, `{}`)
	if err != nil || more.Seq != 3 || more.Key == send.Key {
		t.Fatalf("next action = %+v, %v", more, err)
	}
}

func TestReconcileSettlesEachClassItsOwnWay(t *testing.T) {
	ctx := t.Context()
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	pure, _ := a.BeginAction(ctx, "search_parts", ActionPure, `{"q":"brake pads"}`)
	put, _ := a.BeginAction(ctx, "save_quote", ActionIdempotent, `{"id":"q-17","total":410}`)
	mail, _ := a.BeginAction(ctx, "send_email", ActionExternal, `{"subject":"Quote q-17"}`)
	sms, _ := a.BeginAction(ctx, "send_sms", ActionExternal, `{"body":"Quote ready"}`)
	crash(b, a)

	next, err := c.Resume(ctx, ResumeRequest{AgentID: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Release(ctx) }()
	var asked []string
	rep, err := next.Reconcile(ctx, func(_ context.Context, act Action) (ActionEnd, error) {
		asked = append(asked, act.Tool)
		switch act.Tool {
		case "save_quote":
			if act.Key != put.Key {
				t.Errorf("rerun key %q, want the original %q", act.Key, put.Key)
			}
			return ActionEnd{Status: ActionDone, Result: "saved", Resolution: "re-run with the same key"}, nil
		case "send_email":
			// The mail log shows it went out before the crash.
			return ActionEnd{Status: ActionDone, Resolution: "found in the sent log"}, nil
		default:
			return ActionEnd{Status: ActionUnknown}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "save_quote,send_email,send_sms" {
		t.Fatalf("resolver asked about %v; pure actions are never sent to it", asked)
	}
	if len(rep.Closed) != 3 || len(rep.Open) != 1 || rep.Open[0].Seq != sms.Seq || rep.Open[0].Status != ActionUnknown {
		t.Fatalf("report = %+v", rep)
	}
	byTool := map[string]Action{}
	for _, act := range rep.Closed {
		byTool[act.Tool] = act
	}
	if byTool["search_parts"].Status != ActionAbandoned || byTool["save_quote"].Status != ActionDone || byTool["send_email"].Resolution != "found in the sent log" {
		t.Fatalf("closed = %+v", rep.Closed)
	}
	_ = pure
	_ = mail

	// What was settled is in the history, so the model sees the email went
	// out instead of seeing its own call with no result.
	rc := next.Resumed()
	for _, want := range []string{"[tool send_email", "settled after a crash: done; found in the sent log", "[tool save_quote", "saved"} {
		if !strings.Contains(rc.Briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, rc.Briefing)
		}
	}
	// The briefing is rebuilt: only the unconfirmed SMS is left to settle.
	if len(rc.PendingActions) != 1 || !strings.Contains(rc.Briefing, "send_sms(") || strings.Contains(rc.Briefing, "send_email(") {
		t.Fatalf("after reconcile: pending %+v\n%s", rc.PendingActions, rc.Briefing)
	}
	if !strings.Contains(rc.Briefing, "Not known whether this happened") {
		t.Errorf("unknown action not flagged:\n%s", rc.Briefing)
	}
	// Reconciling again changes nothing, and the action stays open until
	// someone who knows ends it.
	rep, err = next.Reconcile(ctx, nil)
	if err != nil || len(rep.Closed) != 0 || len(rep.Open) != 1 {
		t.Fatalf("second reconcile = %+v, %v", rep, err)
	}
	if _, err := next.EndAction(ctx, sms.Seq, ActionEnd{Status: ActionFailed, Resolution: "carrier shows no message; the agent will send it again"}); err != nil {
		t.Fatal(err)
	}
	if _, err := next.EndAction(ctx, sms.Seq, ActionEnd{Status: ActionDone}); err == nil {
		t.Fatal("ending a closed action twice succeeded")
	}
	if pending, err := next.PendingActions(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after ending = %+v, %v", pending, err)
	}
}

func TestReconcileWithoutAResolver(t *testing.T) {
	ctx := t.Context()
	b := newLeasingBackend()
	c := newAgentClient(t, b)
	a, _ := c.Resume(ctx, ResumeRequest{AgentID: "x"})
	_, _ = a.BeginAction(ctx, "read_file", ActionPure, `{"path":"a.py"}`)
	put, _ := a.BeginAction(ctx, "write_file", ActionIdempotent, `{"path":"a.py"}`)
	mail, _ := a.BeginAction(ctx, "send_email", ActionExternal, `{}`)
	crash(b, a)

	next, err := c.Resume(ctx, ResumeRequest{AgentID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Release(ctx) }()
	rep, err := next.Reconcile(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is run again by Recall: the idempotent write waits for the
	// harness, and the email is marked unknown rather than assumed.
	if len(rep.Closed) != 1 || rep.Closed[0].Status != ActionAbandoned || len(rep.Open) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Open[0].Seq != put.Seq || rep.Open[0].Status != ActionStarted || rep.Open[1].Seq != mail.Seq || rep.Open[1].Status != ActionUnknown {
		t.Fatalf("open = %+v", rep.Open)
	}
	if !strings.Contains(next.Resumed().Briefing, "Run it again with the same key") {
		t.Errorf("briefing:\n%s", next.Resumed().Briefing)
	}
}

func TestActionValidationAndLargeArgs(t *testing.T) {
	ctx := t.Context()
	c := newAgentClient(t, newLeasingBackend())
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "v", OutputThreshold: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release(ctx) }()
	if _, err := a.BeginAction(ctx, "send", "", "{}"); err == nil {
		t.Fatal("an action with no class was accepted")
	}
	if _, err := a.BeginAction(ctx, "", ActionPure, "{}"); err == nil {
		t.Fatal("an action with no tool was accepted")
	}
	if _, err := a.EndAction(ctx, 99, ActionEnd{Status: ActionDone}); err == nil {
		t.Fatal("ending a missing action succeeded")
	}
	big := strings.Repeat(`{"row":1},`, 200)
	act, err := a.BeginAction(ctx, "bulk_upsert", ActionIdempotent, big)
	if err != nil {
		t.Fatal(err)
	}
	if act.ArgsRef == "" || len(act.Args) >= len(big) {
		t.Fatalf("args not spilled: ref %q, %d bytes", act.ArgsRef, len(act.Args))
	}
	if out, err := a.FetchOutput(ctx, act.ArgsRef); err != nil || out.Content != big {
		t.Fatalf("spilled args = %d bytes, %v", len(out.Content), err)
	}
	if _, err := a.EndAction(ctx, act.Seq, ActionEnd{Status: "maybe"}); err == nil {
		t.Fatal("an invalid end status was accepted")
	}
}

func TestServerFenceRefusesAStalledHoldersWrite(t *testing.T) {
	ctx := t.Context()
	b := &fencingBackend{leasingBackend: newLeasingBackend()}
	c := newAgentClient(t, b)
	a, err := c.Resume(ctx, ResumeRequest{AgentID: "a", LeaseTTL: 30 * time.Second, NoKeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.BeginAction(ctx, "send_email", ActionExternal, `{}`); err != nil {
		t.Fatalf("holder write: %v", err)
	}
	// The process stalls just after its client-side check passed, long
	// enough for its lease to lapse and a rival to take over. Its own
	// clock still says the lease is fresh, so only the server can stop it.
	b.expire("memory_agents", "a")
	rival, err := c.Resume(ctx, ResumeRequest{AgentID: "a", NoKeepAlive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rival.Release(ctx) }()
	if _, err := a.RecordTurn(ctx, Turn{Role: "assistant", Content: "zombie"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("zombie write err = %v, want ErrLeaseLost", err)
	}
	refused := b.refused
	// Once fenced, the process knows it lost and stops asking.
	if _, err := a.BeginAction(ctx, "send_email", ActionExternal, `{}`); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("second zombie write err = %v", err)
	}
	if b.refused != refused {
		t.Fatalf("a known-lost holder still sent a write to the server")
	}
	if turns, err := rival.RecentTurns(ctx, 10); err != nil || len(turns) != 0 {
		t.Fatalf("zombie turn landed: %+v, %v", turns, err)
	}
	// The rival writes normally and sees the zombie's earlier action open.
	if len(rival.Resumed().PendingActions) != 1 {
		t.Fatalf("rival pending = %+v", rival.Resumed().PendingActions)
	}
	if _, err := rival.RecordTurn(ctx, Turn{Role: "assistant", Content: "took over"}); err != nil {
		t.Fatal(err)
	}
}

// stopKeepAlive ends the background renewal without releasing, as a killed
// process's renewal ends.
func (l *agentLease) stopKeepAlive() {
	if l.stop != nil {
		close(l.stop)
		<-l.done
		l.stop = nil
	}
}
