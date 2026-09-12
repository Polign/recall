package recall

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// timeLayout is RFC3339 with a fixed nine-digit fraction. The stdlib's
// RFC3339Nano trims trailing zeros, which makes its output variable width and
// therefore not lexically sortable: "12:00:00.5Z" sorts before "12:00:00.05Z".
// Timestamps here are compared as strings by anything reading stored metadata,
// so the width has to be fixed.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders an instant for storage, always in UTC so that two nodes
// in different zones write comparable values.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// parseTime reads a stored instant, tolerating the looser RFC3339 forms that
// a hand-written record or an older writer might carry.
func parseTime(s string) time.Time {
	for _, layout := range []string{timeLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// eventID derives an event's identity from its content and its instant.
//
// memkit derived an id from content alone, which made re-remembering a
// statement an idempotent upsert. That is the wrong identity for a log: after
// a value is retracted and later asserted again, both events must survive, and
// a content-only id would have the second overwrite the first and erase the
// retraction from history. Idempotence moves to Remember, which folds before
// it writes and returns early when the belief already holds.
func eventID(subject, predicate string, value any, retraction bool, observedAt time.Time) string {
	var b strings.Builder
	b.WriteString(subject)
	b.WriteByte(0)
	b.WriteString(predicate)
	b.WriteByte(0)
	b.WriteString(ValueKey(value))
	b.WriteByte(0)
	if retraction {
		b.WriteString("retract")
	} else {
		b.WriteString("assert")
	}
	b.WriteByte(0)
	b.WriteString(formatTime(observedAt))
	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("m-%x", h[:12])
}

// Text is the natural-language rendering an event is embedded from, so that
// semantic recall searches the same statements exact recall filters.
func (e Event) Text() string {
	if e.Retraction && e.Value == nil {
		return e.Subject + " no longer has any " + strings.ReplaceAll(e.Predicate, "_", " ")
	}
	stem := e.Subject + " " + strings.ReplaceAll(e.Predicate, "_", " ") + " " + fmt.Sprintf("%v", e.Value)
	if e.Retraction {
		return "not: " + stem
	}
	return stem
}

// Metadata flattens an event to typed metadata. Confidence stays a number so
// range filters compare numerically rather than lexically, and the instant is
// stored twice: once human readable and lexically sortable, once as
// milliseconds for numeric range filters.
func (e Event) Metadata() map[string]any {
	md := map[string]any{
		"kind":        e.Kind,
		"subject":     e.Subject,
		"predicate":   e.Predicate,
		"confidence":  e.Confidence,
		"source":      e.Source,
		"retraction":  e.Retraction,
		"observed_at": formatTime(e.ObservedAt),
		"observed_ms": float64(e.ObservedAt.UTC().UnixMilli()),
	}
	// A blanket retraction has no value, and storing a nil would give the
	// field a type the filter language cannot compare.
	if e.Value != nil {
		md["value"] = e.Value
	}
	return md
}

// EventFromMetadata reads an event back. A record whose metadata is missing or
// malformed comes back with a zero ObservedAt, which the fold discards rather
// than letting it sort ahead of real events.
func EventFromMetadata(id string, m map[string]any) Event {
	str := func(k string) string { v, _ := m[k].(string); return v }
	conf, _ := m["confidence"].(float64)
	retraction, _ := m["retraction"].(bool)
	return Event{
		ID:         id,
		Kind:       str("kind"),
		Subject:    str("subject"),
		Predicate:  str("predicate"),
		Value:      m["value"], // string, float64, or bool, as stored
		Confidence: conf,
		Source:     str("source"),
		Retraction: retraction,
		ObservedAt: parseTime(str("observed_at")),
	}
}
