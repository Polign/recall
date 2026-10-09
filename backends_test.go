package recall_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

type nopBackend struct{ url string }

func (nopBackend) Put(context.Context, string, string, []float32, map[string]any) error {
	return nil
}

func (nopBackend) List(context.Context, string, map[string]any, int) ([]recall.StoredVector, int, error) {
	return nil, 0, nil
}

func (nopBackend) Search(context.Context, string, []float32, int, map[string]any) ([]recall.Hit, error) {
	return nil, nil
}

func TestRegisterAndOpenBackend(t *testing.T) {
	recall.RegisterBackend("test-nop", recall.BackendDriver{
		Open:       func(o recall.BackendOptions) (recall.Backend, error) { return nopBackend{o.URL}, nil },
		DefaultURL: "http://default",
	})
	if !slices.Contains(recall.Backends(), "test-nop") {
		t.Fatalf("Backends() = %v", recall.Backends())
	}
	b, err := recall.OpenBackend("test-nop", recall.BackendOptions{})
	if err != nil || b.(nopBackend).url != "http://default" {
		t.Fatalf("open with no URL = %v, %v; want the default URL", b, err)
	}
	b, _ = recall.OpenBackend("test-nop", recall.BackendOptions{URL: "http://given"})
	if b.(nopBackend).url != "http://given" {
		t.Fatalf("a given URL was replaced: %v", b)
	}
	if _, err := recall.OpenBackend("no-such", recall.BackendOptions{}); err == nil || !strings.Contains(err.Error(), "test-nop") {
		t.Fatalf("unknown backend error %v should list the registered ones", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registering a name twice did not panic")
		}
	}()
	recall.RegisterBackend("test-nop", recall.BackendDriver{Open: func(recall.BackendOptions) (recall.Backend, error) { return nil, nil }})
}
