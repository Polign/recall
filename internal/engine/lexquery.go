package engine

import (
	"regexp"
	"strings"
	"unicode"
)

// lexicalQuery prepares a question for word search: the hashed-word vectors
// and, where the database has one, its keyword index. Both index the words
// of each statement ("user lives in Portland"), so a question matches by
// the words it shares with them:
//
//   - question and filler words ("where", "does", "now") are dropped, since
//     they match nothing useful and dilute the words that do;
//   - each remaining word also searches its common English inflections, so
//     "live" finds "lives" and "eats" finds "eat";
//   - the user's own name is dropped: statements about the user say "user",
//     and the name would otherwise rank every statement that happens to
//     contain it, such as an email address, above the one asked about.
//
// It is used for both the vector and the keyword search. Only the query
// changes; stored vectors and the keyword index are as before, so no
// collection needs rebuilding. A question that would be left with no words
// is searched as asked.
func lexicalQuery(question string, names []string) string {
	drop := map[string]bool{}
	for _, n := range names {
		for _, w := range words(n) {
			drop[w] = true
		}
	}
	var out []string
	for _, w := range words(question) {
		if stopWords[w] || drop[w] {
			continue
		}
		out = append(out, inflections(w)...)
	}
	if len(out) == 0 {
		return question
	}
	return strings.Join(out, " ")
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// inflections returns w and the forms of it a statement is likely to use.
func inflections(w string) []string {
	if len(w) < 3 {
		return []string{w}
	}
	out := []string{w}
	add := func(v string) {
		if len(v) >= 2 {
			out = append(out, v)
		}
	}
	switch {
	case strings.HasSuffix(w, "ies"):
		add(w[:len(w)-3] + "y")
	case strings.HasSuffix(w, "ing") && len(w) >= 5:
		add(w[:len(w)-3])
		add(w[:len(w)-3] + "e")
	case strings.HasSuffix(w, "ed") && len(w) > 4:
		add(w[:len(w)-2])
		add(w[:len(w)-1])
	case strings.HasSuffix(w, "es"):
		add(w[:len(w)-2])
		add(w[:len(w)-1])
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		add(w[:len(w)-1])
	case strings.HasSuffix(w, "y"):
		add(w + "s")
		add(w[:len(w)-1] + "ies")
	default:
		add(w + "s")
	}
	return out
}

// stopWords are question and function words: they appear in questions, say
// nothing about which statement is wanted, and are cheap to match by chance.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but if of to in on at by for from with about into over
		is are was were be been being am do does did doing done has have had having can could will would
		shall should may might must what which who whom whose where when why how
		i me my mine we us our you your he him his she her hers it its they them their this that these those
		there here now then today currently current still just also any some all
		anything something everything nothing anyone someone anywhere somewhere
		tell know say said please`) {
		stopWords[w] = true
	}
}

// asUser rewrites a question for a model embedder: statements about the user
// say "user", not the user's name, so "What can Maya eat?" is searched as
// "What can the user eat?". With the name left in, a model ranks every
// statement that contains it (her name, an email address with it) above
// the one asked about.
func asUser(question string, names []string) string {
	for _, name := range names {
		for _, w := range words(name) {
			if len(w) < 2 || stopWords[w] {
				continue
			}
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(w) + `\b`)
			question = re.ReplaceAllString(question, "the user")
		}
	}
	return question
}
