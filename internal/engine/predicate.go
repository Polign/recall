package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	predicateName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	validSources  = map[string]bool{"user_stated": true, "agent_inferred": true, "tool_result": true}
	validKinds    = map[string]bool{"fact": true, "preference": true}
)

// Predicate is one registry entry. Cardinality decides what a second value
// for the same subject means: "single" makes a newer value supersede the old
// one, "multi" makes it an additional fact. ValueType decides what a value
// IS: a string, a number, a boolean, an enum, a date, or a ref. Values are
// stored with that type, so a number compares numerically in recall filters
// instead of lexically.
//
// An enum is one of Allowed, stored in the spelling Allowed gives it. A date
// is a calendar day (2026-10-01) or an RFC3339 instant, stored in one
// canonical form. A ref names another subject, stored the way subjects are,
// so the value of one belief is the subject of others.
type Predicate struct {
	Cardinality string `json:"cardinality"`
	ValueType   string `json:"value_type"`
	Description string `json:"description"`
	// Allowed lists an enum's values. Only an enum has it.
	Allowed []string `json:"allowed,omitempty"`
	// Aliases are names this predicate used to have. Events written under an
	// alias fold with this predicate, and a write naming an alias is stored
	// under this predicate's name. Renaming a predicate is giving the new
	// name the old one as an alias.
	Aliases []string `json:"aliases,omitempty"`
}

// Value types a predicate may declare.
const (
	typeString  = "string"
	typeNumber  = "number"
	typeBoolean = "boolean"
	typeEnum    = "enum"
	typeDate    = "date"
	typeRef     = "ref"
)

// valueType is the declared type, with the unset default filled in.
func (p Predicate) valueType() string {
	if p.ValueType == "" {
		return typeString
	}
	return p.ValueType
}

// storageClass is how a value type is stored: an enum, a date, and a ref are
// all strings in the log. A predicate may move between types of one class
// without making its history unreadable.
func storageClass(valueType string) string {
	switch valueType {
	case typeNumber, typeBoolean:
		return valueType
	default:
		return typeString
	}
}

func (p Predicate) clone() Predicate {
	p.Allowed = append([]string(nil), p.Allowed...)
	p.Aliases = append([]string(nil), p.Aliases...)
	return p
}

// Cardinal returns the predicate's cardinality as the fold understands it.
func (p Predicate) Cardinal() Cardinality {
	if p.Cardinality == string(Multi) {
		return Multi
	}
	return Single
}

// Registry is the closed set of predicates the store accepts. A write with an
// unregistered predicate is rejected, so an agent cannot invent near-duplicate
// predicates ("editor_preference" beside "prefers_editor") and split one fact
// across two names that never meet.
type Registry map[string]Predicate

// NotePredicate is where a statement goes when no registered predicate fits
// it. Every client and store registers it, so "nothing matched" always has a
// place to be written instead of being dropped. A note is multi-valued free
// text: it never supersedes anything, and recall ranks it after typed beliefs.
const NotePredicate = "note"

// notePredicate is the only spec NotePredicate may have.
var notePredicate = Predicate{Cardinality: "multi", ValueType: "string",
	Description: "A statement worth keeping that no other predicate fits, in the words it was stated"}

// withNote returns a copy of r with NotePredicate registered. A registry may
// define note itself, with its own description, but not as anything other
// than multi-valued text: a single-valued note would let one note erase
// another.
func (r Registry) withNote() (Registry, error) {
	out := r.Clone()
	if p, ok := out[NotePredicate]; ok {
		if p.Cardinal() != Multi || (p.ValueType != "" && p.ValueType != "string") {
			return nil, fmt.Errorf("predicate registry: %q is reserved for statements no other predicate fits and must be multi-valued string", NotePredicate)
		}
		return out, nil
	}
	out[NotePredicate] = notePredicate
	return out, nil
}

// LoadRegistry parses and validates a registry document.
func LoadRegistry(raw []byte) (Registry, error) {
	var r Registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("predicate registry: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Clone returns an independent copy of this registry.
func (r Registry) Clone() Registry {
	out := make(Registry, len(r))
	for name, p := range r {
		out[name] = p.clone()
	}
	return out
}

// Validate checks the names, cardinalities, value types, enum values, and
// aliases of a registry.
func (r Registry) Validate() error {
	aliasOf := map[string]string{}
	for _, name := range r.Names() {
		p := r[name]
		if !predicateName.MatchString(name) {
			return fmt.Errorf("predicate registry: %q is not snake_case", name)
		}
		if name == RegistryPredicate {
			return fmt.Errorf("predicate registry: %q is reserved for the registry's own history", name)
		}
		if p.Cardinality != "single" && p.Cardinality != "multi" {
			return fmt.Errorf("predicate registry: %q has cardinality %q, want single or multi", name, p.Cardinality)
		}
		switch p.ValueType {
		case "", typeString, typeNumber, typeBoolean, typeEnum, typeDate, typeRef:
		default:
			return fmt.Errorf("predicate registry: %q has value_type %q, want string, number, boolean, enum, date, or ref", name, p.ValueType)
		}
		if p.ValueType == typeEnum && len(p.Allowed) == 0 {
			return fmt.Errorf("predicate registry: %q is an enum and must list its allowed values", name)
		}
		if p.ValueType != typeEnum && len(p.Allowed) > 0 {
			return fmt.Errorf("predicate registry: %q lists allowed values but has value_type %q, want enum", name, p.valueType())
		}
		seen := map[string]bool{}
		for _, v := range p.Allowed {
			k := strings.ToLower(v)
			if strings.TrimSpace(v) == "" || v != strings.TrimSpace(v) {
				return fmt.Errorf("predicate registry: %q has an allowed value that is empty or padded with spaces", name)
			}
			if seen[k] {
				return fmt.Errorf("predicate registry: %q lists allowed value %q twice", name, v)
			}
			seen[k] = true
		}
		for _, a := range p.Aliases {
			if !predicateName.MatchString(a) || a == RegistryPredicate || a == NotePredicate {
				return fmt.Errorf("predicate registry: %q has alias %q, which is not a usable predicate name", name, a)
			}
			if _, registered := r[a]; registered {
				return fmt.Errorf("predicate registry: %q is both a predicate and an alias of %q; remove the old entry to rename it", a, name)
			}
			if other, taken := aliasOf[a]; taken {
				return fmt.Errorf("predicate registry: alias %q is claimed by both %q and %q", a, other, name)
			}
			aliasOf[a] = name
		}
	}
	return nil
}

// canonical resolves a name to the predicate that owns it: itself when it is
// registered, the predicate it is an alias of otherwise.
func (r Registry) canonical(name string) string {
	if _, ok := r[name]; ok {
		return name
	}
	for owner, p := range r {
		for _, a := range p.Aliases {
			if a == name {
				return owner
			}
		}
	}
	return name
}

// storedNames lists every name a predicate's events may be stored under: its
// own first, then its aliases.
func (r Registry) storedNames(name string) []string {
	return append([]string{name}, r[name].Aliases...)
}

// Names returns the registered predicates sorted, for error messages and the
// system prompt.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for name := range r {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PromptTable renders the registry for an agent's system prompt.
func (r Registry) PromptTable() string {
	var b strings.Builder
	for _, name := range r.Names() {
		p := r[name]
		fmt.Fprintf(&b, "- %s (%s-valued %s): %s%s\n", name, p.Cardinality, p.valueType(), p.Description, valueHint(p))
	}
	return b.String()
}

// valueHint tells an agent what a typed value has to look like, for the types
// whose name alone does not say.
func valueHint(p Predicate) string {
	switch p.valueType() {
	case typeEnum:
		return ". One of: " + strings.Join(p.Allowed, ", ")
	case typeDate:
		return ". A date such as 2026-10-01, or an RFC3339 time"
	case typeRef:
		return ". The subject it refers to"
	}
	return ""
}

// dateOnly is a calendar day with no time of day.
const dateOnly = "2006-01-02"

// parseDate reads a date value: a calendar day or an RFC3339 instant.
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(dateOnly, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.ToUpper(s)); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// normalizeDate keeps a calendar day as written and renders an instant in
// UTC, so one moment stated in two zones is one value.
func normalizeDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	t, ok := parseDate(s)
	if !ok {
		return "", false
	}
	if len(s) == len(dateOnly) {
		return t.Format(dateOnly), true
	}
	return t.Format(time.RFC3339Nano), true
}

// normalizeValue enforces the predicate's declared value type. Deliberately no
// coercion: a number predicate rejects the string "8000" with an error naming
// the expected type, and the model corrects the call, the same self-repair
// loop the registry uses for unknown predicates.
func normalizeValue(predicate string, spec Predicate, v any) (any, error) {
	vt := spec.valueType()
	switch vt {
	case typeString, typeEnum, typeDate, typeRef:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s expects %s value, got %s", predicate, typeArticle(vt), describeValue(v))
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("value must not be empty")
		}
		switch vt {
		case typeEnum:
			for _, allowed := range spec.Allowed {
				if strings.EqualFold(allowed, s) {
					return allowed, nil
				}
			}
			return nil, fmt.Errorf("%s expects one of: %s; got %q", predicate, strings.Join(spec.Allowed, ", "), s)
		case typeDate:
			d, ok := normalizeDate(s)
			if !ok {
				return nil, fmt.Errorf("%s expects a date such as 2026-10-01 or an RFC3339 time, got %q", predicate, s)
			}
			return d, nil
		case typeRef:
			return normalizeSubject(s), nil
		}
		return s, nil
	case "number":
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("%s expects a number value, got %s", predicate, describeValue(v))
		}
		if !finite(f) {
			return nil, fmt.Errorf("%s expects a finite number value", predicate)
		}
		return f, nil
	default: // boolean
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s expects a boolean value, got %s", predicate, describeValue(v))
		}
		return b, nil
	}
}

// parseValue converts a value's string form to the predicate's declared type,
// for callers that address a belief by value rather than by id.
func (r Registry) parseValue(predicate, value string) (any, error) {
	spec, ok := r[predicate]
	if !ok {
		return value, nil
	}
	switch spec.ValueType {
	case "number":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || !finite(f) {
			return nil, fmt.Errorf("%s expects a number value, got %q", predicate, value)
		}
		return f, nil
	case "boolean":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("%s expects a boolean value, got %q", predicate, value)
		}
		return b, nil
	default:
		return value, nil
	}
}

func typeArticle(vt string) string {
	if vt == typeEnum {
		return "an enum"
	}
	return "a " + vt
}

// checkStored verifies that a stored value has the type its predicate stores.
// It is deliberately looser than normalizeValue: an enum that has since
// dropped a value, or a predicate that moved from string to date, must not
// make the events already written unreadable.
func checkStored(predicate string, spec Predicate, v any) error {
	var ok bool
	switch storageClass(spec.valueType()) {
	case typeNumber:
		_, ok = v.(float64)
	case typeBoolean:
		_, ok = v.(bool)
	default:
		_, ok = v.(string)
	}
	if !ok {
		return fmt.Errorf("%s expects %s value, got %s", predicate, typeArticle(spec.valueType()), describeValue(v))
	}
	return nil
}

func describeValue(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("string %q", x)
	case float64:
		return fmt.Sprintf("number %g", x)
	case bool:
		return fmt.Sprintf("boolean %t", x)
	case nil:
		return "nothing"
	default:
		return fmt.Sprintf("%T", v)
	}
}
