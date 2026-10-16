package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Polign/recall"
	"github.com/Polign/recall/internal/recallsetup"
)

// cmdReembed copies a collection's event log into another collection,
// embedding each event with a new embedder. Nothing is extracted again and
// event IDs are kept, so beliefs, histories and as_of reads come out the
// same; only search changes. The source is left as it was.
func cmdReembed(c *api, urlGiven bool, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reembed", flag.ContinueOnError)
	from := fs.String("from", "", "collection to copy (default: the saved setup's, then "+defaultLexicalCollection+")")
	fromModel := fs.String("from-embed-model", "", "embedder that wrote -from (default: the saved setup's, then lexical)")
	embedModel := fs.String("embed-model", os.Getenv("POLIGN_EMBED_MODEL"), "embedder for the copy, as provider:model[@dim] (required)")
	to := fs.String("to", "", "collection to write; must be empty or hold vectors of this embedder (default: one named after -embed-model)")
	dir := fs.String("config-dir", recallHome(), "configuration written by recall setup, used when no server URL is given")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: recall [global flags] reembed -embed-model provider:model[@dim] [options]")
		fmt.Fprintln(fs.Output(), "Copies memory into a collection searched by meaning. Run against a saved setup, it also switches the setup to the copy.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("reembed: unexpected arguments: %v", fs.Args())
	}
	if *embedModel == "" {
		fs.Usage()
		return errors.New("reembed: -embed-model is required")
	}
	var saved *recallsetup.Config
	configPath := filepath.Join(*dir, "config.json")
	if !urlGiven && (c.backend == "" || c.backend == setupBackend) {
		var cfg recallsetup.Config
		if err := recallsetup.Read(configPath, &cfg); err == nil {
			if err := ensureRecallServer(*dir, cfg); err != nil {
				return err
			}
			conn, err := recallAPI(*dir, cfg)
			if err != nil {
				return err
			}
			conn.backend = c.backend
			c, saved = conn, &cfg
		}
	}
	if *from == "" {
		*from = defaultLexicalCollection
		if saved != nil {
			*from = saved.Collection
		}
	}
	if *fromModel == "" && saved != nil && *from == saved.Collection {
		*fromModel = saved.EmbedModel
	}
	src, err := openEmbedding(*fromModel, false)
	if err != nil {
		return fmt.Errorf("reembed: -from-embed-model: %w", err)
	}
	dst, err := openEmbedding(*embedModel, false)
	if err != nil {
		return fmt.Errorf("reembed: -embed-model: %w", err)
	}
	if *to == "" {
		*to = defaultCollection(dst.spec)
	}
	if *to == *from {
		return errors.New("reembed: -to must differ from -from; the source is kept as it was")
	}
	backend, err := openMemoryBackend(c)
	if err != nil {
		return err
	}
	n, err := reembed(context.Background(), backend, *from, src, *to, dst)
	if err != nil {
		return fmt.Errorf("reembed: %w", err)
	}
	fmt.Fprintf(out, "Copied %d events from %s (%s) to %s (%s).\n", n, *from, src.name(), *to, dst.name())
	if saved != nil && *from == saved.Collection {
		previous := *saved
		saved.Collection, saved.EmbedModel = *to, dst.spec
		if err := recallsetup.Write(configPath, *saved); err != nil {
			return err
		}
		fmt.Fprintf(out, "Setup now uses %s. The old collection stays; to go back: recall setup -collection %s -embed-model %s\n",
			*to, previous.Collection, embedName(previous.EmbedModel))
	}
	return nil
}

func embedName(spec string) string {
	if spec == "" {
		return "lexical"
	}
	return spec
}

// reembed copies from's event log into to with dst's embedder. It refuses a
// target that holds vectors of another size, and checks the first vector
// before writing the rest, so a wrong model fails after one call.
func reembed(ctx context.Context, b recall.Backend, from string, src embedding, to string, dst embedding) (int, error) {
	if _, _, err := fitEmbedding(ctx, b, from, src); err != nil {
		return 0, fmt.Errorf("source: %w", err)
	}
	if _, _, err := fitEmbedding(ctx, b, to, dst); err != nil {
		return 0, fmt.Errorf("target: %w", err)
	}
	reader, err := recall.NewClient(recall.Config{Backend: b, Collection: from, Embedder: src, Open: true})
	if err != nil {
		return 0, err
	}
	events, err := reader.ExportEvents(ctx, recall.Export{})
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", from, err)
	}
	if len(events) == 0 {
		return 0, fmt.Errorf("%s holds no memory to copy", from)
	}
	if dst.dim > 0 {
		v, err := dst.Embed(ctx, "recall reembed check")
		if err != nil {
			return 0, err
		}
		if len(v) != dst.dim {
			return 0, fmt.Errorf("%s returned %d dimensions, expected %d", dst.name(), len(v), dst.dim)
		}
	}
	writer, err := recall.NewClient(recall.Config{Backend: b, Collection: to, Embedder: dst, Open: true})
	if err != nil {
		return 0, err
	}
	return writer.ImportEvents(ctx, events)
}
