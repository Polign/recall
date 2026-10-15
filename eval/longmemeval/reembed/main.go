// Command reembed copies a question's Recall store from the lexical collection
// into a model-embedder collection, so a run with LME_EMBED_URL reuses the
// facts an earlier run extracted instead of extracting them again.
//
//	go run ./eval/longmemeval/reembed -url http://127.0.0.1:23001 -key-file STORE/local-key \
//	    -embed-url http://127.0.0.1:8766
//
// The store's polign-server must be running (recall setup -local starts it).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Polign/recall"
	_ "github.com/Polign/recall/polign"
)

func main() {
	url := flag.String("url", "", "the store's polign-server")
	keyFile := flag.String("key-file", "", "file holding the store's API key")
	from := flag.String("from", "recall_lexical_v1", "source collection")
	to := flag.String("to", "recall_embed_v1", "destination collection")
	embedURL := flag.String("embed-url", "", "embedding service: POST {text} answers {values}")
	flag.Parse()
	if err := run(*url, *keyFile, *from, *to, *embedURL); err != nil {
		fmt.Fprintln(os.Stderr, "reembed:", err)
		os.Exit(1)
	}
}

func run(url, keyFile, from, to, embedURL string) error {
	ctx := context.Background()
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	backend, err := recall.OpenBackend("polign", recall.BackendOptions{URL: url, APIKey: strings.TrimSpace(string(key))})
	if err != nil {
		return err
	}
	src, err := recall.NewClient(recall.Config{Backend: backend, Collection: from, Embedder: recall.LexicalEmbedder{}, Open: true})
	if err != nil {
		return err
	}
	events, err := src.ExportEvents(ctx, recall.Export{})
	if err != nil {
		return err
	}
	httpc := &http.Client{Timeout: 60 * time.Second}
	embed := recall.EmbedFunc(func(ctx context.Context, text string) ([]float32, error) {
		body, _ := json.Marshal(map[string]string{"text": text})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(embedURL, "/")+"/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := httpc.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("embed: HTTP %d", res.StatusCode)
		}
		var out struct {
			Values []float32 `json:"values"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return nil, err
		}
		return out.Values, nil
	})
	dst, err := recall.NewClient(recall.Config{Backend: backend, Collection: to, Embedder: embed, Open: true})
	if err != nil {
		return err
	}
	n, err := dst.ImportEvents(ctx, events)
	if err != nil {
		return err
	}
	fmt.Printf("copied %d events from %s to %s\n", n, from, to)
	return nil
}
