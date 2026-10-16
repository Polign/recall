package engine

import (
	"regexp"
	"strings"
)

// sentenceEnd splits text after a sentence's closing punctuation or at a
// line break.
var sentenceEnd = regexp.MustCompile(`[.!?]+(\s+|$)|\n+`)

// minUncoveredWords is how long a sentence must be to be worth a second
// look: "Thanks!" or "Hi, I'm back." hold nothing to remember.
const minUncoveredWords = 4

// uncovered returns the sentences of text that no proposal quotes, joined
// with spaces, or "" when every sentence worth a look is covered or the
// extractor found nothing at all (then the text is taken to hold nothing).
// An assistant's lines in a transcript are left out: by design they are not
// recorded about the user.
func uncovered(text string, proposals []Proposal) string {
	if len(proposals) == 0 {
		return ""
	}
	type span struct{ from, to int }
	var quoted []span
	for _, p := range proposals {
		ev := strings.TrimSpace(p.Evidence)
		if ev == "" {
			continue
		}
		if i := strings.Index(text, ev); i >= 0 {
			quoted = append(quoted, span{i, i + len(ev)})
		}
	}
	var missed []string
	start := 0
	cut := func(end int) {
		sentence := strings.TrimSpace(text[start:end])
		from := start
		start = end
		if len(strings.Fields(sentence)) < minUncoveredWords || strings.HasPrefix(sentence, "Assistant:") {
			return
		}
		for _, q := range quoted {
			if q.from < end && q.to > from {
				return
			}
		}
		missed = append(missed, sentence)
	}
	for _, m := range sentenceEnd.FindAllStringIndex(text, -1) {
		cut(m[1])
	}
	if start < len(text) {
		cut(len(text))
	}
	return strings.Join(missed, " ")
}

// newProposals keeps the second look's proposals that quote the whole text
// and say something the first look did not.
func newProposals(text string, have, more []Proposal) []Proposal {
	key := func(p Proposal) string {
		return normalizeSubject(p.Subject) + "\x00" + strings.TrimSpace(p.Predicate) + "\x00" + ValueKey(p.Value)
	}
	seen := map[string]bool{}
	for _, p := range have {
		seen[key(p)] = true
	}
	var out []Proposal
	for _, p := range more {
		ev := strings.TrimSpace(p.Evidence)
		if ev == "" || !strings.Contains(text, ev) || seen[key(p)] {
			continue
		}
		seen[key(p)] = true
		out = append(out, p)
	}
	return out
}
