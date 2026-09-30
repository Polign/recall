// resume shows an agent surviving a crash by resuming from its records, not from
// a process snapshot. Each run resumes the agent, prints the briefing it would
// hand the model, then works
// through a fixed task, recording its turns and working state as it goes.
// Pass -crash-at to kill the process mid-task without releasing its lease;
// the next run waits out the lease and continues from the last note.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/polign"
)

var task = []string{
	"list the call sites of billing.charge",
	"migrate the call sites in api/",
	"migrate the call sites in worker/",
	"run the test suite",
	"open a pull request",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("server", "http://localhost:23000", "Polign server URL")
	collection := flag.String("collection", "resume-example", "memory collection; agent records go in <collection>_agents")
	agentID := flag.String("agent", "coder-1", "agent to resume")
	crashAt := flag.Int("crash-at", 0, "exit without releasing the lease before this step (1-5); 0 finishes the task")
	ttl := flag.Duration("lease-ttl", 10*time.Second, "lease TTL; a crashed run blocks the next one this long")
	flag.Parse()

	backend, err := polign.New(polign.Config{BaseURL: *endpoint, APIKey: os.Getenv("POLIGN_API_KEY")})
	if err != nil {
		return err
	}
	client, err := recall.NewClient(recall.Config{Backend: backend, Collection: *collection,
		Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		return err
	}
	ctx := context.Background()

	agent, err := resume(ctx, client, *agentID, *ttl)
	if err != nil {
		return err
	}
	rc := agent.Resumed()
	fmt.Printf("--- briefing (epoch %d, %d tokens) ---\n%s---\n\n", rc.Epoch, rc.Tokens, rc.Briefing)

	ws := agent.WorkingState()
	if ws.Goal == "" {
		ws = recall.WorkingState{Goal: "migrate billing.charge to the v2 API", Plan: task}
	}
	for step := int(ws.Step) + 1; step <= len(task); step++ {
		if step == *crashAt {
			fmt.Printf("crashing before step %d, lease not released\n", step)
			os.Exit(3)
		}
		doing := task[step-1]
		if _, err := agent.RecordTurn(ctx, recall.Turn{Role: "assistant", Content: "Working on: " + doing}); err != nil {
			return err
		}
		ws.Step = int64(step)
		ws.Focus = doing
		ws.Plan = task[step:]
		ws.Progress = fmt.Sprintf("%d of %d steps done; last: %s", step, len(task), doing)
		if ws, err = agent.UpdateWorkingState(ctx, ws); err != nil {
			return err
		}
		fmt.Printf("step %d: %s\n", step, doing)
	}
	fmt.Println("task complete")
	return agent.Release(ctx)
}

// resume retries while a crashed run's lease is still live.
func resume(ctx context.Context, client *recall.Client, id string, ttl time.Duration) (*recall.Agent, error) {
	deadline := time.Now().Add(2 * ttl)
	for {
		agent, err := client.Resume(ctx, recall.ResumeRequest{AgentID: id, LeaseTTL: ttl})
		var held *recall.LeaseHeldError
		if errors.As(err, &held) && time.Now().Before(deadline) {
			fmt.Printf("waiting: %v\n", err)
			time.Sleep(ttl / 4)
			continue
		}
		return agent, err
	}
}
