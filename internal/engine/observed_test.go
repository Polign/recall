package engine

import (
	"strings"
	"testing"
	"time"
)

func rememberObserved(t *testing.T, c *Client, predicate string, value any, at time.Time) RememberResult {
	t.Helper()
	res, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: predicate, Value: value, ObservedAt: at})
	if err != nil {
		t.Fatalf("Remember(%s, %v at %s): %v", predicate, value, at, err)
	}
	return res
}

func currentValues(t *testing.T, c *Client, predicate string, asOf time.Time) []any {
	t.Helper()
	got, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: predicate, AsOf: asOf})
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, b := range got {
		out = append(out, b.Value)
	}
	return out
}

// An imported conversation says "vim" in March and "emacs" in May. Writing
// them in either order must leave emacs current and vim true for April.
func TestObservedAtOrdersImportedStatements(t *testing.T) {
	march := time.Date(2023, 3, 1, 12, 0, 0, 0, time.UTC)
	april := march.AddDate(0, 1, 0)
	may := march.AddDate(0, 2, 0)
	for _, order := range [][]string{{"vim", "emacs"}, {"emacs", "vim"}} {
		c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
		for _, v := range order {
			at := march
			if v == "emacs" {
				at = may
			}
			res := rememberObserved(t, c, "prefers_editor", v, at)
			if !res.Stored.ObservedAt.Equal(at) {
				t.Fatalf("stored at %s, want %s", res.Stored.ObservedAt, at)
			}
		}
		if got := currentValues(t, c, "prefers_editor", time.Time{}); len(got) != 1 || got[0] != "emacs" {
			t.Fatalf("order %v: now = %v, want emacs", order, got)
		}
		if got := currentValues(t, c, "prefers_editor", april); len(got) != 1 || got[0] != "vim" {
			t.Fatalf("order %v: as of April = %v, want vim", order, got)
		}
		history, err := c.History(t.Context(), "user", "prefers_editor")
		if err != nil || len(history) != 2 {
			t.Fatalf("history = %v, %v", history, err)
		}
	}
}

// A backdated statement is judged as of its own time: restating in March
// what was already believed in March writes nothing.
func TestObservedAtFoldsAtTheObservation(t *testing.T) {
	c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
	march := time.Date(2023, 3, 1, 12, 0, 0, 0, time.UTC)
	rememberObserved(t, c, "prefers_editor", "vim", march)
	if res := rememberObserved(t, c, "prefers_editor", "vim", march.Add(time.Hour)); !res.Existing {
		t.Fatalf("restating vim an hour later wrote a new event: %+v", res)
	}
}

func TestObservedAtRefusesTheFuture(t *testing.T) {
	c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
	_, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim", ObservedAt: time.Now().Add(time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("a statement an hour ahead was accepted: %v", err)
	}
	// A writer whose clock runs a little ahead is within the skew allowance.
	rememberObserved(t, c, "prefers_editor", "vim", time.Now().Add(MaxObservationSkew/2))
}

func TestRememberTextAtStampsEveryStatement(t *testing.T) {
	c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
	at := time.Date(2023, 5, 20, 2, 21, 0, 0, time.UTC)
	text := "I switched to neovim. I also like hiking."
	res, err := c.RememberTextAt(t.Context(), text, ProposedStatements{
		{Subject: "user", Predicate: "prefers_editor", Value: "neovim", Evidence: "I switched to neovim."},
		{Subject: "user", Predicate: "hobby", Value: "hiking", Evidence: "I also like hiking."},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the typed statement and a note for the unfiled one", res.Results)
	}
	for _, r := range res.Results {
		if !r.Stored.ObservedAt.Equal(at) {
			t.Fatalf("%s stored at %s, want %s", r.Stored.Predicate, r.Stored.ObservedAt, at)
		}
	}
}
