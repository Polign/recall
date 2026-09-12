package recall

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	predicateName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	validSources  = map[string]bool{"user_stated": true, "agent_inferred": true, "tool_result": true}
	validKinds    = map[string]bool{"fact": true, "preference": true}
)

// Predicate is one registry entry. Cardinality decides what a second value
// for the same subject means: "single" makes a newer value supersede the old
// one, "multi" makes it an additional fact. ValueType decides what a value
// IS: a string, a number, or a boolean. Values are stored with that type, so
// a number compares numerically in recall filters instead of lexically.
type Predicate struct {
	Cardinality string `json:"cardinality"`
	ValueType   string `json:"value_type"`
	Description string `json:"description"`
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
		out[name] = p
	}
	return out
}

// Validate checks the names, cardinalities, and value types of a registry.
func (r Registry) Validate() error {
	for name, p := range r {
		if !predicateName.MatchString(name) {
			return fmt.Errorf("predicate registry: %q is not snake_case", name)
		}
		if p.Cardinality != "single" && p.Cardinality != "multi" {
			return fmt.Errorf("predicate registry: %q has cardinality %q, want single or multi", name, p.Cardinality)
		}
		switch p.ValueType {
		case "", "string", "number", "boolean":
		default:
			return fmt.Errorf("predicate registry: %q has value_type %q, want string, number, or boolean", name, p.ValueType)
		}
	}
	return nil
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
		vt := p.ValueType
		if vt == "" {
			vt = "string"
		}
		fmt.Fprintf(&b, "- %s (%s-valued %s): %s\n", name, p.Cardinality, vt, p.Description)
	}
	return b.String()
}

// normalizeValue enforces the predicate's declared value type. Deliberately no
// coercion: a number predicate rejects the string "8000" with an error naming
// the expected type, and the model corrects the call, the same self-repair
// loop the registry uses for unknown predicates.
func normalizeValue(predicate string, spec Predicate, v any) (any, error) {
	vt := spec.ValueType
	if vt == "" {
		vt = "string"
	}
	switch vt {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s expects a string value, got %s", predicate, describeValue(v))
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("value must not be empty")
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
