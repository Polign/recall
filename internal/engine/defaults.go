package engine

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// DefaultRegistry returns an independent starter vocabulary, the one for
// coding agents. Applications may extend the returned map before constructing
// a Client. StarterRegistry has the others.
func DefaultRegistry() Registry {
	r, _ := StarterRegistry(StarterCoding)
	return r
}

// Starter registries, by the kind of agent each is written for.
const (
	StarterCoding  = "coding"
	StarterSupport = "support"
	StarterSales   = "sales"
	StarterVoice   = "voice"
)

// StarterNames lists the starter registries StarterRegistry knows.
func StarterNames() []string {
	return []string{StarterCoding, StarterSales, StarterSupport, StarterVoice}
}

// StarterRegistry returns an independent copy of a starter vocabulary: a
// registry that is useful for one kind of agent before anyone has written
// their own. It is a starting point, to be extended or trimmed, and it always
// includes the note predicate.
func StarterRegistry(name string) (Registry, error) {
	build, ok := starters[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("recall: no starter registry %q; the starters are: %s", name, strings.Join(StarterNames(), ", "))
	}
	r := build()
	r[NotePredicate] = notePredicate
	return r, nil
}

func one(valueType, description string) Predicate {
	return Predicate{Cardinality: "single", ValueType: valueType, Description: description}
}

func many(valueType, description string) Predicate {
	return Predicate{Cardinality: "multi", ValueType: valueType, Description: description}
}

func oneOf(description string, allowed ...string) Predicate {
	return Predicate{Cardinality: "single", ValueType: typeEnum, Description: description, Allowed: allowed}
}

var starters = map[string]func() Registry{
	// What a coding agent needs to stop asking twice: who it works with, how
	// they like to work, and the project's fixed points.
	StarterCoding: func() Registry {
		return Registry{
			"prefers_editor":          one(typeString, "Preferred text editor"),
			"prefers_language":        one(typeString, "Preferred programming language"),
			"prefers_response_style":  one(typeString, "Preferred answer style, such as concise or detailed"),
			"prefers_test_framework":  one(typeString, "Preferred testing framework"),
			"prefers_package_manager": one(typeString, "Preferred package manager"),
			"name":                    one(typeString, "Name the subject uses"),
			"pronouns":                one(typeString, "Pronouns the subject uses"),
			"timezone":                one(typeString, "Subject's timezone"),
			"role":                    one(typeString, "Current professional or project role"),
			"project_name":            one(typeString, "Project's name"),
			"repository_url":          one(typeString, "Project's source repository URL"),
			"project_status":          one(typeString, "Current project status"),
			"deployment_target":       one(typeString, "Where the project is deployed"),
			"uses_technology":         many(typeString, "Technology used by the subject or project"),
			"project_constraint":      many(typeString, "An explicit project requirement or constraint"),
		}
	},
	// A support agent's subject is a customer. The facts that change are the
	// ones worth typing: the plan, who owns the case, what is still open.
	StarterSupport: func() Registry {
		return Registry{
			"name":               one(typeString, "Name the customer uses"),
			"preferred_language": one(typeString, "Language the customer wants to be answered in"),
			"timezone":           one(typeString, "Customer's timezone"),
			"contact_channel":    oneOf("How the customer wants to be contacted", "email", "phone", "chat", "sms"),
			"company":            one(typeRef, "Organization the customer belongs to"),
			"account_id":         one(typeString, "Customer's account identifier"),
			"plan":               one(typeString, "Plan or tier the customer is on"),
			"renewal_date":       one(typeDate, "When the customer's plan renews"),
			"sentiment":          oneOf("How the customer feels about the service right now", "positive", "neutral", "frustrated"),
			"escalated_to":       one(typeRef, "Person or team now handling the customer's case"),
			"uses_product":       many(typeString, "Product or feature the customer uses"),
			"open_issue":         many(typeString, "A problem the customer reported that is not resolved"),
			"resolved_issue":     many(typeString, "A problem the customer reported that has been resolved"),
			"promised":           many(typeString, "Something the customer was told would be done"),
		}
	},
	// A sales agent keeps two kinds of subject, people and accounts, and the
	// refs between them are most of what it needs to remember.
	StarterSales: func() Registry {
		return Registry{
			"name":             one(typeString, "Name the contact uses"),
			"title":            one(typeString, "Contact's job title"),
			"works_at":         one(typeRef, "Account the contact works at"),
			"reports_to":       one(typeRef, "Person the contact reports to"),
			"champion":         one(typeRef, "Person inside the account who wants the deal to happen"),
			"decision_maker":   many(typeRef, "Person who has to approve the deal"),
			"deal_stage":       oneOf("Where the deal stands", "lead", "qualified", "proposal", "negotiation", "won", "lost"),
			"deal_value":       one(typeNumber, "Expected value of the deal, in the account's currency"),
			"close_date":       one(typeDate, "When the deal is expected to close"),
			"last_contacted":   one(typeDate, "When the contact or account was last spoken to"),
			"budget_confirmed": one(typeBoolean, "Whether the account has confirmed it has budget"),
			"next_step":        one(typeString, "What was agreed to happen next"),
			"pain_point":       many(typeString, "A problem the account wants solved"),
			"objection":        many(typeString, "A reason the account gave for not buying"),
			"competitor":       many(typeString, "Another vendor the account is considering or using"),
			"uses_technology":  many(typeString, "Technology the account uses"),
		}
	},
	// A voice agent meets the same caller again with no screen to scroll
	// back through, so it remembers how to address them and why they call.
	StarterVoice: func() Registry {
		return Registry{
			"name":                   one(typeString, "Name the caller wants to be called"),
			"pronouns":               one(typeString, "Pronouns the caller uses"),
			"preferred_language":     one(typeString, "Language the caller wants to speak"),
			"timezone":               one(typeString, "Caller's timezone"),
			"callback_number":        one(typeString, "Number to call the caller back on"),
			"preferred_contact_time": one(typeString, "When the caller wants to be called"),
			"prefers_response_style": one(typeString, "How the caller wants to be spoken to, such as brief or step by step"),
			"reason_for_calling":     one(typeString, "Why the caller is calling, as they last put it"),
			"appointment_time":       one(typeDate, "When the caller's next appointment is"),
			"consent_to_record":      one(typeBoolean, "Whether the caller agreed to the call being recorded"),
			"calling_on_behalf_of":   one(typeRef, "Person the caller is calling for"),
			"open_request":           many(typeString, "Something the caller asked for that is not done yet"),
			"accessibility_need":     many(typeString, "Something the caller needs the agent to accommodate"),
		}
	},
}

// LexicalEmbedder is a dependency-free signed feature-hashing fallback. It
// retrieves overlapping words, not semantic synonyms. Use a dedicated collection
// for this versioned 256-dimensional space; never mix it with model embeddings.
type LexicalEmbedder struct{}

const LexicalSpace = "recall-lexical-v1-256"

func (LexicalEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v := make([]float32, 256)
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(word))
		n := h.Sum64()
		if n>>63 == 0 {
			v[n%256]++
		} else {
			v[n%256]--
		}
	}
	var norm float64
	for _, x := range v {
		norm += float64(x * x)
	}
	if norm == 0 {
		v[0] = 1
	} else {
		for i := range v {
			v[i] /= float32(math.Sqrt(norm))
		}
	}
	return v, ctx.Err()
}
