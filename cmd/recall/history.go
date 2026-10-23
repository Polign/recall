package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall"
)

// The correction commands read the log as a record: how a belief changed
// (history), what was believed at an instant (ask -as-of), and what changed
// between two instants (diff). They read only.

func readFlags(name, usage string) (*flag.FlagSet, *dbFlags, *string, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	sub := addDatabaseFlags(fs)
	collection := fs.String("collection", setting("COLLECTION"), "memory collection (default $RECALL_COLLECTION, the saved setup's, then recall_lexical_v1)")
	dir := fs.String("config-dir", recallHome(), "the directory recall setup wrote; its saved setup is used when nothing else names a database")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: recall "+name+" "+usage)
		fs.PrintDefaults()
	}
	return fs, sub, collection, dir, asJSON
}

func parseFlags(fs *flag.FlagSet, args []string) (bool, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// cmdHistory prints how a belief changed: every statement about it, oldest
// first, each with what it began and ended believing.
func cmdHistory(global *dbFlags, args []string, out io.Writer) error {
	fs, sub, collection, dir, asJSON := readFlags("history", "[options] <question> | -subject S -predicate P")
	subject := fs.String("subject", "", "with -predicate: the subject whose history to print, instead of a question")
	predicate := fs.String("predicate", "", "with -subject: the predicate whose history to print")
	if ok, err := parseFlags(fs, args); !ok {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if (*subject == "") != (*predicate == "") {
		return errors.New("history: give -subject and -predicate together")
	}
	if question == "" && *subject == "" {
		fs.Usage()
		return errors.New("history: a question, or -subject and -predicate, is required")
	}
	mem, err := openCLIMemory(global, sub, *collection, *dir, "history")
	if err != nil {
		return err
	}
	ctx := context.Background()
	var timelines []recall.Timeline
	if *subject != "" {
		t, err := mem.client.Timeline(ctx, *subject, *predicate)
		if err != nil {
			return err
		}
		timelines = []recall.Timeline{t}
	} else if timelines, err = mem.client.TimelinesFor(ctx, question); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, timelines)
	}
	if len(timelines) == 0 || len(timelines) == 1 && len(timelines[0].Steps) == 0 {
		fmt.Fprintln(out, "Nothing remembered matches that.")
		return nil
	}
	for i, t := range timelines {
		if i > 0 {
			fmt.Fprintln(out)
		}
		writeTimeline(out, t)
	}
	return nil
}

func writeTimeline(out io.Writer, t recall.Timeline) {
	fmt.Fprintf(out, "%s %s\n", t.Subject, strings.ReplaceAll(t.Predicate, "_", " "))
	if len(t.Now) == 0 {
		fmt.Fprintln(out, "  believed now: nothing")
	}
	for _, n := range t.Now {
		fmt.Fprintln(out, "  believed now: "+n)
	}
	for _, s := range t.Steps {
		head := "  " + minute(s.At) + "  " + s.Change
		if s.Source != "" {
			head += ", " + s.Source
		}
		fmt.Fprintln(out, head)
		for _, r := range s.Removed {
			fmt.Fprintln(out, "      - "+r)
		}
		for _, a := range s.Added {
			fmt.Fprintln(out, "      + "+a)
		}
		if len(s.Added)+len(s.Removed) == 0 || s.Change == "withdrawn" || s.Predicate != t.Predicate {
			fmt.Fprintln(out, "      by: "+s.Statement)
		}
		if s.Evidence != "" {
			fmt.Fprintf(out, "      said: %q\n", s.Evidence)
		}
	}
}

// cmdAsk prints what is believed, now or at an earlier instant, each with
// what it replaced.
func cmdAsk(global *dbFlags, args []string, out io.Writer) error {
	fs, sub, collection, dir, asJSON := readFlags("ask", "[options] [question]")
	asOf := fs.String("as-of", "", "answer as of this instant: RFC3339, a date (2026-10-01), or a time ago (36h, 7d)")
	since := fs.String("since", "", "only what was stated after this instant, newest first")
	if ok, err := parseFlags(fs, args); !ok {
		return err
	}
	at, err := parseWhen("as-of", *asOf, time.Now())
	if err != nil {
		return err
	}
	after, err := parseWhen("since", *since, time.Now())
	if err != nil {
		return err
	}
	mem, err := openCLIMemory(global, sub, *collection, *dir, "ask")
	if err != nil {
		return err
	}
	memories, err := mem.client.AskWith(context.Background(), recall.AskRequest{Question: strings.Join(fs.Args(), " "), AsOf: at, Since: after})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, memories)
	}
	if !at.IsZero() {
		fmt.Fprintf(out, "Believed as of %s:\n", minute(at))
	}
	if len(memories) == 0 {
		fmt.Fprintln(out, "Nothing remembered matches that.")
		return nil
	}
	for _, m := range memories {
		fmt.Fprintf(out, "%s  (since %s)\n", m.Text, day(m.Since))
		for _, b := range m.Before {
			fmt.Fprintf(out, "  replaced: %s  (since %s)\n", b.Text, day(b.Since))
		}
		for _, e := range m.Elsewhere {
			fmt.Fprintf(out, "  elsewhere: %s  (since %s)\n", e.Text, day(e.Since))
		}
	}
	return nil
}

// cmdDiff prints what was believed differently at one instant than at
// another.
func cmdDiff(global *dbFlags, args []string, out io.Writer) error {
	fs, sub, collection, dir, asJSON := readFlags("diff", "-from T [-to T] [-subject S]")
	from := fs.String("from", "", "the earlier instant: RFC3339, a date (2026-10-01), or a time ago (36h, 7d); required")
	to := fs.String("to", "", "the later instant, in the same forms (default now)")
	subject := fs.String("subject", "", "only this subject (default every subject)")
	if ok, err := parseFlags(fs, args); !ok {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("diff: unexpected argument %q", fs.Arg(0))
	}
	if *from == "" {
		fs.Usage()
		return errors.New("diff: -from is required")
	}
	now := time.Now()
	f, err := parseWhen("from", *from, now)
	if err != nil {
		return err
	}
	t, err := parseWhen("to", *to, now)
	if err != nil {
		return err
	}
	mem, err := openCLIMemory(global, sub, *collection, *dir, "diff")
	if err != nil {
		return err
	}
	changes, err := mem.client.Diff(context.Background(), recall.DiffRequest{Subject: *subject, From: f, To: t})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, changes)
	}
	end := "now"
	if !t.IsZero() {
		end = minute(t)
	}
	if len(changes) == 0 {
		fmt.Fprintf(out, "Nothing changed between %s and %s.\n", minute(f), end)
		return nil
	}
	fmt.Fprintf(out, "Changed between %s and %s:\n", minute(f), end)
	for _, ch := range changes {
		for _, r := range ch.Removed {
			fmt.Fprintln(out, "- "+r)
		}
		for _, a := range ch.Added {
			fmt.Fprintln(out, "+ "+a)
		}
	}
	return nil
}

// parseWhen reads an instant as RFC3339, a date (midnight UTC), or a time
// before now such as 36h or 7d. Empty is the zero time.
func parseWhen(name, v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && strings.HasSuffix(v, "d") && n >= 0 {
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("-%s %q: want RFC3339 (2026-10-01T09:00:00Z), a date (2026-10-01), or a time ago (36h, 7d)", name, v)
}

func minute(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") }

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
