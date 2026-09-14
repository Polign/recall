package engine

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// DefaultRegistry returns an independent starter vocabulary. Applications may
// extend the returned map before constructing a Client.
func DefaultRegistry() Registry {
	r := Registry{}
	for name, description := range map[string]string{
		"prefers_editor": "Preferred text editor", "prefers_language": "Preferred programming language",
		"prefers_response_style": "Preferred answer style, such as concise or detailed",
		"prefers_test_framework": "Preferred testing framework", "prefers_package_manager": "Preferred package manager",
		"name": "Name the subject uses", "pronouns": "Pronouns the subject uses",
		"timezone": "Subject's timezone", "role": "Current professional or project role",
		"project_name": "Project's name", "repository_url": "Project's source repository URL",
		"project_status": "Current project status", "deployment_target": "Where the project is deployed",
	} {
		r[name] = Predicate{"single", "string", description}
	}
	r["uses_technology"] = Predicate{"multi", "string", "Technology used by the subject or project"}
	r["project_constraint"] = Predicate{"multi", "string", "An explicit project requirement or constraint"}
	return r
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
