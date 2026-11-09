// The demo actor for Recall on Agent Substrate.
//
// It stands in for an agent. POST /v1/<operation> is passed to Recall with a
// placeholder Authorization header, which Substrate's egress gateway replaces
// with a JWT minted for this actor: the actor holds no key. POST /count
// increments a counter in process memory, to show what a snapshot keeps
// beside what Recall keeps.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	recallURL := strings.TrimRight(os.Getenv("RECALL_URL"), "/")
	if recallURL == "" {
		log.Fatal("RECALL_URL is unset")
	}
	// No keep-alives: Substrate does not keep open connections across a
	// suspend, so every call dials again.
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var count atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/count", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%d\n", count.Add(1))
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, recallURL+r.URL.Path, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// The gateway discards this value and puts the actor's JWT in its place.
		req.Header.Set("Authorization", "Bearer placeholder")
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	log.Fatal(http.ListenAndServe(":80", mux))
}
