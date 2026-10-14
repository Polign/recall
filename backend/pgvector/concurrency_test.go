package pgvector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/backendtest"
)

// TestConcurrentFirstWrites starts several backends, as separate processes
// would be, writing to a collection that does not exist yet. One table must
// be created and every write must land.
func TestConcurrentFirstWrites(t *testing.T) {
	live(t)
	c := backendtest.Collection(t)
	const writers, each = 6, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		b := live(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := b.Put(context.Background(), c, fmt.Sprintf("w%d-%02d", w, i), []float32{1, float32(w), float32(i)}, map[string]any{"subject": "u"}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	_, total, err := live(t).List(context.Background(), c, nil, 1)
	if err != nil || total != writers*each {
		t.Fatalf("total %d, %v; want %d", total, err, writers*each)
	}
}

// TestLeaseRace has many holders acquire one lease at once; exactly one wins.
func TestLeaseRace(t *testing.T) {
	live(t)
	c := backendtest.Collection(t)
	const holders = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []recall.LeaseGrant
	)
	for h := range holders {
		b := live(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := b.AcquireLease(context.Background(), c, "agent", fmt.Sprintf("pod-%d", h), time.Minute)
			var held *recall.LeaseHeldError
			switch {
			case err == nil:
				mu.Lock()
				wins = append(wins, g)
				mu.Unlock()
			case errors.As(err, &held):
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(wins) != 1 || wins[0].Epoch != 1 {
		t.Fatalf("grants %+v, want exactly one at epoch 1", wins)
	}
}
