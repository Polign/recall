package engine

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A question like "what did I plant two weeks ago" names when as well as
// what. Lexical and vector search only see the words, so the session from
// two weeks ago ranks no higher than any other that mentions planting.
// queryWindow reads the time phrase and returns the span of observed times
// it points at, relative to the instant the question is asked, so recall can
// search that span first. The windows are deliberately loose: people say
// "two weeks ago" about anything from ten to eighteen days back.

// timeHint is a time phrase found in search text.
type timeHint struct {
	from, to time.Time
	// rest is the search text with the phrase taken out, so "weeks" and
	// "ago" do not steer the word search.
	rest string
}

var numberWords = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	"couple": 2, "a couple": 2, "a couple of": 2, "couple of": 2, "few": 3, "a few": 3, "several": 3,
}

const numberPattern = `(\d{1,2}|a couple of|a couple|couple of|a few|several|few|couple|an|a|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)`

var (
	reAgo      = regexp.MustCompile(`\b` + numberPattern + `\s+(day|week|month)s?\s+ago\b`)
	rePast     = regexp.MustCompile(`\b(?:in\s+)?(?:the\s+)?(?:past|last|previous)\s+(?:` + numberPattern + `\s+)?(day|week|month)s?\b`)
	reWeekend  = regexp.MustCompile(`\b(?:last|this past|the past|past|previous)\s+weekend\b`)
	reWeekday  = regexp.MustCompile(`\b(?:last|this past|on|the past|past|previous)\s+(sunday|monday|tuesday|wednesday|thursday|friday|saturday)\b`)
	reMonth    = regexp.MustCompile(`\b(?:in|during|of|back in|since)\s+(january|february|march|april|may|june|july|august|september|october|november|december)\b`)
	reYest     = regexp.MustCompile(`\byesterday\b`)
	reToday    = regexp.MustCompile(`\b(?:today|this morning|earlier today)\b`)
	reThisWeek = regexp.MustCompile(`\bthis\s+(week|month)\b`)
)

var monthNames = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March, "april": time.April,
	"may": time.May, "june": time.June, "july": time.July, "august": time.August,
	"september": time.September, "october": time.October, "november": time.November, "december": time.December,
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

func countOf(s string) int {
	if s == "" {
		return 1
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return numberWords[s]
}

// queryWindow returns the time hint in text, read relative to now, or false
// when the text names no time it understands. The window never reaches past
// now: a question cannot be about what has not been said yet.
func queryWindow(text string, now time.Time) (timeHint, bool) {
	lower := strings.ToLower(text)
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := func(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

	var from, to time.Time
	var loc []int
	switch {
	case reAgo.MatchString(lower):
		m := reAgo.FindStringSubmatch(lower)
		loc = reAgo.FindStringIndex(lower)
		n := countOf(m[1])
		switch m[2] {
		case "day":
			slack := max(1, n/4)
			from, to = day.Add(-days(n+slack)), day.Add(-days(n-slack)+days(1))
		case "week":
			center := day.Add(-days(7 * n))
			from, to = center.Add(-days(4+n)), center.Add(days(4+n))
		case "month":
			center := day.AddDate(0, -n, 0)
			from, to = center.Add(-days(10+3*n)), center.Add(days(10+3*n))
		}
	case reWeekend.MatchString(lower):
		loc = reWeekend.FindStringIndex(lower)
		// The most recent Saturday that is over, with its Sunday; a day of
		// slack each side for "Friday night" and "Monday morning" mentions.
		back := (int(day.Weekday()) - int(time.Saturday) + 7) % 7
		if back == 0 || back == 1 {
			back += 7 // on a weekend, "last weekend" is the one before
		}
		sat := day.Add(-days(back))
		from, to = sat.Add(-days(1)), sat.Add(days(3))
	case reWeekday.MatchString(lower):
		m := reWeekday.FindStringSubmatch(lower)
		loc = reWeekday.FindStringIndex(lower)
		back := (int(day.Weekday()) - int(weekdays[m[1]]) + 7) % 7
		if back == 0 {
			back = 7
		}
		d := day.Add(-days(back))
		from, to = d.Add(-days(1)), d.Add(days(2))
	case rePast.MatchString(lower):
		m := rePast.FindStringSubmatch(lower)
		loc = rePast.FindStringIndex(lower)
		n := countOf(m[1])
		switch m[2] {
		case "day":
			from = day.Add(-days(n + 1))
		case "week":
			from = day.Add(-days(7*n + 3))
		case "month":
			from = day.AddDate(0, -n, -5)
		}
		to = now
	case reMonth.MatchString(lower):
		m := reMonth.FindStringSubmatch(lower)
		loc = reMonth.FindStringIndex(lower)
		month := monthNames[m[1]]
		year := now.Year()
		if month > now.Month() {
			year-- // "in December" asked in March means last December
		}
		start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		from, to = start.Add(-days(2)), start.AddDate(0, 1, 2)
	case reYest.MatchString(lower):
		loc = reYest.FindStringIndex(lower)
		from, to = day.Add(-days(2)), day
	case reThisWeek.MatchString(lower):
		m := reThisWeek.FindStringSubmatch(lower)
		loc = reThisWeek.FindStringIndex(lower)
		if m[1] == "week" {
			from = day.Add(-days(int(day.Weekday()) + 1))
		} else {
			from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-days(1))
		}
		to = now
	case reToday.MatchString(lower):
		loc = reToday.FindStringIndex(lower)
		from, to = day, now
	default:
		return timeHint{}, false
	}
	if to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return timeHint{}, false
	}
	src := text
	if len(lower) != len(text) {
		src = lower // lowering changed byte offsets; the lowered text is as good for search
	}
	rest := strings.Join(strings.Fields(src[:loc[0]]+" "+src[loc[1]:]), " ")
	return timeHint{from: from, to: to, rest: rest}, true
}

// withObserved narrows a search filter to events observed in [from, to].
// Either bound may be zero. observed_ms is written on every event.
func withObserved(filter map[string]any, from, to time.Time) map[string]any {
	if from.IsZero() && to.IsZero() {
		return filter
	}
	out := make(map[string]any, len(filter)+1)
	for k, v := range filter {
		out[k] = v
	}
	r := map[string]any{}
	if !from.IsZero() {
		r["$gte"] = float64(from.UTC().UnixMilli())
	}
	if !to.IsZero() {
		r["$lte"] = float64(to.UTC().UnixMilli())
	}
	out["observed_ms"] = r
	return out
}
