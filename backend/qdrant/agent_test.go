package qdrant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// TestUnleasedResume checks that an agent resumed without a lease keeps its
// working state, turns and pointers on Qdrant, and that a leased resume is
// refused, since Qdrant cannot grant leases.
func TestUnleasedResume(t *testing.T) {
	base := os.Getenv("RECALL_QDRANT_URL")
	if base == "" {
		t.Skip("set RECALL_QDRANT_URL to run against a Qdrant server")
	}
	b, err := New(Config{BaseURL: base, APIKey: os.Getenv("RECALL_QDRANT_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl, err := recall.NewClient(recall.Config{Backend: b, Collection: fmt.Sprintf("recall_unleased_%d", time.Now().UnixNano()), Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Resume(ctx, recall.ResumeRequest{AgentID: "a1"}); !errors.Is(err, recall.ErrLeaseUnsupported) {
		t.Fatalf("leased resume: %v, want ErrLeaseUnsupported", err)
	}
	a, err := cl.Resume(ctx, recall.ResumeRequest{AgentID: "a1", Unleased: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateWorkingState(ctx, recall.WorkingState{Goal: "ship it"}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := a.RecordTurn(ctx, recall.Turn{Role: "user", Content: fmt.Sprintf("turn %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.SetPointer(ctx, recall.Pointer{Name: "repo", Type: recall.PointerGitRef, Fields: map[string]string{"branch": "main"}}); err != nil {
		t.Fatal(err)
	}
	again, err := cl.Resume(ctx, recall.ResumeRequest{AgentID: "a1", Unleased: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.WorkingState().Goal; got != "ship it" {
		t.Fatalf("goal %q after resume", got)
	}
	if turns, err := again.RecentTurns(ctx, 10); err != nil || len(turns) != 3 {
		t.Fatalf("turns %d, %v; want 3", len(turns), err)
	}
	if ps, err := again.Pointers(ctx); err != nil || len(ps) != 1 {
		t.Fatalf("pointers %d, %v; want 1", len(ps), err)
	}
}
