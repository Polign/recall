package engine

import (
	"context"
	"testing"
)

// getBackend adds reads by id to lockedBackend.
type getBackend struct{ *lockedBackend }

func (b getBackend) Get(_ context.Context, _ string, ids []string) ([]StoredVector, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []StoredVector
	for _, id := range ids {
		if rec, ok := b.db.records[id]; ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

func TestRememberTextLinksEachStatementToItsText(t *testing.T) {
	c := clientFor(t, getBackend{newLockedBackend()}, EmbedFunc(testEmbed))
	text := "user: I finally switched to neovim last week.\n\nassistant: Nice choice."
	out, err := c.RememberText(t.Context(), text, ProposedStatements{
		{Subject: "user", Predicate: "prefers_editor", Value: "neovim", Evidence: "I finally switched to neovim"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Episode == nil || out.Episode.Stored.Predicate != NotePredicate || out.Episode.Stored.Value != text {
		t.Fatalf("episode = %+v", out.Episode)
	}
	stored := out.Results[0].Stored
	if stored.Evidence != "I finally switched to neovim" || stored.EvidenceID != out.Episode.Stored.EventID {
		t.Fatalf("statement evidence = %q -> %q, want the quote -> %s", stored.Evidence, stored.EvidenceID, out.Episode.Stored.EventID)
	}

	got, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(got) != 1 || got[0].EvidenceID != stored.EvidenceID || got[0].Evidence != stored.Evidence {
		t.Fatalf("recalled belief lost its evidence: %+v %v", got, err)
	}

	events, err := c.Events(t.Context(), []string{got[0].EvidenceID, "missing", got[0].EvidenceID})
	if err != nil || len(events) != 1 || events[0].Value != text {
		t.Fatalf("Events = %+v, %v; want the episode text once", events, err)
	}

	// Stating the same text again keeps one episode and writes nothing new.
	again, err := c.RememberText(t.Context(), text, ProposedStatements{
		{Subject: "user", Predicate: "prefers_editor", Value: "neovim", Evidence: "I finally switched to neovim"},
	})
	if err != nil || !again.Episode.Existing || again.Episode.Stored.EventID != out.Episode.Stored.EventID || !again.Results[0].Existing {
		t.Fatalf("restated text = %+v, %v", again, err)
	}
}

func TestStatementsAreSearchableByTheirEvidence(t *testing.T) {
	c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
	res, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "likes", Value: "sourdough",
		Evidence: "an impulse buy at my favorite bakery", EvidenceID: "episode-1"})
	if err != nil {
		t.Fatal(err)
	}
	b := c.backend.(*lockedBackend)
	md := b.db.records[res.Stored.EventID].Metadata
	if md[TextField] != "user likes sourdough\nan impulse buy at my favorite bakery" || md["evidence_id"] != "episode-1" {
		t.Fatalf("metadata = %v", md)
	}
}

func TestEvidenceIsValidated(t *testing.T) {
	c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
	long := make([]byte, MaxEvidenceBytes+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "likes", Value: "go", Evidence: string(long)}); err == nil {
		t.Fatal("oversized evidence accepted")
	}
	if _, err := c.RememberText(t.Context(), string(long), ProposedStatements{{Subject: "user", Predicate: "likes", Value: "x", Evidence: string(long)}}); err == nil {
		t.Fatal("oversized proposal evidence accepted")
	}
	if _, err := c.Events(t.Context(), []string{"a"}); err == nil {
		t.Fatal("Events on a backend without Get must say so")
	}
	for _, bad := range []map[string]any{{"evidence": ""}, {"evidence_id": 7}} {
		md := Event{ID: "e", Kind: "fact", Subject: "user", Predicate: "likes", Value: "go", Confidence: 1, Source: "user_stated", ObservedAt: t0}.Metadata()
		for k, v := range bad {
			md[k] = v
		}
		if _, err := DecodeEvent("e", md); err == nil {
			t.Fatalf("decoded malformed evidence %v", bad)
		}
	}
}
