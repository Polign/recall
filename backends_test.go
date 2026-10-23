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

type paramBackend struct {
	url    string
	params map[string]string
}

func (paramBackend) Put(context.Context, string, string, []float32, map[string]any) error {
	return nil
}

func (paramBackend) List(context.Context, string, map[string]any, int) ([]recall.StoredVector, int, error) {
	return nil, 0, nil
}

func (paramBackend) Search(context.Context, string, []float32, int, map[string]any) ([]recall.Hit, error) {
	return nil, nil
}

func TestOpenBackendMovesListedQueryKeysIntoParams(t *testing.T) {
	recall.RegisterBackend("test-params", recall.BackendDriver{
		Open:   func(o recall.BackendOptions) (recall.Backend, error) { return paramBackend{o.URL, o.Params}, nil },
		Params: []string{"schema", "text_search"},
	})
	b, err := recall.OpenBackend("test-params", recall.BackendOptions{URL: "postgres://u@h/db?schema=memory&sslmode=require&text_search=simple"})
	if err != nil {
		t.Fatal(err)
	}
	got := b.(paramBackend)
	if got.url != "postgres://u@h/db?sslmode=require" {
		t.Fatalf("url = %q; want only the driver's own key left", got.url)
	}
	if got.params["schema"] != "memory" || got.params["text_search"] != "simple" || len(got.params) != 2 {
		t.Fatalf("params = %v", got.params)
	}
	// Nothing to move: the URL is passed through untouched, query and all.
	b, _ = recall.OpenBackend("test-params", recall.BackendOptions{URL: "postgres://u@h/db?sslmode=require"})
	if b.(paramBackend).url != "postgres://u@h/db?sslmode=require" || b.(paramBackend).params != nil {
		t.Fatalf("untouched URL changed: %#v", b)
	}
	// Params given directly win over the query string.
	b, _ = recall.OpenBackend("test-params", recall.BackendOptions{URL: "postgres://h/db?schema=a", Params: map[string]string{"schema": "b"}})
	if b.(paramBackend).params["schema"] != "b" {
		t.Fatalf("explicit param lost to the query: %v", b.(paramBackend).params)
	}
	if _, err := recall.OpenBackend("test-params", recall.BackendOptions{URL: "postgres://h/db?schema=a&schema=b"}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("a key given twice was accepted: %v", err)
	}
}
