package recall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// AuditVersion identifies the bundle envelope and its canonical checksum.
	AuditVersion = "recall-audit-v1"
	// EventVersion identifies the typed Event fields recorded in a bundle.
	EventVersion = "recall-event-v1"
	// FoldVersion identifies observation-time ordering and the current single
	// and multi-value fold rules. Incompatible changes need a new replay path.
	FoldVersion = "recall-fold-v1"
)

var (
	// ErrInvalidAudit marks unsupported versions or invalid bundle inputs.
	ErrInvalidAudit = errors.New("recall: invalid audit bundle")
	// ErrDigestMismatch means validated content differs from its checksum.
	ErrDigestMismatch = errors.New("recall: digest mismatch")
)

// AuditScope selects complete histories. Empty fields match all subjects or
// predicates. Unlike Export, this scope cannot request a truncated event subset.
type AuditScope struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
}

// AuditRequest selects histories and the instant at which to replay them.
// A zero AsOf captures the store's current time once, before reading events.
type AuditRequest struct {
	Scope AuditScope
	AsOf  time.Time
}

// AuditBundle records the inputs needed to reproduce exact beliefs offline.
// The digest covers every field except Digest itself, independent of event or
// registry iteration order. It is a checksum, not a signature or proof that the
// exporter supplied every event. Retain its digest through a trusted channel.
type AuditBundle struct {
	Version      string     `json:"version"`
	EventVersion string     `json:"event_version"`
	FoldVersion  string     `json:"fold_version"`
	Scope        AuditScope `json:"scope"`
	AsOf         time.Time  `json:"as_of"`
	Registry     Registry   `json:"registry"`
	Events       []Event    `json:"events"`
	Digest       string     `json:"digest"`
}

// ExportAudit reads complete histories (at most MaxExport events), retaining
// events after AsOf as well. It fails on missing registry entries or partial
// reads. Completeness is subject to the backend's listing consistency; this
// does not acquire a cross-writer snapshot.
func (s *Store) ExportAudit(q AuditRequest) (AuditBundle, error) {
	b := AuditBundle{Version: AuditVersion, EventVersion: EventVersion, FoldVersion: FoldVersion,
		Scope: AuditScope{Subject: normalizeSubject(q.Scope.Subject), Predicate: strings.TrimSpace(q.Scope.Predicate)},
		AsOf:  q.AsOf.UTC(), Registry: s.registry.Clone(),
	}
	if b.AsOf.IsZero() {
		b.AsOf = s.now().UTC()
	}
	if err := b.validateHeader(); err != nil {
		return AuditBundle{}, err
	}
	var err error
	b.Events, err = s.ExportEvents(Export{Subject: b.Scope.Subject, Predicate: b.Scope.Predicate})
	if err != nil {
		return AuditBundle{}, err
	}
	b.Digest, err = b.checksum()
	if err != nil {
		return AuditBundle{}, err
	}
	return b, nil
}

// ExportAudit exports without requiring an embedder and honors cancellation.
func (c *Client) ExportAudit(ctx context.Context, q AuditRequest) (AuditBundle, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return AuditBundle{}, err
	}
	b, err := s.ExportAudit(q)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return AuditBundle{}, err
	}
	return b, nil
}

// Verify checks supported versions, registry, event validity, scope, and digest.
// Unknown versions and duplicate IDs are refused, never silently interpreted.
func (b AuditBundle) Verify() error {
	want, err := b.checksum()
	if err != nil {
		return err
	}
	if b.Digest != want {
		return ErrDigestMismatch
	}
	return nil
}

// Replay verifies the bundle then folds all included pairs at its fixed AsOf.
// Results are ordered by subject, predicate, then the fold's value order. This
// reproduces exact beliefs in Scope, not semantic ranking or a past read made
// against a different registry or a different set of visible events.
func (b AuditBundle) Replay() ([]Belief, error) {
	if err := b.Verify(); err != nil {
		return nil, err
	}
	type pair struct{ subject, predicate string }
	groups := map[pair][]Event{}
	for _, e := range b.Events {
		e.ObservedAt = e.ObservedAt.UTC()
		p := pair{normalizeSubject(e.Subject), strings.TrimSpace(e.Predicate)}
		groups[p] = append(groups[p], e)
	}
	keys := make([]pair, 0, len(groups))
	for p := range groups {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].subject != keys[j].subject {
			return keys[i].subject < keys[j].subject
		}
		return keys[i].predicate < keys[j].predicate
	})
	out := make([]Belief, 0)
	for _, p := range keys {
		out = append(out, Fold(groups[p], b.Registry[p.predicate].Cardinal(), b.AsOf)...)
	}
	return out, nil
}

func (b AuditBundle) validateHeader() error {
	invalid := func(why string) error { return fmt.Errorf("%w: %s", ErrInvalidAudit, why) }
	if b.Version != AuditVersion || b.EventVersion != EventVersion || b.FoldVersion != FoldVersion {
		return invalid("unsupported bundle, event, or fold version")
	}
	if b.AsOf.IsZero() || parseTime(formatTime(b.AsOf)).IsZero() {
		return invalid("as_of must be an explicit RFC3339 instant")
	}
	if !utf8.ValidString(b.Scope.Subject) || b.Scope.Subject != normalizeSubject(b.Scope.Subject) {
		return invalid("subject scope must be normalized UTF-8")
	}
	if len(b.Registry) == 0 {
		return invalid("registry must not be empty")
	}
	if err := b.Registry.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAudit, err)
	}
	for _, p := range b.Registry {
		if !utf8.ValidString(p.Description) {
			return invalid("registry description must be UTF-8")
		}
	}
	if b.Scope.Predicate != "" {
		if _, ok := b.Registry[b.Scope.Predicate]; !ok {
			return invalid("predicate scope is not in the registry")
		}
	}
	return nil
}

func (b AuditBundle) checksum() (string, error) {
	if err := b.validateHeader(); err != nil {
		return "", err
	}
	if len(b.Events) > MaxExport {
		return "", fmt.Errorf("%w: more than %d events", ErrInvalidAudit, MaxExport)
	}
	digest, err := DigestV2(b.Events)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidAudit, err)
	}
	for _, e := range b.Events {
		if b.Scope.Subject != "" && e.Subject != b.Scope.Subject || b.Scope.Predicate != "" && e.Predicate != b.Scope.Predicate {
			return "", fmt.Errorf("%w: event %q is outside scope", ErrInvalidAudit, e.ID)
		}
		p, ok := b.Registry[e.Predicate]
		if !ok {
			return "", fmt.Errorf("%w: event %q predicate is not in registry", ErrInvalidAudit, e.ID)
		}
		if e.Value != nil {
			if _, err := normalizeValue(e.Predicate, p, e.Value); err != nil {
				return "", fmt.Errorf("%w: event %q: %w", ErrInvalidAudit, e.ID, err)
			}
		}
	}
	h := sha256.New()
	hashFields(h, b.Version, b.EventVersion, b.FoldVersion, b.Scope.Subject, b.Scope.Predicate, formatTime(b.AsOf), digest, strconv.Itoa(len(b.Registry)))
	for _, name := range b.Registry.Names() {
		p := b.Registry[name]
		hashFields(h, name, p.Cardinality, p.ValueType, p.Description)
	}
	return "sha256:audit-v1:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DigestV2 hashes exact typed event fields, including case-sensitive strings
// and the IEEE-754 bits of finite numbers (so -0 and +0 differ). Instants are
// canonical UTC timestamps. It rejects invalid events and duplicate IDs. Digest
// remains the legacy, case-insensitive checksum for existing exports.
func DigestV2(events []Event) (string, error) {
	ordered := append([]Event(nil), events...)
	for i := range ordered {
		ordered[i].ObservedAt = ordered[i].ObservedAt.UTC()
	}
	SortEvents(ordered)
	h := sha256.New()
	hashFields(h, "recall-events-digest-v2", strconv.Itoa(len(ordered)))
	seen := make(map[string]bool, len(ordered))
	for _, e := range ordered {
		if seen[e.ID] {
			return "", fmt.Errorf("%w: duplicate event %q", ErrInvalidEvent, e.ID)
		}
		seen[e.ID] = true
		if _, err := DecodeEvent(e.ID, e.Metadata()); err != nil {
			return "", err
		}
		for _, field := range []string{e.ID, e.Subject, e.Predicate, e.Kind, e.Source} {
			if !utf8.ValidString(field) {
				return "", fmt.Errorf("%w: event %q has invalid UTF-8", ErrInvalidEvent, e.ID)
			}
		}
		valueType, value := "null", ""
		switch v := e.Value.(type) {
		case string:
			if !utf8.ValidString(v) {
				return "", fmt.Errorf("%w: event %q has invalid UTF-8 value", ErrInvalidEvent, e.ID)
			}
			valueType, value = "string", v
		case float64:
			valueType, value = "number", floatBits(v)
		case bool:
			valueType, value = "boolean", strconv.FormatBool(v)
		}
		hashFields(h, e.ID, e.Kind, e.Subject, e.Predicate, valueType, value, floatBits(e.Confidence), e.Source, strconv.FormatBool(e.Retraction), formatTime(e.ObservedAt))
	}
	return "sha256:v2:" + hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyDigest verifies either a legacy sha256: digest or a sha256:v2: digest.
// Legacy verification intentionally retains its original identity semantics.
func VerifyDigest(events []Event, expected string) error {
	var actual string
	if strings.HasPrefix(expected, "sha256:v2:") {
		var err error
		actual, err = DigestV2(events)
		if err != nil {
			return err
		}
	} else if strings.HasPrefix(expected, "sha256:") && len(expected) == len("sha256:")+64 {
		actual = Digest(events)
	} else {
		return fmt.Errorf("recall: unsupported digest format")
	}
	if actual != expected {
		return ErrDigestMismatch
	}
	return nil
}

func floatBits(v float64) string { return fmt.Sprintf("%016x", math.Float64bits(v)) }

func hashFields(h hash.Hash, fields ...string) {
	for _, field := range fields {
		fmt.Fprintf(h, "%d:%s", len(field), field)
	}
}
