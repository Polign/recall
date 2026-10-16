package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestUncovered(t *testing.T) {
	text := "Bluefin was acquired by Orca Labs, so I work at Orca Labs now, same role. My email is maya@orcalabs.com. Thanks!"
	got := uncovered(text, []Proposal{{Evidence: "so I work at Orca Labs now, same role"}})
	if got != "My email is maya@orcalabs.com." {
		t.Fatalf("uncovered = %q", got)
	}
	if got := uncovered(text, nil); got != "" {
		t.Errorf("nothing proposed: %q, want no second look", got)
	}
	all := []Proposal{{Evidence: "so I work at Orca Labs now"}, {Evidence: "My email is maya@orcalabs.com."}}
	if got := uncovered(text, all); got != "" {
		t.Errorf("all covered: %q", got)
	}
	transcript := "User: We moved to Portland last month.\nAssistant: You might like the vegan place on Pike Street."
	if got := uncovered(transcript, []Proposal{{Evidence: "We moved to Portland"}}); got != "" {
		t.Errorf("assistant line given a second look: %q", got)
	}
}

// passOver files only the first fact it is shown, the way a model sometimes
// passes over a clause.
type passOver struct{ calls []string }

func (m *passOver) Extract(_ context.Context, text string, _ Registry) ([]Proposal, error) {
	m.calls = append(m.calls, text)
	var out []Proposal
	if strings.Contains(text, "I work at Orca Labs") {
		out = append(out, Proposal{Subject: "user", Predicate: "works_at", Value: "Orca Labs", Evidence: "I work at Orca Labs"})
	} else if strings.Contains(text, "My email is maya@orcalabs.com") {
		out = append(out, Proposal{Subject: "user", Predicate: "email", Value: "maya@orcalabs.com", Evidence: "My email is maya@orcalabs.com"})
	}
	return out, nil
}

func TestRememberTextLooksAgainAtWhatWasPassedOver(t *testing.T) {
	c := openClient(t, newLockedBackend())
	remember(t, c, "works_at", "Bluefin", "")
	remember(t, c, "email", "maya@bluefin.io", "")
	m := &passOver{}
	r, err := c.RememberText(context.Background(), "Since the merger I work at Orca Labs now, same role. My email is maya@orcalabs.com.", m)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 || m.calls[1] != "My email is maya@orcalabs.com." {
		t.Fatalf("extractor calls %q", m.calls)
	}
	var filed []string
	for _, x := range r.Results {
		filed = append(filed, x.Stored.Predicate)
	}
	if !slices.Contains(filed, "works_at") || !slices.Contains(filed, "email") {
		t.Fatalf("filed %v, want works_at and email", filed)
	}
}
