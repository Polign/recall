package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The registry's own history lives in the log it governs. Each change is an
// ordinary event about RegistrySubject under RegistryPredicate, whose value is
// one predicate's definition as JSON. Being an event, it is immutable, it is
// ordered by observation time like everything else, and it travels with
// exports. Reads leave these events out unless a query names them.
const (
	RegistrySubject   = "recall:registry"
	RegistryPredicate = "registry_change"
)

// ErrRegistryMismatch means this client's configured registry disagrees with
// the registry recorded in the store, or asks for a change that would make
// stored values unreadable. Two clients folding one log under different rules
// would give different answers, so the disagreement is an error rather than a
// silent second opinion. SyncRegistry records an intended change.
var ErrRegistryMismatch = errors.New("recall: registry does not match the store's registry log")

// registryTTL bounds how long a client trusts the registry log it last read.
// A change another client records is seen within this window.
const registryTTL = 30 * time.Second

// RegistryChange is one recorded definition of a predicate: what it was from
// At until the next change to the same name.
type RegistryChange struct {
	At        time.Time `json:"at"`
	Name      string    `json:"name"`
	Predicate Predicate `json:"predicate"`
	// Auto marks a definition an open client recorded because a write named a
	// predicate nothing had defined yet.
	Auto bool `json:"auto,omitempty"`
	// Retroactive marks a correction: the definition applies to the
	// predicate's whole history up to it, not only from At onward.
	Retroactive bool `json:"retroactive,omitempty"`
	// EventID is the log event holding this change.
	EventID string `json:"event_id"`
}

// registryRecord is a registry event's value.
type registryRecord struct {
	Name        string `json:"name"`
	Auto        bool   `json:"auto,omitempty"`
	Retroactive bool   `json:"retroactive,omitempty"`
	Predicate
}

func decodeRegistryChange(e Event) (RegistryChange, error) {
	bad := func(why string) (RegistryChange, error) {
		return RegistryChange{}, fmt.Errorf("%w: registry event %q %s", ErrInvalidEvent, e.ID, why)
	}
	if e.Subject != RegistrySubject || e.Predicate != RegistryPredicate || e.Retraction {
		return bad("is not a registry change")
	}
	raw, ok := e.Value.(string)
	if !ok {
		return bad("has no definition")
	}
	var rec registryRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return bad("has a malformed definition")
	}
	if err := (Registry{rec.Name: rec.Predicate}).Validate(); err != nil {
		return bad("has an invalid definition: " + err.Error())
	}
	return RegistryChange{At: e.ObservedAt.UTC(), Name: rec.Name, Predicate: rec.Predicate, Auto: rec.Auto, Retroactive: rec.Retroactive, EventID: e.ID}, nil
}

// decodeRegistryLog reads the registry changes in order. An automatic
// definition of a name that is already defined is left out: two open clients
// that both met a new predicate at once each record a definition, and the
// first one recorded is the one every client folds under.
func decodeRegistryLog(events []Event) ([]RegistryChange, error) {
	ordered := append([]Event(nil), events...)
	SortEvents(ordered)
	out := make([]RegistryChange, 0, len(ordered))
	defined := map[string]bool{}
	for _, e := range ordered {
		c, err := decodeRegistryChange(e)
		if err != nil {
			return nil, err
		}
		if c.Auto && defined[c.Name] {
			continue
		}
		defined[c.Name] = true
		out = append(out, c)
	}
	return out, nil
}

// schema is the registry a store reads and writes under: the configured
// registry joined with what the store's registry log records.
type schema struct {
	// reg is every predicate that can be read: the configured ones, plus any
	// the log defines that this client's configuration does not mention.
	reg Registry
	// writable is the configured registry. A predicate known only from the
	// log is readable, never writable: the registry a client accepts writes
	// under is the one it was given.
	writable Registry
	// alias maps a former name to the predicate that owns it now.
	alias map[string]string
	// defs holds each predicate's recorded definitions, oldest first,
	// including those made under a former name.
	defs map[string][]RegistryChange
	// log is the registry events themselves, oldest first.
	log []Event
}

// newSchema joins a configured registry with a registry log. With check set,
// a configured predicate that disagrees with its latest recorded definition
// is an error.
func newSchema(configured Registry, log []Event, check bool) (*schema, error) {
	changes, err := decodeRegistryLog(log)
	if err != nil {
		return nil, err
	}
	sc := &schema{reg: configured.Clone(), writable: configured, alias: map[string]string{}, defs: map[string][]RegistryChange{}}
	sc.log = append([]Event(nil), log...)
	SortEvents(sc.log)
	for name, p := range configured {
		for _, a := range p.Aliases {
			sc.alias[a] = name
		}
	}

	latest := map[string]RegistryChange{}
	for _, c := range changes {
		latest[c.Name] = c
	}
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	sort.Strings(names)

	// renamed maps a recorded name to the predicate that later took it as an
	// alias. A definition made after that alias was recorded takes the name
	// back.
	renamed := map[string]string{}
	for _, owner := range names {
		for _, a := range latest[owner].Predicate.Aliases {
			if old, ok := latest[a]; !ok || !old.At.After(latest[owner].At) {
				renamed[a] = owner
			}
		}
	}

	old := make([]string, 0, len(renamed))
	for name := range renamed {
		old = append(old, name)
	}
	sort.Strings(old)
	for _, name := range old {
		if _, configuredStill := configured[name]; configuredStill {
			if check {
				return nil, fmt.Errorf("%w: predicate %q was renamed to %q; update this registry, or call SyncRegistry to take the name back", ErrRegistryMismatch, name, renamed[name])
			}
			continue
		}
		if _, ok := sc.alias[name]; !ok {
			sc.alias[name] = renamed[name]
		}
	}

	for _, name := range names {
		logged := latest[name].Predicate
		if _, gone := renamed[name]; gone {
			continue
		}
		if mine, ok := configured[name]; ok {
			if check && (mine.Cardinal() != logged.Cardinal() || mine.valueType() != logged.valueType()) {
				return nil, fmt.Errorf("%w: predicate %q is %s-valued %s in the store and %s-valued %s in this client's registry; call SyncRegistry to record the change, or correct the registry",
					ErrRegistryMismatch, name, logged.Cardinality, logged.valueType(), mine.Cardinality, mine.valueType())
			}
			continue
		}
		if _, isAlias := sc.alias[name]; isAlias {
			continue
		}
		// Known only from the log: readable under its recorded definition.
		spec := logged.clone()
		spec.Aliases = slices.DeleteFunc(spec.Aliases, func(a string) bool {
			_, taken := configured[a]
			_, claimed := sc.alias[a]
			return taken || claimed
		})
		sc.reg[name] = spec
	}

	for _, c := range changes {
		owner := sc.canonical(c.Name)
		sc.defs[owner] = append(sc.defs[owner], c)
	}
	return sc, nil
}

// bundleRegistry is the registry an audit bundle carries: every readable
// predicate, each listing every former name its events may be stored under,
// so the bundle replays without the store it came from.
func (sc *schema) bundleRegistry() Registry {
	out := sc.reg.Clone()
	for name, p := range out {
		p.Aliases = sc.storedNames(name)[1:]
		if len(p.Aliases) == 0 {
			p.Aliases = nil
		}
		out[name] = p
	}
	return out
}

// canonical resolves a stored or requested predicate name to its owner.
func (sc *schema) canonical(name string) string {
	if _, ok := sc.reg[name]; ok {
		return name
	}
	if owner, ok := sc.alias[name]; ok {
		return owner
	}
	return name
}

// storedNames lists the names a predicate's events may be stored under.
func (sc *schema) storedNames(name string) []string {
	out := []string{name}
	for a, owner := range sc.alias {
		if owner == name {
			out = append(out, a)
		}
	}
	sort.Strings(out[1:])
	return out
}

// cardAt reports a predicate's cardinality at an instant: its latest recorded
// definition at or before then. Events older than the first recorded
// definition fold under that first one, which is the registry the store was
// first seen with. A predicate with no recorded definition folds as
// configured, and an unknown one as single-valued, which is the safer
// default: it supersedes rather than accumulates.
//
// A retroactive definition is a correction of the ones before it, so it also
// governs every instant before it was recorded; a later ordinary definition
// still takes over from its own time.
func (sc *schema) cardAt(predicate string, at time.Time) Cardinality {
	defs := sc.defs[predicate]
	if len(defs) == 0 {
		if spec, ok := sc.reg[predicate]; ok {
			return spec.Cardinal()
		}
		return Single
	}
	card := defs[0].Predicate.Cardinal()
	for _, d := range defs {
		if !d.At.After(at) || d.Retroactive {
			card = d.Predicate.Cardinal()
		}
	}
	return card
}

// fold reduces one pair's events, which may be stored under several names,
// to beliefs named by the predicate that owns them now.
func (sc *schema) fold(predicate string, events []Event, asOf time.Time) []Belief {
	beliefs := foldWith(events, func(at time.Time) Cardinality { return sc.cardAt(predicate, at) }, asOf)
	for i := range beliefs {
		beliefs[i].Predicate = predicate
	}
	return beliefs
}

// registryLog caches the schema one client or store reads under.
type registryLog struct {
	mu       sync.Mutex
	view     *schema
	err      error
	loadedAt time.Time
}

func (l *registryLog) invalidate() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.view, l.err, l.loadedAt = nil, nil, time.Time{}
	l.mu.Unlock()
}

// view returns the schema this store operates under, reading the registry
// log when the cached copy has expired.
func (s *Store) view() (*schema, error) {
	if s.reglog == nil || s.db == nil {
		return newSchema(s.registry, nil, false)
	}
	l := s.reglog
	l.mu.Lock()
	defer l.mu.Unlock()
	if (l.view != nil || l.err != nil) && time.Since(l.loadedAt) < registryTTL {
		return l.view, l.err
	}
	log, err := s.readRegistryLog()
	if err != nil {
		return nil, err
	}
	sc, err := newSchema(s.registry, log, true)
	if err != nil && !errors.Is(err, ErrRegistryMismatch) {
		return nil, err
	}
	l.view, l.err, l.loadedAt = sc, err, time.Now()
	return sc, err
}

func (s *Store) readRegistryLog() ([]Event, error) {
	events, err := s.completeEvents(map[string]any{"subject": RegistrySubject, "predicate": RegistryPredicate}, MaxHistoryEvents)
	if err != nil {
		return nil, err
	}
	SortEvents(events)
	return events, nil
}

// RegistryLog returns every recorded registry change, oldest first. It is
// empty for a store whose registry has never been synced.
func (s *Store) RegistryLog() ([]RegistryChange, error) {
	if s.registryErr != nil {
		return nil, s.registryErr
	}
	log, err := s.readRegistryLog()
	if err != nil {
		return nil, err
	}
	return decodeRegistryLog(log)
}

// SyncRegistry records this store's configured registry in the log, and
// returns the names of the predicates whose definitions it recorded. A
// predicate already recorded as configured writes nothing, so calling it at
// every start is safe.
//
// This is how a registry changes. A new predicate, a new description, a
// changed cardinality, a wider enum, and a rename (the new name listing the
// old one in Aliases) each become one event, effective from the moment it is
// recorded: events observed before a cardinality change keep folding under
// the old cardinality, so as-of reads answer as they did then. A change of
// stored type, such as number to string, is refused because the values
// already written could not be read under it. Splitting one predicate into
// two is not a registry change: which half an old event belongs to is a
// judgment about that event, so it is done by re-remembering.
func (s *Store) SyncRegistry() ([]string, error) {
	if s.registryErr != nil {
		return nil, s.registryErr
	}
	log, err := s.readRegistryLog()
	if err != nil {
		return nil, err
	}
	changes, err := decodeRegistryLog(log)
	if err != nil {
		return nil, err
	}
	latest := map[string]Predicate{}
	for _, c := range changes {
		latest[c.Name] = c.Predicate
	}

	var pending []string
	for _, name := range s.registry.Names() {
		spec := s.registry[name]
		old, recorded := latest[name]
		if recorded && sameDefinition(old, spec) {
			continue
		}
		if recorded && storageClass(old.valueType()) != storageClass(spec.valueType()) {
			return nil, fmt.Errorf("%w: predicate %q cannot change from %s to %s, because its stored values would become unreadable; register a new predicate instead",
				ErrRegistryMismatch, name, old.valueType(), spec.valueType())
		}
		for _, a := range spec.Aliases {
			if was, ok := latest[a]; ok && storageClass(was.valueType()) != storageClass(spec.valueType()) {
				return nil, fmt.Errorf("%w: predicate %q cannot take %q as an alias: %q stored %s values and %q stores %s",
					ErrRegistryMismatch, name, a, a, was.valueType(), name, spec.valueType())
			}
		}
		pending = append(pending, name)
	}
	if len(log)+len(pending) > MaxHistoryEvents {
		return nil, fmt.Errorf("%w: registry log would exceed %d events", ErrIncompleteHistory, MaxHistoryEvents)
	}

	defer s.reglog.invalidate()
	recorded := make([]string, 0, len(pending))
	for _, name := range pending {
		ev, err := s.appendDefinition(log, registryRecord{Name: name, Predicate: s.registry[name]})
		if err != nil {
			return recorded, err
		}
		log = append(log, ev)
		recorded = append(recorded, name)
	}
	return recorded, nil
}

// Redefine corrects a predicate's definition for its whole history, not only
// from now on. It is how a wrong guess made when an open write defined the
// predicate is fixed: redefining a single-valued predicate as multi-valued
// brings back every value it hid, in present and as-of answers alike, because
// answers are folded from the log at read time and nothing was deleted. The
// stored type cannot change, because values already written must stay
// readable.
func (s *Store) Redefine(name string, p Predicate) error {
	if s.registryErr != nil {
		return s.registryErr
	}
	sc, err := s.view()
	if err != nil {
		return err
	}
	name = sc.canonical(s.predicateName(name))
	old, ok := sc.reg[name]
	if !ok {
		return fmt.Errorf("predicate %q is not defined", name)
	}
	if p.ValueType == "" {
		p.ValueType = old.ValueType
	}
	if p.Description == "" {
		p.Description = old.Description
	}
	if err := (Registry{name: p}).Validate(); err != nil {
		return err
	}
	if storageClass(old.valueType()) != storageClass(p.valueType()) {
		return fmt.Errorf("%w: predicate %q cannot change from %s to %s, because its stored values would become unreadable",
			ErrRegistryMismatch, name, old.valueType(), p.valueType())
	}
	if len(sc.log) >= MaxHistoryEvents {
		return fmt.Errorf("%w: registry log is full at %d events", ErrIncompleteHistory, MaxHistoryEvents)
	}
	defer s.reglog.invalidate()
	_, err = s.appendDefinition(sc.log, registryRecord{Name: name, Retroactive: true, Predicate: p})
	return err
}

// Redefine corrects a predicate's definition for its whole history. See
// Store.Redefine. It writes, so it needs an embedder.
func (c *Client) Redefine(ctx context.Context, name string, p Predicate) error {
	s, err := c.forContext(ctx)
	if err != nil {
		return err
	}
	return s.Redefine(name, p)
}

// appendDefinition records one registry change after everything in log.
func (s *Store) appendDefinition(log []Event, rec registryRecord) (Event, error) {
	value, err := json.Marshal(rec)
	if err != nil {
		return Event{}, err
	}
	at := writeInstant(log, s.now())
	ev := Event{
		ID:         eventID(RegistrySubject, RegistryPredicate, string(value), false, at),
		Kind:       "fact",
		Subject:    RegistrySubject,
		Predicate:  RegistryPredicate,
		Value:      string(value),
		Confidence: 1,
		Source:     "tool_result",
		ObservedAt: at,
	}
	return ev, s.append(ev)
}

func sameDefinition(a, b Predicate) bool {
	return a.Cardinal() == b.Cardinal() && a.valueType() == b.valueType() && a.Description == b.Description &&
		slices.Equal(a.Allowed, b.Allowed) && slices.Equal(sorted(a.Aliases), sorted(b.Aliases))
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// SyncRegistry records the client's configured registry in the store's log.
// See Store.SyncRegistry. It writes, so it needs an embedder.
func (c *Client) SyncRegistry(ctx context.Context) ([]string, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.SyncRegistry()
}

// RegistryLog returns every recorded registry change, oldest first.
func (c *Client) RegistryLog(ctx context.Context) ([]RegistryChange, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.RegistryLog()
}

// isRegistryPair reports a pair that holds the registry's own history.
func isRegistryPair(p pair) bool {
	return p.predicate == RegistryPredicate && p.subject == RegistrySubject
}

// lenientValue normalizes a value named in a withdrawal. A value that no
// longer passes the predicate's rules, such as an enum value since removed,
// can still be held and must still be withdrawable.
func lenientValue(predicate string, spec Predicate, v any) (any, error) {
	out, err := normalizeValue(predicate, spec, v)
	if err == nil {
		return out, nil
	}
	if s, ok := v.(string); ok && storageClass(spec.valueType()) == typeString && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s), nil
	}
	return nil, err
}
