package engine

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
	// v2 adds Evidence and EvidenceID, and its bundles are checksummed with
	// DigestV3, which covers them. v1 bundles still replay and verify.
	EventVersion = "recall-event-v2"
	// eventVersionV1 predates evidence; its checksum uses DigestV2.
	eventVersionV1 = "recall-event-v1"
	// FoldVersion identifies observation-time ordering and the current single
	// and multi-value fold rules. Incompatible changes need a new replay path.
	//
	// v2 orders a retraction after the assertion it withdraws when the two
	// share an instant. v1 ordered that tie on the event id, so the same log
	// can fold differently under the two, and a v1 bundle must not be replayed
	// here as though nothing had changed.
	//
	// v3 adds three registry rules: a retroactive definition governs the
	// predicate's history before it, an automatic definition of a name
	// already defined is ignored, and a merge record makes one name read as
	// another. A bundle whose registry log uses neither
	// folds the same under v2 and is still written as v2, so its checksum
	// and older verifiers are unaffected.
	FoldVersion = "recall-fold-v3"
	// foldVersionV2 is written for bundles that do not need v3's rules.
	foldVersionV2 = "recall-fold-v2"
)

// foldVersionFor is the oldest fold version that replays a registry log
// correctly.
func foldVersionFor(log []Event) string {
	if usesFoldV3(log) {
		return FoldVersion
	}
	return foldVersionV2
}

// usesFoldV3 reports a registry log that folds differently under v3: one
// with a retroactive definition, or with an automatic definition of a name
// already defined.
func usesFoldV3(log []Event) bool {
	ordered := append([]Event(nil), log...)
	SortEvents(ordered)
	defined := map[string]bool{}
	for _, e := range ordered {
		c, err := decodeRegistryChange(e)
		if err != nil {
			continue
		}
		if c.Retroactive || c.AliasOf != "" || c.Auto && defined[c.Name] {
			return true
		}
		defined[c.Name] = true
	}
	return false
}

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
	// RegistryLog holds the store's recorded registry changes, when it has
	// any, so a replay folds each event under the cardinality in effect when
	// it was observed. They are kept apart from Events, which Scope bounds.
	RegistryLog []Event `json:"registry_log,omitempty"`
	Events      []Event `json:"events"`
	Digest      string  `json:"digest"`
}

// ExportAudit reads complete histories (at most MaxExport events), retaining
// events after AsOf as well. It fails on missing registry entries or partial
// reads. Completeness is subject to the backend's listing consistency; this
// does not acquire a cross-writer snapshot.
func (s *Store) ExportAudit(q AuditRequest) (AuditBundle, error) {
	if s.registryErr != nil {
		return AuditBundle{}, s.registryErr
	}
	sc, err := s.view()
	if err != nil {
		return AuditBundle{}, err
	}
	// The bundle carries every predicate its events can be read under, which
	// includes any the store's log defines and this client's registry omits.
	b := AuditBundle{Version: AuditVersion, EventVersion: EventVersion, FoldVersion: foldVersionFor(sc.log),
		Scope: AuditScope{Subject: normalizeSubject(q.Scope.Subject), Predicate: sc.canonical(strings.TrimSpace(q.Scope.Predicate))},
		AsOf:  q.AsOf.UTC(), Registry: sc.bundleRegistry(), RegistryLog: sc.log,
	}
	if b.AsOf.IsZero() {
		b.AsOf = s.now().UTC()
	}
	if err := b.validateHeader(); err != nil {
		return AuditBundle{}, err
	}
	events, err := s.ExportEvents(Export{Subject: b.Scope.Subject, Predicate: b.Scope.Predicate})
	if err != nil {
		return AuditBundle{}, err
	}
	b.Events = make([]Event, 0, len(events))
	for _, e := range events {
		if !isReservedPair(pair{e.Subject, e.Predicate}) {
			b.Events = append(b.Events, e)
		}
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
	sc, err := newSchema(b.Registry, b.RegistryLog, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAudit, err)
	}
	groups := map[pair][]Event{}
	for _, e := range b.Events {
		e.ObservedAt = e.ObservedAt.UTC()
		p := pair{normalizeSubject(e.Subject), sc.canonical(strings.TrimSpace(e.Predicate))}
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
		out = append(out, sc.fold(p.predicate, groups[p], b.AsOf)...)
	}
	return out, nil
}

func (b AuditBundle) validateHeader() error {
	invalid := func(why string) error { return fmt.Errorf("%w: %s", ErrInvalidAudit, why) }
	// Name the mismatch. A bundle that cannot be replayed is evidence someone
	// is trying to audit, so "unsupported" without saying which version moved
	// leaves them nothing to act on.
	for _, v := range []struct{ what, got, want string }{
		{"bundle", b.Version, AuditVersion},
		{"event", b.EventVersion, EventVersion},
		{"fold", b.FoldVersion, FoldVersion},
	} {
		if v.what == "event" && v.got == eventVersionV1 {
			continue
		}
		if v.what == "fold" && v.got == foldVersionV2 {
			if usesFoldV3(b.RegistryLog) {
				return invalid(fmt.Sprintf("fold version %q cannot replay this registry log, which needs %q", v.got, FoldVersion))
			}
			continue
		}
		if v.got != v.want {
			return invalid(fmt.Sprintf("%s version %q cannot be replayed by this build, which writes %q", v.what, v.got, v.want))
		}
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
	digest, err := DigestV3(b.Events)
	if b.EventVersion == eventVersionV1 {
		for _, e := range b.Events {
			if e.Evidence != "" || e.EvidenceID != "" {
				return "", fmt.Errorf("%w: event %q carries evidence, which %s bundles cannot record", ErrInvalidAudit, e.ID, eventVersionV1)
			}
		}
		digest, err = DigestV2(b.Events)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidAudit, err)
	}
	for _, e := range b.Events {
		// An event written under a former name belongs to the predicate that
		// owns that name now.
		owner := b.Registry.canonical(e.Predicate)
		if b.Scope.Subject != "" && e.Subject != b.Scope.Subject || b.Scope.Predicate != "" && owner != b.Scope.Predicate {
			return "", fmt.Errorf("%w: event %q is outside scope", ErrInvalidAudit, e.ID)
		}
		p, ok := b.Registry[owner]
		if !ok {
			return "", fmt.Errorf("%w: event %q predicate is not in registry", ErrInvalidAudit, e.ID)
		}
		if e.Value != nil {
			if err := checkStored(e.Predicate, p, e.Value); err != nil {
				return "", fmt.Errorf("%w: event %q: %w", ErrInvalidAudit, e.ID, err)
			}
		}
	}
	h := sha256.New()
	hashFields(h, b.Version, b.EventVersion, b.FoldVersion, b.Scope.Subject, b.Scope.Predicate, formatTime(b.AsOf), digest, strconv.Itoa(len(b.Registry)))
	for _, name := range b.Registry.Names() {
		p := b.Registry[name]
		hashFields(h, name, p.Cardinality, p.ValueType, p.Description)
		// Hashed only when present, so a bundle written before enums and
		// aliases existed keeps the checksum it was given.
		if len(p.Allowed) > 0 || len(p.Aliases) > 0 {
			hashFields(h, "allowed", strconv.Itoa(len(p.Allowed)))
			hashFields(h, p.Allowed...)
			hashFields(h, "aliases", strconv.Itoa(len(p.Aliases)))
			hashFields(h, sorted(p.Aliases)...)
		}
	}
	if len(b.RegistryLog) > 0 {
		if _, err := decodeRegistryLog(b.RegistryLog); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidAudit, err)
		}
		logDigest, err := DigestV3(b.RegistryLog)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidAudit, err)
		}
		hashFields(h, "registry-log", logDigest)
	}
	return "sha256:audit-v1:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DigestV2 hashes exact typed event fields, including case-sensitive strings
// and the IEEE-754 bits of finite numbers (so -0 and +0 differ). Instants are
// canonical UTC timestamps. It rejects invalid events and duplicate IDs. Digest
// remains the legacy, case-insensitive checksum for existing exports. It does
// not cover Evidence or EvidenceID; DigestV3 does.
func DigestV2(events []Event) (string, error) {
	return digestTyped(events, false)
}

// DigestV3 is DigestV2 that also covers each event's Evidence and EvidenceID,
// so an export cannot alter what a statement was drawn from undetected.
func DigestV3(events []Event) (string, error) {
	return digestTyped(events, true)
}

func digestTyped(events []Event, withEvidence bool) (string, error) {
	ordered := append([]Event(nil), events...)
	for i := range ordered {
		ordered[i].ObservedAt = ordered[i].ObservedAt.UTC()
	}
	SortEvents(ordered)
	h := sha256.New()
	label, prefix := "recall-events-digest-v2", "sha256:v2:"
	if withEvidence {
		label, prefix = "recall-events-digest-v3", "sha256:v3:"
	}
	hashFields(h, label, strconv.Itoa(len(ordered)))
	seen := make(map[string]bool, len(ordered))
	for _, e := range ordered {
		if seen[e.ID] {
			return "", fmt.Errorf("%w: duplicate event %q", ErrInvalidEvent, e.ID)
		}
		seen[e.ID] = true
		if _, err := DecodeEvent(e.ID, e.Metadata()); err != nil {
			return "", err
		}
		for _, field := range []string{e.ID, e.Subject, e.Predicate, e.Kind, e.Source, e.Evidence, e.EvidenceID} {
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
		if withEvidence {
			// Every event contributes both fields, empty or not, so the field
			// boundaries are fixed and no two logs share a digest.
			hashFields(h, e.Evidence, e.EvidenceID)
		}
	}
	return prefix + hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyDigest verifies either a legacy sha256: digest or a sha256:v2: digest.
// Legacy verification intentionally retains its original identity semantics.
func VerifyDigest(events []Event, expected string) error {
	var actual string
	if strings.HasPrefix(expected, "sha256:v2:") || strings.HasPrefix(expected, "sha256:v3:") {
		var err error
		actual, err = digestTyped(events, strings.HasPrefix(expected, "sha256:v3:"))
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
