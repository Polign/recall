// amend demonstrates Recall as the memory of record for a support team: a
// chat agent and a voice agent share one memory of customer commitments and
// policies. A commitment is corrected once and both agents answer from the
// correction, a policy is withdrawn without being deleted, and an as-of read
// shows what the agents believed on the day a customer made a disputed
// request, quoting the turn the belief came from.
//
// Turns are scripted and their statements are fixed proposals, so the demo
// needs no model and gives the same answers on every run.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/polign"
)

const (
	customer = "customer-4812"
	policies = "policies"

	goodwillCredit = "goodwill credit: $25 for outages over 4 hours"
	refundWindow   = "refunds within 30 days of purchase"
)

// The history the seed beat replays. The correction and the withdrawal happen
// when the demo runs, so their timestamps are real.
var (
	policyMemoAt = time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	promisedAt   = time.Date(2026, 9, 12, 17, 4, 0, 0, time.UTC)
	requestedAt  = time.Date(2026, 9, 20, 15, 30, 0, 0, time.UTC)
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("server", "http://localhost:8080", "Polign server URL")
	collection := flag.String("collection", "amend-demo", "collection for this demo; use a new one for a fresh run")
	beat := flag.String("beat", "all", "all, seed, correct, withdraw, timetravel, or audit")
	pause := flag.Bool("pause", false, "with -beat all, wait for Enter between beats")
	auditOut := flag.String("audit-out", "amend-audit.json", "where the audit beat writes its bundle")
	flag.Parse()

	beats := map[string]func(context.Context, *demo) error{
		"seed": seed, "correct": correct, "withdraw": withdraw, "timetravel": timetravel,
		"audit": func(ctx context.Context, d *demo) error { return audit(ctx, d, *auditOut) },
	}
	order := []string{"seed", "correct", "withdraw", "timetravel", "audit"}
	if *beat != "all" {
		if beats[*beat] == nil {
			return fmt.Errorf("-beat must be all, %s", strings.Join(order, ", "))
		}
		order = []string{*beat}
	}

	registry, err := recall.StarterRegistry(recall.StarterSupport)
	if err != nil {
		return err
	}
	registry["refund_exception"] = recall.Predicate{Cardinality: "single", ValueType: "enum", Allowed: []string{"approved", "revoked"},
		Description: "Whether the customer's refund exception stands"}
	registry["policy"] = recall.Predicate{Cardinality: "multi", ValueType: "string",
		Description: "A customer-facing policy the company currently offers"}

	// Each agent gets its own backend and client, as separate services would.
	d := &demo{}
	for _, c := range []**recall.Client{&d.chat, &d.voice} {
		backend, err := polign.New(polign.Config{BaseURL: *endpoint, APIKey: os.Getenv("POLIGN_API_KEY")})
		if err != nil {
			return err
		}
		*c, err = recall.NewClient(recall.Config{Backend: backend, Collection: *collection, Registry: registry, Embedder: recall.LexicalEmbedder{}})
		if err != nil {
			return err
		}
	}

	in := bufio.NewReader(os.Stdin)
	for i, name := range order {
		if i > 0 && *pause {
			fmt.Print("\n[Enter for the next beat] ")
			in.ReadString('\n')
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := beats[name](ctx, d)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

type demo struct {
	chat, voice *recall.Client
}

// turn records one transcript turn. Every statement it files quotes the turn,
// and the whole turn is kept as the evidence those statements link to.
func turn(ctx context.Context, agent *recall.Client, at time.Time, text string, statements ...recall.Proposal) error {
	_, err := agent.RememberTextAt(ctx, text, recall.ProposedStatements(statements), at)
	return err
}

func seed(ctx context.Context, d *demo) error {
	header("Seed", "the history both agents share")

	memo := "Policy memo: customers get " + refundWindow + ". Customers also get a " + goodwillCredit + "."
	if err := turn(ctx, d.chat, policyMemoAt, memo,
		recall.Proposal{Subject: policies, Predicate: "policy", Value: refundWindow, Evidence: refundWindow},
		recall.Proposal{Subject: policies, Predicate: "policy", Value: goodwillCredit, Evidence: goodwillCredit},
	); err != nil {
		return err
	}
	step(policyMemoAt, "ops", "policy memo recorded: 2 policies")

	promise := "[chat, account 4812] Dana (support): I've approved a refund exception for your annual Pro plan. " +
		"You can use it past the 30-day window."
	if err := turn(ctx, d.chat, promisedAt, promise,
		recall.Proposal{Subject: customer, Predicate: "refund_exception", Value: "approved", Evidence: "I've approved a refund exception for your annual Pro plan"},
		recall.Proposal{Subject: customer, Predicate: "plan", Value: "Pro annual", Evidence: "your annual Pro plan"},
	); err != nil {
		return err
	}
	step(promisedAt, "chat", "Dana promises customer 4812 a refund exception")

	answer, err := exception(ctx, d.voice, requestedAt)
	if err != nil {
		return err
	}
	request := "[voice, account 4812] Customer: I'd like to use the refund exception Dana gave me on my annual plan."
	if err := turn(ctx, d.voice, requestedAt, request,
		recall.Proposal{Subject: customer, Predicate: "open_issue", Value: "refund requested under the 9/12 exception", Evidence: "I'd like to use the refund exception Dana gave me"},
	); err != nil {
		return err
	}
	step(requestedAt, "voice", "customer calls to use the exception; voice agent sees: "+answer)
	return nil
}

func correct(ctx context.Context, d *demo) error {
	header("Correct", "one agent records the correction, the other answers from it")

	at := time.Now().UTC()
	revoke := "[chat, account 4812] Priya (support lead): Revoking the refund exception for account 4812, effective today. It was granted outside policy."
	if err := turn(ctx, d.chat, at, revoke,
		recall.Proposal{Subject: customer, Predicate: "refund_exception", Value: "revoked", Evidence: "Revoking the refund exception for account 4812, effective today"},
	); err != nil {
		return err
	}
	step(at, "chat", "support lead revokes the exception")

	answer, err := exception(ctx, d.voice, time.Time{})
	if err != nil {
		return err
	}
	step(time.Now().UTC(), "voice", "asked \"can I still use my refund exception?\" and sees: "+answer)

	fmt.Println("\n  History of refund_exception for", customer+":")
	return printHistory(ctx, d.voice, customer, "refund_exception")
}

func withdraw(ctx context.Context, d *demo) error {
	header("Withdraw", "a discontinued policy leaves the beliefs and stays in the history")

	if _, err := d.chat.Forget(ctx, recall.ForgetRequest{Subject: policies, Predicate: "policy", Value: goodwillCredit}); err != nil {
		return err
	}
	step(time.Now().UTC(), "ops", "goodwill credit withdrawn")

	current, err := d.voice.Recall(ctx, recall.Query{Subject: policies, Predicate: "policy"})
	if err != nil {
		return err
	}
	fmt.Println("\n  Policies the voice agent can cite now:")
	for _, b := range current {
		fmt.Printf("    - %v\n", b.Value)
	}
	fmt.Println("\n  History of policy:")
	return printHistory(ctx, d.voice, policies, "policy")
}

func timetravel(ctx context.Context, d *demo) error {
	header("Timetravel", "the customer disputes the refund they asked for on "+day(requestedAt))

	then, err := d.voice.Recall(ctx, recall.Query{Subject: customer, Predicate: "refund_exception", AsOf: requestedAt})
	if err != nil {
		return err
	}
	if len(then) != 1 {
		return fmt.Errorf("expected one belief about the exception as of %s, got %d; run the seed beat first", day(requestedAt), len(then))
	}
	b := then[0]
	fmt.Printf("  As of %s, the agents believed: refund_exception = %v\n", requestedAt.Format("2006-01-02 15:04 MST"), b.Value)
	fmt.Printf("  True since:  %s\n", b.ObservedAt.Format("2006-01-02 15:04 MST"))
	fmt.Printf("  Quote:       %q\n", b.Evidence)
	if b.EvidenceID != "" {
		events, err := d.voice.Events(ctx, []string{b.EvidenceID})
		if err != nil {
			return err
		}
		if len(events) == 1 {
			fmt.Printf("  Whole turn:  %v\n", events[0].Value)
		}
	}

	now, err := d.voice.Recall(ctx, recall.Query{Subject: customer, Predicate: "refund_exception"})
	if err != nil {
		return err
	}
	if len(now) == 1 {
		fmt.Printf("\n  Today the agents believe: refund_exception = %v (since %s)\n", now[0].Value, now[0].ObservedAt.Format("2006-01-02 15:04 MST"))
	}
	fmt.Printf("\n  The request on %s was made while the exception stood, so the company honors it.\n", day(requestedAt))
	return nil
}

func audit(ctx context.Context, d *demo, path string) error {
	header("Audit", "what was believed about "+customer+" on "+day(requestedAt)+", as a file anyone can check")

	bundle, err := d.voice.ExportAudit(ctx, recall.AuditRequest{Scope: recall.AuditScope{Subject: customer}, AsOf: requestedAt})
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	// Replay checks the digest and folds the events with no server involved,
	// which is what a third party holding only the file can do.
	beliefs, err := bundle.Replay()
	if err != nil {
		return err
	}
	fmt.Printf("  %d events, digest %s\n", len(bundle.Events), bundle.Digest)
	fmt.Printf("  Written to %s\n\n", path)
	fmt.Printf("  Replayed from the file alone as of %s, digest verified:\n", requestedAt.Format("2006-01-02 15:04 MST"))
	for _, b := range beliefs {
		fmt.Printf("    %-17s %v\n", b.Predicate, b.Value)
	}
	fmt.Printf("\n  Check it yourself: go run ./cmd/recall-audit < %s\n", path)
	return nil
}

// exception is what an agent would tell the customer about their refund
// exception, read from memory as of at (zero means now).
func exception(ctx context.Context, agent *recall.Client, at time.Time) (string, error) {
	beliefs, err := agent.Recall(ctx, recall.Query{Subject: customer, Predicate: "refund_exception", AsOf: at})
	if err != nil {
		return "", err
	}
	if len(beliefs) == 0 {
		return "no exception on record", nil
	}
	return fmt.Sprintf("exception %v (since %s)", beliefs[0].Value, day(beliefs[0].ObservedAt)), nil
}

func printHistory(ctx context.Context, agent *recall.Client, subject, predicate string) error {
	events, err := agent.History(ctx, subject, predicate)
	if err != nil {
		return err
	}
	for _, e := range events {
		what := fmt.Sprintf("asserted  %v", e.Value)
		if e.Retraction {
			what = fmt.Sprintf("withdrawn %v", e.Value)
		}
		fmt.Printf("    %s  %s\n", e.ObservedAt.Format("2006-01-02 15:04"), what)
	}
	return nil
}

func header(name, about string) {
	fmt.Printf("\n== %s: %s\n\n", name, about)
}

func step(at time.Time, who, what string) {
	fmt.Printf("  %s  %-5s  %s\n", at.Format("2006-01-02 15:04"), who, what)
}

func day(t time.Time) string { return t.Format("Jan 2") }
