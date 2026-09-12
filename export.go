package recall

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Export selects part of the log for audit. An empty field means "every one of
// these": zero Export exports everything the store holds.
type Export struct {
	Subject   string
	Predicate string
	// Limit bounds how many events are read. Zero uses MaxExport.
	Limit int
}

// MaxExport bounds an unbounded export, so that asking for everything cannot
// turn into an unbounded scan by accident.
const MaxExport = 10000

// ExportEvents returns the raw event log, oldest first.
//
// This is the audit primitive. A belief is a pure function of these events, so
// a reviewer given this log can re-derive every answer the store ever gave,
// including the ones it no longer gives: superseded statements and retracted
// ones are here too, because nothing is deleted.
func (s *Store) ExportEvents(q Export) ([]Event, error) {
	filter := map[string]any{}
	if q.Subject != "" {
		filter["subject"] = normalizeSubject(q.Subject)
	}
	if q.Predicate != "" {
		filter["predicate"] = strings.TrimSpace(q.Predicate)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = MaxExport
	}
	events, err := s.eventsMatching(filter, limit)
	if err != nil {
		return nil, err
	}
	SortEvents(events)
	return events, nil
}

// SortEvents orders a log oldest first, breaking ties on id so that two
// exports of the same events are byte-identical.
func SortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].ObservedAt.Equal(events[j].ObservedAt) {
			return events[i].ObservedAt.Before(events[j].ObservedAt)
		}
		return events[i].ID < events[j].ID
	})
}

// Digest is a stable checksum over a log.
//
// It lets a reviewer confirm that an export they were handed is the one the
// store produced, and lets two parties compare logs without exchanging them.
// The input is each event's canonical fields in a fixed order, so the digest
// depends on the content of the log and not on how it was serialised, paged,
// or which node answered.
func Digest(events []Event) string {
	ordered := make([]Event, len(events))
	copy(ordered, events)
	SortEvents(ordered)

	h := sha256.New()
	for _, e := range ordered {
		// Length-prefixed so that no combination of field values can be
		// rearranged into a different log with the same digest.
		for _, field := range []string{
			e.ID,
			e.Kind,
			e.Subject,
			e.Predicate,
			ValueKey(e.Value),
			strconv.FormatFloat(e.Confidence, 'f', -1, 64),
			e.Source,
			strconv.FormatBool(e.Retraction),
			formatTime(e.ObservedAt),
		} {
			fmt.Fprintf(h, "%d:%s", len(field), field)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
