package engine

import (
	"testing"
	"time"
)

// asked is a Tuesday afternoon.
var asked = time.Date(2023, 5, 30, 15, 0, 0, 0, time.UTC)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

func TestQueryWindowReadsTimePhrases(t *testing.T) {
	cases := []struct {
		text   string
		inside []time.Time
		out    []time.Time
		rest   string
	}{
		{"What gardening activity did I do two weeks ago?", []time.Time{day(2023, 5, 16), day(2023, 5, 12), day(2023, 5, 20)},
			[]time.Time{day(2023, 5, 29), day(2023, 5, 1)}, "What gardening activity did I do ?"},
		{"Which book did I finish a week ago?", []time.Time{day(2023, 5, 23)}, []time.Time{day(2023, 5, 10)}, "Which book did I finish ?"},
		{"I received a piece of jewelry last Saturday from whom?", []time.Time{day(2023, 5, 27)},
			[]time.Time{day(2023, 5, 20), day(2023, 5, 30)}, "I received a piece of jewelry from whom?"},
		{"Which bike did I fix the past weekend?", []time.Time{day(2023, 5, 27), day(2023, 5, 28)}, []time.Time{day(2023, 5, 21)}, ""},
		{"What sports events did I go to in the past month?", []time.Time{day(2023, 5, 2), day(2023, 5, 29)}, []time.Time{day(2023, 4, 10)}, ""},
		{"concerts in the past two months", []time.Time{day(2023, 4, 2)}, []time.Time{day(2023, 3, 1)}, ""},
		{"What did I watch in January?", []time.Time{day(2023, 1, 15)}, []time.Time{day(2023, 3, 1), day(2022, 1, 15)}, ""},
		{"What did I buy in December?", []time.Time{day(2022, 12, 10)}, []time.Time{day(2023, 5, 10)}, ""},
		{"three days ago I called someone", []time.Time{day(2023, 5, 27)}, []time.Time{day(2023, 5, 20)}, ""},
		{"what did I eat yesterday", []time.Time{day(2023, 5, 29)}, []time.Time{day(2023, 5, 25)}, ""},
		{"a few weeks ago I started a course", []time.Time{day(2023, 5, 9)}, []time.Time{day(2023, 4, 1)}, ""},
		{"two months ago I moved", []time.Time{day(2023, 3, 30), day(2023, 3, 20)}, []time.Time{day(2023, 5, 25)}, ""},
	}
	for _, c := range cases {
		h, ok := queryWindow(c.text, asked)
		if !ok {
			t.Errorf("%q: no window", c.text)
			continue
		}
		for _, d := range c.inside {
			if d.Before(h.from) || d.After(h.to) {
				t.Errorf("%q: %s outside [%s, %s]", c.text, d.Format(time.DateOnly), h.from.Format(time.DateOnly), h.to.Format(time.DateOnly))
			}
		}
		for _, d := range c.out {
			if !d.Before(h.from) && !d.After(h.to) {
				t.Errorf("%q: %s inside [%s, %s]", c.text, d.Format(time.DateOnly), h.from.Format(time.DateOnly), h.to.Format(time.DateOnly))
			}
		}
		if h.to.After(asked) {
			t.Errorf("%q: window ends after the question", c.text)
		}
		if c.rest != "" && h.rest != c.rest {
			t.Errorf("%q: rest = %q, want %q", c.text, h.rest, c.rest)
		}
	}
}

func TestQueryWindowIgnoresTextWithoutATime(t *testing.T) {
	for _, text := range []string{
		"Which editor does the user prefer?",
		"How many days did it take me to finish The Nightingale?",
		"What is my last name?",
		"Did I say anything about May?",
	} {
		if h, ok := queryWindow(text, asked); ok {
			t.Errorf("%q: window [%s, %s]", text, h.from, h.to)
		}
	}
}

// A question that names when puts what was said then ahead of the same
// words said at any other time.
func TestTimeHintRanksTheNamedSpanFirst(t *testing.T) {
	s, _, clock := newStore(t)
	*clock = asked.AddDate(0, 0, -40)
	mustRemember(t, s, "likes", "planting basil")
	*clock = asked.AddDate(0, 0, -14)
	mustRemember(t, s, "likes", "planting tomato saplings")
	*clock = asked.AddDate(0, 0, -2)
	mustRemember(t, s, "likes", "planting peppers")

	got := recallValues(t, s, Query{Text: "what did I plant two weeks ago", AsOf: asked, Limit: 3})
	if len(got) != 3 || got[0] != "planting tomato saplings" {
		t.Fatalf("got %v, want the two-weeks-ago value first and nothing dropped", got)
	}

	got = recallValues(t, s, Query{Text: "what did I plant two weeks ago", AsOf: asked, Limit: 3, NoTimeHint: true})
	if len(got) != 3 || got[0] != "planting basil" {
		t.Fatalf("NoTimeHint: got %v, want search order", got)
	}
}

func TestObservedWindowFiltersBeliefs(t *testing.T) {
	s, _, clock := newStore(t)
	*clock = asked.AddDate(0, 0, -40)
	mustRemember(t, s, "likes", "basil")
	*clock = asked.AddDate(0, 0, -14)
	mustRemember(t, s, "likes", "tomatoes")

	got := recallValues(t, s, Query{Text: "likes", AsOf: asked, ObservedAfter: asked.AddDate(0, 0, -20)})
	if len(got) != 1 || got[0] != "tomatoes" {
		t.Fatalf("got %v, want only tomatoes", got)
	}
}
