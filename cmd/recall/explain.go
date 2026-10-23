package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Polign/recall"
)

// cmdExplain prints why each belief a question finds is held: the text it
// came from, the name it was written under, the definitions that decide how
// it folds, and its history. It reads only.
func cmdExplain(global *dbFlags, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	sub := addDatabaseFlags(fs)
	collection := fs.String("collection", setting("COLLECTION"), "memory collection (default $RECALL_COLLECTION, the saved setup's, then recall_lexical_v1)")
	dir := fs.String("config-dir", recallHome(), "the directory recall setup wrote; its saved setup is used when nothing else names a database")
	asJSON := fs.Bool("json", false, "print the explanations as JSON")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: recall [global flags] explain [options] <question>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fs.Usage()
		return errors.New("explain: a question is required")
	}
	mem, err := openCLIMemory(global, sub, *collection, *dir, "explain")
	if err != nil {
		return err
	}
	explanations, err := mem.client.Explain(context.Background(), question)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(explanations)
	}
	if len(explanations) == 0 {
		fmt.Fprintln(out, "Nothing remembered matches that question.")
		return nil
	}
	for i, e := range explanations {
		if i > 0 {
			fmt.Fprintln(out)
		}
		writeExplanation(out, e)
	}
	return nil
}

func writeExplanation(out io.Writer, e recall.Explanation) {
	fmt.Fprintln(out, e.Memory)
	stated := "  stated " + day(e.StatedAt) + " (" + e.Source + ")"
	if e.WrittenAs != "" {
		stated += ", written as " + e.WrittenAs
	}
	fmt.Fprintln(out, stated)
	if e.SourceText != "" {
		fmt.Fprintf(out, "  from: %q\n", e.SourceText)
	}
	if e.Evidence != "" {
		fmt.Fprintf(out, "  excerpt: %q\n", e.Evidence)
	}
	if len(e.Rules) > 0 {
		fmt.Fprintln(out, "  rules:")
		for _, r := range e.Rules {
			line := "    "
			if !r.At.IsZero() {
				line += day(r.At) + " "
			}
			line += r.Name + " " + r.Change
			if r.Cardinality != "" {
				line += ": " + r.Cardinality + "-valued " + r.ValueType
			}
			fmt.Fprintln(out, line)
		}
	}
	fmt.Fprintln(out, "  history:")
	for _, h := range e.History {
		value := fmt.Sprint(h.Value)
		if h.Retraction {
			value = "withdrawn " + value
			if h.Value == nil {
				value = "withdrawn, all values"
			}
		}
		fmt.Fprintf(out, "    %s %s\n", day(h.ObservedAt), value)
	}
}

// openCLIMemory opens memory for a one-shot read command: the database the
// flags, the environment or the saved setup name (database.go). When the
// saved setup decides, its collection, predicates and embedder apply too.
func openCLIMemory(global, sub *dbFlags, collection, dir, cmd string) (*memoryRuntime, error) {
	predicates := setting("PREDICATES")
	emb, err := envEmbedding()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	d, err := resolveDatabase(global, sub, dir, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	c, err := d.connect(context.Background())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	if d.fromSetup() {
		cfg := d.Setup
		if collection == "" {
			collection = cfg.Collection
		}
		predicates = cfg.Predicates
		if setting("EMBED_URL") == "" && setting("EMBED_MODEL") == "" {
			if emb, err = openEmbedding(cfg.EmbedModel, false); err != nil {
				return nil, fmt.Errorf("%s: saved embedder: %w", cmd, err)
			}
		}
	}
	if collection == "" {
		collection = defaultCollection(emb.spec)
	}
	return newMemoryRuntime(c, collection, predicates, emb.Embedder, false)
}
