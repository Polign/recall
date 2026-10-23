package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/Polign/recall"
	"github.com/Polign/recall/model"
)

// lexicalDim is the size of LexicalEmbedder's vectors (recall.LexicalSpace).
const lexicalDim = 256

// defaultLexicalCollection is where word-search memory lives when no
// collection is named, as it has since the first release.
const defaultLexicalCollection = "recall_lexical_v1"

// embedding is how a memory runtime turns text into vectors. Pass its
// Embedder field, never the embedding itself, wherever a recall.Embedder is
// wanted: the engine recognizes recall.LexicalEmbedder by type to rank word
// search keyword-first, and a wrapper hides it.
type embedding struct {
	recall.Embedder
	// spec names the vector space, as model.ParseEmbed reads it; "" is
	// Recall's built-in lexical hashing.
	spec string
	// dim is the size of the vectors, or 0 when the model is not one Recall
	// knows the size of.
	dim int
	// derived means the embedder followed the extraction model rather than
	// being named, so an existing collection's own vectors take precedence.
	derived bool
}

var lexical = embedding{Embedder: recall.LexicalEmbedder{}, dim: lexicalDim}

func (e embedding) name() string {
	if e.spec == "" {
		return "lexical"
	}
	return e.spec
}

// chooseEmbedding works out the embedder from the flags: -embed-url (a
// service that speaks Recall's own {text} -> {values} contract), else
// -embed-model, else the one that follows -extract-model, else lexical.
func chooseEmbedding(embedURL, embedModel, extractModel string) (embedding, error) {
	if embedURL != "" {
		if embedModel != "" {
			return embedding{}, errors.New("-embed-url and -embed-model name different embedders; pass one")
		}
		return embedding{Embedder: newRemoteEmbedder(embedURL, 0), spec: "url:" + embedURL}, nil
	}
	if embedModel != "" {
		return openEmbedding(embedModel, false)
	}
	return openEmbedding(model.EmbedFor(extractModel), true)
}

// openEmbedding builds the embedder a spec names; "" and "lexical" mean the
// built-in lexical hashing.
func openEmbedding(spec string, derived bool) (embedding, error) {
	if spec == "" || strings.EqualFold(spec, "lexical") {
		return lexical, nil
	}
	cfg, err := model.ParseEmbed(spec)
	if err != nil {
		return embedding{}, err
	}
	// A local model's first download is reported on stderr, which MCP
	// clients log and setup shows.
	e, err := model.OpenEmbedding(cfg, os.Stderr)
	if err != nil {
		return embedding{}, err
	}
	return embedding{Embedder: e, spec: e.Spec(), dim: e.Dim(), derived: derived}, nil
}

// defaultCollection is the collection an embedder writes when none is
// named. Each vector space gets its own, since vectors from different
// embedders cannot be searched together.
func defaultCollection(spec string) string {
	if spec == "" {
		return defaultLexicalCollection
	}
	var b strings.Builder
	b.WriteString("recall_")
	underscore := false
	for _, r := range strings.ToLower(spec) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			underscore = false
		} else if !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}
	return strings.TrimRight(b.String(), "_") + "_v1"
}

// openMemoryBackend opens the database c names; Open makes no request.
func openMemoryBackend(c *api) (recall.Backend, error) {
	name := c.backend
	if name == "" {
		name = setupBackend
	}
	return recall.OpenBackend(name, recall.BackendOptions{URL: c.base, APIKey: c.key, Params: c.params})
}

// storedDim is the size of the vectors already in collection, or 0 when it
// holds none or does not exist yet.
func storedDim(ctx context.Context, b recall.Backend, collection string) (int, error) {
	got, _, err := b.List(ctx, collection, nil, 1)
	if err != nil {
		var nf interface{ NotFound() bool }
		if errors.As(err, &nf) && nf.NotFound() {
			return 0, nil
		}
		return 0, err
	}
	if len(got) == 0 {
		return 0, nil
	}
	return len(got[0].Values), nil
}

// fitEmbedding checks emb against the vectors collection already holds, so
// a search never compares vectors from two embedders. An embedder that only
// followed the extraction model gives way to a collection of lexical
// vectors: that memory keeps working with word search, and a note says how
// to move it. A named embedder that does not fit is an error. When the
// database cannot be read yet, the check is skipped; the first write or
// search reports that failure.
func fitEmbedding(ctx context.Context, b recall.Backend, collection string, emb embedding) (embedding, string, error) {
	dim, err := storedDim(ctx, b, collection)
	if err != nil || dim == 0 || emb.dim == 0 || dim == emb.dim {
		return emb, "", nil
	}
	if emb.derived && dim == lexicalDim {
		return lexical, fmt.Sprintf("collection %s holds lexical vectors, so it keeps word search. "+
			"To search it by meaning with %s, copy it: recall reembed -from %s -to %s -embed-model %s",
			collection, emb.spec, collection, defaultCollection(emb.spec), emb.spec), nil
	}
	held := "lexical vectors"
	if dim != lexicalDim {
		held = fmt.Sprintf("%d-dimensional vectors", dim)
	}
	return emb, "", fmt.Errorf("collection %s holds %s and %s makes %d-dimensional ones; name the embedder "+
		"that wrote it with -embed-model (lexical for word search), or copy it with recall reembed", collection, held, emb.name(), emb.dim)
}

// envEmbedding is the embedder the environment names for commands that only
// read, such as explain: POLIGN_EMBED_URL, else POLIGN_EMBED_MODEL, else
// lexical. It never follows an extraction model, because reading has none.
func envEmbedding() (embedding, error) {
	return chooseEmbedding(setting("EMBED_URL"), setting("EMBED_MODEL"), "")
}
