package mongodb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/backendtest"
)

// live opens a backend on the server RECALL_MONGODB_URL names, such as
// mongodb://localhost:27017/?directConnection=true, and skips without one.
// Each test gets a database of its own, dropped afterwards.
func live(t *testing.T) *Backend {
	t.Helper()
	url := os.Getenv("RECALL_MONGODB_URL")
	if url == "" {
		t.Skip("set RECALL_MONGODB_URL to run against MongoDB")
	}
	db := fmt.Sprintf("recall_test_%d", time.Now().UnixNano()%1e12)
	b, err := New(Config{URL: url, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = b.Database().Drop(ctx)
		b.Close()
	})
	return b
}

// TestConformance runs the shared backend suite, including leases, the
// write fence, watermarks and text search.
func TestConformance(t *testing.T) {
	live(t)
	backendtest.Run(t, func(t *testing.T) recall.Backend { return live(t) })
}

// TestATakeoverCannotSlipBetweenCheckAndWrite pauses a fenced write after
// its lease check, lets the lease lapse, and tries a takeover in that window.
// With transactions the check is a write to the lease document, so the
// takeover waits for the fenced write to commit or abort: it can never
// finish first and then have the old holder's write land after it. The
// standalone case shows the window exists without transactions.
func TestATakeoverCannotSlipBetweenCheckAndWrite(t *testing.T) {
	b := live(t)
	ctx := t.Context()
	txn, err := b.Transactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const c = "race_agents"
	v := []float32{1, 0, 0, 0}
	g, err := b.AcquireLease(ctx, c, "a", "pod-a", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	took := make(chan error, 1)
	var tookFirst atomic.Bool
	var once sync.Once
	testHookFencedWrite = func() {
		once.Do(func() {
			time.Sleep(150 * time.Millisecond) // pod-a stalls; its lease lapses
			go func() {
				for {
					_, err := b.AcquireLease(context.Background(), c, "a", "pod-b", time.Minute)
					if !errors.Is(err, recall.ErrLeaseHeld) {
						took <- err
						return
					}
				}
			}()
			select {
			case err := <-took:
				tookFirst.Store(err == nil)
				took <- err
			case <-time.After(time.Second):
			}
		})
	}
	t.Cleanup(func() { testHookFencedWrite = nil })
	werr := b.PutFenced(ctx, c, "zombie", v, map[string]any{"by": "pod-a"}, recall.Fence{Lease: "a", Holder: "pod-a", Epoch: g.Epoch})
	if err := <-took; err != nil {
		t.Fatalf("takeover: %v", err)
	}
	slipped := werr == nil && tookFirst.Load()
	if txn && slipped {
		t.Fatal("the takeover finished between the lease check and the write, and the old holder's write still landed")
	}
	if !txn && !slipped {
		t.Fatalf("standalone: expected the unprotected window to show (write err %v, takeover first %v)", werr, tookFirst.Load())
	}
}
