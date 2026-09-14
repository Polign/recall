package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestObservationTimeOverridesWriteAcceptanceOrder(t *testing.T) {
	s, _, clock := newStore(t)
	*clock = at(2 * time.Hour)
	mustRemember(t, s, "prefers_editor", "neovim")
	*clock = at(time.Hour) // a later accepted write from a clock running behind
	r := mustRemember(t, s, "prefers_editor", "vim")
	if r.Stored.Value != "vim" || r.Existing {
		t.Fatalf("write result = %+v", r)
	}
	for _, tc := range []struct {
		when time.Time
		want string
	}{{at(time.Hour), "vim"}, {at(3 * time.Hour), "neovim"}} {
		b, err := s.ExportAudit(AuditRequest{AsOf: tc.when})
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Replay()
		if err != nil || len(got) != 1 || got[0].Value != tc.want {
			t.Fatalf("replay at %s = %+v, %v", tc.when, got, err)
		}
	}
}

func TestConcurrentRememberAndTargetedForgetReplay(t *testing.T) {
	ctx := t.Context()
	db := newLockedBackend()
	seed := clientFor(t, db, EmbedFunc(testEmbed))
	initial, err := seed.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim"})
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	backend := backendFuncs{
		put: db.Put,
		list: func(ctx context.Context, c string, f map[string]any, n int) ([]StoredVector, int, error) {
			rows, total, err := db.List(ctx, c, f, n)
			if calls.Add(1) <= 2 {
				arrived <- struct{}{}
				<-release
			}
			return rows, total, err
		},
	}
	writer := func(offset time.Duration) *Store {
		s := NewStore(requestBackend{ctx: ctx, backend: backend}, "memories", testRegistry(t), func(string) []float32 { return []float32{1, 0, 0} })
		s.now = func() time.Time { return initial.Stored.ObservedAt.Add(offset) }
		return s
	}
	remember, forget := writer(time.Second), writer(2*time.Second)
	done := make(chan error, 2)
	go func() {
		_, err := remember.Remember("fact", "user", "prefers_editor", "emacs", 1, "user_stated")
		done <- err
	}()
	go func() { _, err := forget.Forget("user", "prefers_editor", "vim"); done <- err }()
	<-arrived
	<-arrived // both have read the same pre-write history
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b, err := seed.ExportAudit(ctx, AuditRequest{AsOf: initial.Stored.ObservedAt.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Replay()
	if err != nil || len(b.Events) != 3 || len(got) != 1 || got[0].Value != "emacs" {
		t.Fatalf("concurrent replay = %+v, %v (%d events)", got, err, len(b.Events))
	}
}

type lostAcknowledgementDB struct {
	*fakeDB
	nextError error
}

func (d *lostAcknowledgementDB) Put(c, id string, v []float32, md map[string]any) error {
	if err := d.fakeDB.Put(c, id, v, md); err != nil {
		return err
	}
	err := d.nextError
	d.nextError = nil
	return err
}

func TestLostAcknowledgementRetryIsStateBasedNotExactlyOnce(t *testing.T) {
	s, db, clock := newStore(t)
	lost := errors.New("write committed but acknowledgement lost")
	s.db = &lostAcknowledgementDB{fakeDB: db, nextError: lost}
	if _, err := s.Remember("fact", "user", "prefers_editor", "vim", 1, "user_stated"); !errors.Is(err, lost) {
		t.Fatalf("lost acknowledgement = %v", err)
	}
	if len(db.records) != 1 {
		t.Fatal("fixture did not commit before returning error")
	}
	*clock = at(time.Second)
	if r := mustRemember(t, s, "prefers_editor", "vim"); !r.Existing || db.puts != 1 {
		t.Fatalf("unchanged-state retry = %+v, puts %d", r, db.puts)
	}
	*clock = at(2 * time.Second)
	mustRemember(t, s, "prefers_editor", "emacs")
	*clock = at(3 * time.Second)
	if r := mustRemember(t, s, "prefers_editor", "vim"); r.Existing || db.puts != 3 {
		t.Fatalf("retry after correction = %+v, puts %d", r, db.puts)
	}
	b, err := s.ExportAudit(AuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Replay()
	if err != nil || len(b.Events) != 3 || len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("retry replay = %+v, %v", got, err)
	}
}

// A peer whose clock runs ahead, or a local clock stepped backwards, leaves
// events in the log that the writer's own clock has not reached. Forget used to
// fold at that clock, see nothing held, and return success having written
// nothing, so the belief reappeared as soon as time caught up.
func TestForgetWithdrawsAnEventLaterThanTheWritersClock(t *testing.T) {
	s, _, clock := newStore(t)
	*clock = at(2 * time.Hour)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = at(time.Hour) // the writer's clock now trails the log

	withdrawn, err := s.Forget("user", "prefers_editor", "")
	if err != nil || withdrawn != 1 {
		t.Fatalf("Forget = %d, %v; want 1 withdrawn", withdrawn, err)
	}
	// The retraction is stamped just after the assertion it withdraws, so the
	// belief still stands at that exact instant and is gone from there on.
	got, err := s.foldPair(pair{"user", "prefers_editor"}, at(2*time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("assertion missing at its own instant: %v %v", got, err)
	}
	if got, err := s.foldPair(pair{"user", "prefers_editor"}, at(3*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("belief survived its retraction: %v %v", got, err)
	}
}

// A stopped or coarse clock must not let two statements share an instant. Ties
// are ordered by id hash, which is deterministic but uncorrelated with the
// order the statements were made, so a correction could silently invert.
func TestCorrectionSurvivesAStoppedClock(t *testing.T) {
	s, _, clock := newStore(t)
	*clock = at(time.Hour)
	first := mustRemember(t, s, "prefers_editor", "vim")
	second := mustRemember(t, s, "prefers_editor", "neovim")
	if !first.Stored.ObservedAt.Before(second.Stored.ObservedAt) {
		t.Fatalf("two statements share an instant: %s and %s", first.Stored.ObservedAt, second.Stored.ObservedAt)
	}
	got, err := s.foldPair(pair{"user", "prefers_editor"}, at(2*time.Hour))
	if err != nil || len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("correction inverted under a stopped clock: %v %v", got, err)
	}
}
