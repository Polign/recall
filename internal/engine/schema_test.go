package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// schemaStore opens a store over db with its own registry and a settable
// clock, the way a second deployment of the same application would.
func schemaStore(db *fakeDB, r Registry, clock *time.Time) *Store {
	s := NewStore(db, "memories", r, func(string) []float32 { return []float32{1, 0, 0} })
	s.now = func() time.Time { return *clock }
	return s
}

func TestEnumStoresTheRegisteredSpellingAndRejectsOthers(t *testing.T) {
	clock := t0
	s := schemaStore(newFakeDB(), Registry{
		"deal_stage": {Cardinality: "single", ValueType: "enum", Description: "stage", Allowed: []string{"lead", "Won"}},
	}, &clock)
	res, err := s.Remember("fact", "acme", "deal_stage", " WON ", 1, "")
	if err != nil || res.Stored.Value != "Won" {
		t.Fatalf("stored %v, %v; want the registered spelling", res.Stored.Value, err)
	}
	_, err = s.Remember("fact", "acme", "deal_stage", "closed", 1, "")
	if err == nil || !strings.Contains(err.Error(), "lead, Won") {
		t.Fatalf("an unlisted value must be refused naming the allowed ones, got %v", err)
	}
}

func TestEnumNeedsAllowedValuesAndOnlyAnEnumHasThem(t *testing.T) {
	for name, r := range map[string]Registry{
		"enum without values": {"x": {Cardinality: "single", ValueType: "enum"}},
		"values on a string":  {"x": {Cardinality: "single", ValueType: "string", Allowed: []string{"a"}}},
		"repeated value":      {"x": {Cardinality: "single", ValueType: "enum", Allowed: []string{"a", "A"}}},
		"alias is registered": {"x": {Cardinality: "single", Aliases: []string{"y"}}, "y": {Cardinality: "single"}},
		"alias claimed twice": {"x": {Cardinality: "single", Aliases: []string{"z"}}, "y": {Cardinality: "single", Aliases: []string{"z"}}},
		"reserved name":       {RegistryPredicate: {Cardinality: "multi"}},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}

func TestDateIsOneValueAcrossZonesAndFiltersByRange(t *testing.T) {
	clock := t0
	s := schemaStore(newFakeDB(), Registry{
		"close_date": {Cardinality: "single", ValueType: "date", Description: "close"},
	}, &clock)
	res, err := s.Remember("fact", "acme", "close_date", "2026-10-01T09:00:00+02:00", 1, "")
	if err != nil || res.Stored.Value != "2026-10-01T07:00:00Z" {
		t.Fatalf("stored %v, %v; want the instant in UTC", res.Stored.Value, err)
	}
	clock = clock.Add(time.Second)
	again, err := s.Remember("fact", "acme", "close_date", "2026-10-01T07:00:00Z", 1, "")
	if err != nil || !again.Existing {
		t.Fatalf("the same instant in another zone must already be held: %+v, %v", again, err)
	}
	if _, err := s.Remember("fact", "globex", "close_date", "2026-12-24", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember("fact", "acme", "close_date", "next week", 1, ""); err == nil {
		t.Fatal("a value that is not a date must be refused")
	}
	got := recallValues(t, s, Query{Predicate: "close_date", ValueAfter: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	if !reflect.DeepEqual(got, []any{"2026-12-24"}) {
		t.Fatalf("dates after November = %v", got)
	}
	got = recallValues(t, s, Query{Predicate: "close_date", ValueBefore: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	if !reflect.DeepEqual(got, []any{"2026-10-01T07:00:00Z"}) {
		t.Fatalf("dates before November = %v", got)
	}
}

func refRegistry() Registry {
	return Registry{
		"works_at":   {Cardinality: "single", ValueType: "ref", Description: "employer"},
		"reports_to": {Cardinality: "single", ValueType: "ref", Description: "manager"},
		"deal_stage": {Cardinality: "single", ValueType: "enum", Description: "stage", Allowed: []string{"lead", "won"}},
	}
}

func TestRefValueIsASubjectAndCanBeReadFromEitherEnd(t *testing.T) {
	clock := t0
	s := schemaStore(newFakeDB(), refRegistry(), &clock)
	for _, w := range [][3]string{
		{"Dana", "works_at", " Acme "}, {"lee", "works_at", "acme"}, {"sam", "works_at", "globex"},
		{"lee", "reports_to", "dana"}, {"acme", "deal_stage", "lead"},
	} {
		clock = clock.Add(time.Second)
		res, err := s.Remember("fact", w[0], w[1], w[2], 1, "")
		if err != nil {
			t.Fatal(err)
		}
		if w[2] == " Acme " && res.Stored.Value != "acme" {
			t.Fatalf("a ref is stored the way subjects are, got %q", res.Stored.Value)
		}
	}

	got, err := s.Recall(Query{RefersTo: "ACME"})
	if err != nil {
		t.Fatal(err)
	}
	var who []string
	for _, b := range got {
		who = append(who, b.Subject+" "+b.Predicate)
	}
	if !reflect.DeepEqual(who, []string{"dana works_at", "lee works_at"}) {
		t.Fatalf("who refers to acme = %v", who)
	}
	if _, err := s.Recall(Query{RefersTo: "acme", Predicate: "deal_stage"}); err == nil {
		t.Fatal("refers_to with a predicate that is not a ref must be refused")
	}

	// Sam moves to Acme and Dana leaves: the reverse read follows the fold.
	clock = clock.Add(time.Second)
	if _, err := s.Remember("fact", "sam", "works_at", "acme", 1, ""); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if _, err := s.Remember("fact", "dana", "works_at", "initech", 1, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.Recall(Query{RefersTo: "acme", Predicate: "works_at"})
	if err != nil {
		t.Fatal(err)
	}
	who = nil
	for _, b := range got {
		who = append(who, b.Subject)
	}
	if !reflect.DeepEqual(who, []string{"lee", "sam"}) {
		t.Fatalf("after the moves, acme has %v", who)
	}
}

func TestFollowRefsAddsOneHopAndSaysHowItGotThere(t *testing.T) {
	clock := t0
	s := schemaStore(newFakeDB(), refRegistry(), &clock)
	for _, w := range [][3]string{
		{"lee", "works_at", "acme"}, {"acme", "deal_stage", "won"}, {"lee", "reports_to", "dana"}, {"dana", "works_at", "globex"},
	} {
		clock = clock.Add(time.Second)
		if _, err := s.Remember("fact", w[0], w[1], w[2], 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Recall(Query{Subject: "lee", FollowRefs: true})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	via := map[string]string{}
	for _, b := range got {
		lines = append(lines, b.Subject+" "+b.Predicate+" "+b.Value.(string))
		via[b.Subject] = b.Via
	}
	want := []string{"lee works_at acme", "lee reports_to dana", "acme deal_stage won", "dana works_at globex"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("followed = %v, want %v (globex is two hops away and must not appear)", lines, want)
	}
	if via["lee"] != "" || via["acme"] != got[0].EventID || via["dana"] != got[1].EventID {
		t.Fatalf("via = %v", via)
	}
	if capped, _ := s.Recall(Query{Subject: "lee", FollowRefs: true, Limit: 3}); len(capped) != 3 {
		t.Fatalf("following refs must stay within the limit, got %d", len(capped))
	}
}

func TestStarterRegistriesAreValidIndependentAndIncludeNote(t *testing.T) {
	for _, name := range StarterNames() {
		r, err := StarterRegistry(name)
		if err != nil || r.Validate() != nil {
			t.Fatalf("%s: %v %v", name, err, r.Validate())
		}
		if _, ok := r[NotePredicate]; !ok || len(r) < 10 {
			t.Fatalf("%s has %d predicates and note=%v", name, len(r), ok)
		}
		for p, spec := range r {
			if strings.TrimSpace(spec.Description) == "" {
				t.Fatalf("%s.%s has no description", name, p)
			}
		}
		delete(r, NotePredicate)
		if again, _ := StarterRegistry(name); again[NotePredicate].Cardinality == "" {
			t.Fatalf("%s is not an independent copy", name)
		}
	}
	if _, err := StarterRegistry("legal"); err == nil || !strings.Contains(err.Error(), "support") {
		t.Fatalf("an unknown starter must name the ones that exist, got %v", err)
	}
	if !reflect.DeepEqual(DefaultRegistry(), mustStarter(t, StarterCoding)) {
		t.Fatal("the default registry is the coding starter")
	}
}

func mustStarter(t *testing.T, name string) Registry {
	t.Helper()
	r, err := StarterRegistry(name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func editorRegistry(cardinality string) Registry {
	return Registry{"prefers_editor": {Cardinality: cardinality, ValueType: "string", Description: "editor"}}
}

func TestCardinalityChangeKeepsPastAnswers(t *testing.T) {
	db := newFakeDB()
	clock := t0
	single := schemaStore(db, editorRegistry("single"), &clock)
	if names, err := single.SyncRegistry(); err != nil || !reflect.DeepEqual(names, []string{"note", "prefers_editor"}) {
		t.Fatalf("first sync recorded %v, %v", names, err)
	}
	clock = at(time.Hour)
	mustRemember(t, single, "prefers_editor", "vim")
	clock = at(2 * time.Hour)
	mustRemember(t, single, "prefers_editor", "emacs")
	tuesday := at(3 * time.Hour)

	clock = at(4 * time.Hour)
	multi := schemaStore(db, editorRegistry("multi"), &clock)
	if _, err := multi.Recall(Query{Subject: "user", Predicate: "prefers_editor"}); !errors.Is(err, ErrRegistryMismatch) {
		t.Fatalf("a client whose registry disagrees with the store must be refused, got %v", err)
	}
	if _, err := multi.Remember("preference", "user", "prefers_editor", "helix", 1, ""); !errors.Is(err, ErrRegistryMismatch) {
		t.Fatalf("and must not write either, got %v", err)
	}
	if names, err := multi.SyncRegistry(); err != nil || !reflect.DeepEqual(names, []string{"prefers_editor"}) {
		t.Fatalf("sync recorded %v, %v", names, err)
	}
	if names, err := multi.SyncRegistry(); err != nil || len(names) != 0 {
		t.Fatalf("a second sync must write nothing, recorded %v, %v", names, err)
	}

	clock = at(5 * time.Hour)
	mustRemember(t, multi, "prefers_editor", "helix")
	if got := recallValues(t, multi, Query{Subject: "user", Predicate: "prefers_editor"}); !reflect.DeepEqual(got, []any{"emacs", "helix"}) {
		t.Fatalf("after the change values accumulate, got %v", got)
	}
	// Folding all of history as multi would answer [vim emacs] for Tuesday.
	if got := recallValues(t, multi, Query{Subject: "user", Predicate: "prefers_editor", AsOf: tuesday}); !reflect.DeepEqual(got, []any{"emacs"}) {
		t.Fatalf("Tuesday's answer changed to %v", got)
	}

	log, err := multi.RegistryLog()
	if err != nil || len(log) != 3 || log[2].Name != "prefers_editor" || log[2].Predicate.Cardinality != "multi" {
		t.Fatalf("registry log = %+v, %v", log, err)
	}
	// The registry's history is in the log but is not something remembered.
	for _, b := range mustRecall(t, multi, Query{}) {
		if b.Predicate == RegistryPredicate {
			t.Fatalf("a broad recall returned the registry's own history: %+v", b)
		}
	}
}

func mustRecall(t *testing.T, s *Store, q Query) []Belief {
	t.Helper()
	got, err := s.Recall(q)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRenameKeepsOneHistoryAcrossBothNames(t *testing.T) {
	db := newFakeDB()
	clock := at(time.Hour)
	old := schemaStore(db, editorRegistry("single"), &clock)
	mustRemember(t, old, "prefers_editor", "vim")

	clock = at(2 * time.Hour)
	renamed := schemaStore(db, Registry{
		"favorite_editor": {Cardinality: "single", ValueType: "string", Description: "editor", Aliases: []string{"prefers_editor"}},
	}, &clock)
	if got := mustRecall(t, renamed, Query{Subject: "user", Predicate: "favorite_editor"}); len(got) != 1 || got[0].Value != "vim" || got[0].Predicate != "favorite_editor" {
		t.Fatalf("the old event must answer under the new name, got %+v", got)
	}
	// A write under the old name lands under the new one and supersedes.
	res, err := renamed.Remember("preference", "user", "prefers_editor", "helix", 1, "")
	if err != nil || res.Stored.Predicate != "favorite_editor" || len(res.Superseded) != 1 || res.Superseded[0].Value != "vim" {
		t.Fatalf("remember under the old name = %+v, %v", res, err)
	}
	for _, q := range []Query{
		{Subject: "user", Predicate: "prefers_editor"}, {Subject: "user"}, {Predicate: "favorite_editor"}, {},
	} {
		if got := recallValues(t, renamed, q); !reflect.DeepEqual(got, []any{"helix"}) {
			t.Fatalf("recall %+v = %v, want one belief across both names", q, got)
		}
	}
	history, err := renamed.History("user", "favorite_editor")
	if err != nil || len(history) != 2 || history[0].Predicate != "prefers_editor" || history[1].Predicate != "favorite_editor" {
		t.Fatalf("history = %+v, %v; want both events, each under the name it was written with", history, err)
	}
	if n, err := renamed.Forget("user", "prefers_editor", ""); err != nil || n != 1 {
		t.Fatalf("forget under the old name withdrew %d, %v", n, err)
	}

	// Once recorded, a client still using the old name is told so.
	if _, err := renamed.SyncRegistry(); err != nil {
		t.Fatal(err)
	}
	stale := schemaStore(db, editorRegistry("single"), &clock)
	if _, err := stale.Recall(Query{Subject: "user"}); !errors.Is(err, ErrRegistryMismatch) || !strings.Contains(err.Error(), "favorite_editor") {
		t.Fatalf("a client on the old name must be pointed at the new one, got %v", err)
	}
	// A client that never heard of either still reads it, under the new name.
	clock = at(3 * time.Hour)
	mustRemember(t, renamed, "favorite_editor", "zed")
	other := schemaStore(db, Registry{"timezone": {Cardinality: "single", Description: "tz"}}, &clock)
	if got := mustRecall(t, other, Query{Subject: "user"}); len(got) != 1 || got[0].Predicate != "favorite_editor" || got[0].Value != "zed" {
		t.Fatalf("a predicate known only from the log = %+v", got)
	}
	if _, err := other.Remember("preference", "user", "favorite_editor", "nano", 1, ""); err == nil {
		t.Fatal("a predicate this client's registry does not list must stay unwritable")
	}
}

func TestSyncRefusesAChangeOfStoredType(t *testing.T) {
	db := newFakeDB()
	clock := t0
	number := schemaStore(db, Registry{"port": {Cardinality: "single", ValueType: "number", Description: "port"}}, &clock)
	if _, err := number.SyncRegistry(); err != nil {
		t.Fatal(err)
	}
	text := schemaStore(db, Registry{"port": {Cardinality: "single", ValueType: "string", Description: "port"}}, &clock)
	if _, err := text.SyncRegistry(); !errors.Is(err, ErrRegistryMismatch) {
		t.Fatalf("number to string must be refused, got %v", err)
	}
	// String to enum keeps every stored value readable, so it is a change.
	plain := schemaStore(db, Registry{"stage": {Cardinality: "single", ValueType: "string", Description: "stage"}}, &clock)
	if _, err := plain.Remember("fact", "acme", "stage", "paused", 1, ""); err != nil {
		t.Fatal(err)
	}
	clock = at(time.Hour)
	enum := schemaStore(db, Registry{"stage": {Cardinality: "single", ValueType: "enum", Description: "stage", Allowed: []string{"lead", "won"}}}, &clock)
	if _, err := enum.SyncRegistry(); err != nil {
		t.Fatal(err)
	}
	if got := recallValues(t, enum, Query{Subject: "acme", Predicate: "stage"}); !reflect.DeepEqual(got, []any{"paused"}) {
		t.Fatalf("a value written before the enum existed must stay readable, got %v", got)
	}
	if n, err := enum.Forget("acme", "stage", "paused"); err != nil || n != 1 {
		t.Fatalf("and withdrawable: %d, %v", n, err)
	}
}

func TestAuditBundleReplaysRenamesAndCardinalityChanges(t *testing.T) {
	db := newFakeDB()
	clock := t0
	single := schemaStore(db, editorRegistry("single"), &clock)
	if _, err := single.SyncRegistry(); err != nil {
		t.Fatal(err)
	}
	clock = at(time.Hour)
	mustRemember(t, single, "prefers_editor", "vim")
	clock = at(2 * time.Hour)
	mustRemember(t, single, "prefers_editor", "emacs")

	clock = at(3 * time.Hour)
	s := schemaStore(db, Registry{
		"favorite_editor": {Cardinality: "multi", ValueType: "string", Description: "editor", Aliases: []string{"prefers_editor"}},
	}, &clock)
	if _, err := s.SyncRegistry(); err != nil {
		t.Fatal(err)
	}
	clock = at(4 * time.Hour)
	mustRemember(t, s, "favorite_editor", "helix")

	clock = at(5 * time.Hour)
	for _, scope := range []AuditScope{{}, {Subject: "user", Predicate: "prefers_editor"}} {
		b, err := s.ExportAudit(AuditRequest{Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Events) != 3 || len(b.RegistryLog) != 3 {
			t.Fatalf("bundle holds %d events and %d registry changes", len(b.Events), len(b.RegistryLog))
		}
		replayed, err := b.Replay()
		if err != nil {
			t.Fatal(err)
		}
		live := mustRecall(t, s, Query{Subject: "user", Predicate: "favorite_editor"})
		if !reflect.DeepEqual(replayed, live) || len(live) != 2 {
			t.Fatalf("replay %+v differs from live %+v", replayed, live)
		}
		b.RegistryLog = b.RegistryLog[:2]
		if err := b.Verify(); !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("dropping a registry change must break the checksum, got %v", err)
		}
	}
}

func TestClientSyncsAndDetectsARegistryMismatch(t *testing.T) {
	ctx := context.Background()
	backend := newLockedBackend()
	open := func(cardinality string) *Client {
		c, err := NewClient(Config{Backend: backend, Collection: "m", Registry: editorRegistry(cardinality),
			Embedder: EmbedFunc(func(context.Context, string) ([]float32, error) { return []float32{1}, nil })})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := open("single")
	if names, err := first.SyncRegistry(ctx); err != nil || len(names) != 2 {
		t.Fatalf("sync recorded %v, %v", names, err)
	}
	if _, err := first.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim"}); err != nil {
		t.Fatal(err)
	}
	second := open("multi")
	if _, err := second.Recall(ctx, Query{Subject: "user"}); !errors.Is(err, ErrRegistryMismatch) {
		t.Fatalf("want a registry mismatch, got %v", err)
	}
	if _, err := second.SyncRegistry(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := second.Recall(ctx, Query{Subject: "user"}); err != nil || len(got) != 1 {
		t.Fatalf("after sync: %+v, %v", got, err)
	}
	if log, err := second.RegistryLog(ctx); err != nil || len(log) != 3 {
		t.Fatalf("registry log = %+v, %v", log, err)
	}
}
