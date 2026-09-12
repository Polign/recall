// sessions demonstrates exact memory across separate client processes. Its
// constant fixture vector needs no model; use a real embedder for semantic recall.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/polign"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("server", "http://localhost:8080", "Polign server URL")
	collection := flag.String("collection", "recall-example", "collection for this example")
	subject := flag.String("subject", "example-user", "subject for this example")
	action := flag.String("action", "inspect", "seed, inspect, or forget")
	flag.Parse()
	if *action != "seed" && *action != "inspect" && *action != "forget" {
		return fmt.Errorf("action must be seed, inspect, or forget")
	}
	backend, err := polign.New(polign.Config{BaseURL: *endpoint, APIKey: os.Getenv("POLIGN_API_KEY")})
	if err != nil {
		return err
	}
	cfg := recall.Config{Backend: backend, Collection: *collection, Registry: recall.Registry{
		"prefers_editor": {Cardinality: "single", ValueType: "string", Description: "preferred editor"},
	}}
	if *action != "inspect" {
		cfg.Embedder = recall.EmbedFunc(func(context.Context, string) ([]float32, error) { return []float32{1, 0, 0}, nil })
	}
	client, err := recall.NewClient(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch *action {
	case "seed":
		for _, value := range []string{"vim", "neovim"} {
			if _, err := client.Remember(ctx, recall.RememberRequest{Subject: *subject, Predicate: "prefers_editor", Value: value}); err != nil {
				return err
			}
		}
	case "forget":
		if _, err := client.Forget(ctx, recall.ForgetRequest{Subject: *subject, Predicate: "prefers_editor", All: true}); err != nil {
			return err
		}
	}
	events, err := client.History(ctx, *subject, "prefers_editor")
	if err != nil {
		return err
	}
	current, err := client.Recall(ctx, recall.Query{Subject: *subject, Predicate: "prefers_editor"})
	if err != nil {
		return err
	}
	var first []recall.Belief
	if len(events) > 0 {
		first, err = client.Recall(ctx, recall.Query{Subject: *subject, Predicate: "prefers_editor", AsOf: events[0].ObservedAt})
		if err != nil {
			return err
		}
	}
	out := struct {
		Current []recall.Belief `json:"current"`
		First   []recall.Belief `json:"at_first_event"`
		History []recall.Event  `json:"history"`
	}{current, first, events}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
