package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Polign/recall"
	"github.com/Polign/recall/internal/recallsetup"
)

// cmdExplain prints why each belief a question finds is held: the text it
// came from, the name it was written under, the definitions that decide how
// it folds, and its history. It reads only.
func cmdExplain(c *api, urlGiven bool, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	collection := fs.String("collection", os.Getenv("POLIGN_COLLECTION"), "memory collection (default $POLIGN_COLLECTION, the saved setup's, then recall_lexical_v1)")
	dir := fs.String("config-dir", recallHome(), "configuration written by recall setup, used when no server URL is given")
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
	predicates := os.Getenv("POLIGN_PREDICATES")
	if !urlGiven && c.backend == setupBackend || !urlGiven && c.backend == "" {
		var cfg recallsetup.Config
		if err := recallsetup.Read(filepath.Join(*dir, "config.json"), &cfg); err == nil {
			saved, err := recallAPI(*dir, cfg)
			if err != nil {
				return err
			}
			saved.backend = c.backend
			c = saved
			if *collection == "" {
				*collection = cfg.Collection
			}
			predicates = cfg.Predicates
		}
	}
	if *collection == "" {
		*collection = "recall_lexical_v1"
	}
	mem, err := newMemoryRuntime(c, *collection, predicates, os.Getenv("POLIGN_EMBED_URL"), false)
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
