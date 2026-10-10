// Command predicate-matching measures how well an embedder tells predicate
// names that mean the same relation from names that do not, the comparison
// an open Recall client makes before defining a new predicate. It prints each
// pair's cosine and, per threshold, how many same-relation pairs would merge
// and how many different-relation pairs would merge by mistake.
//
//	go run ./eval/predicate-matching -ollama http://localhost:11434 -model nomic-embed-text
//	go run ./eval/predicate-matching -lexical
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/Polign/recall"
)

func main() {
	log.SetFlags(0)
	ollama := flag.String("ollama", "http://localhost:11434", "Ollama base URL")
	model := flag.String("model", "nomic-embed-text", "Ollama embedding model")
	lexical := flag.Bool("lexical", false, "use Recall's built-in lexical embedder instead of Ollama")
	pairsPath := flag.String("pairs", "eval/predicate-matching/pairs.json", "labeled pairs")
	flag.Parse()

	raw, err := os.ReadFile(*pairsPath)
	if err != nil {
		log.Fatal(err)
	}
	var pairs struct{ Same, Different [][2]string }
	if err := json.Unmarshal(raw, &pairs); err != nil {
		log.Fatal(err)
	}
	embed := func(text string) []float32 {
		if *lexical {
			v, _ := recall.LexicalEmbedder{}.Embed(context.Background(), text)
			return v
		}
		return ollamaEmbed(*ollama, *model, text)
	}
	text := func(name string) string { return strings.ReplaceAll(name, "_", " ") }
	score := func(ps [][2]string) []float64 {
		out := make([]float64, len(ps))
		for i, p := range ps {
			out[i] = cosine(embed(text(p[0])), embed(text(p[1])))
			fmt.Printf("%.3f  %-22s %s\n", out[i], p[0], p[1])
		}
		return out
	}
	fmt.Println("same relation:")
	same := score(pairs.Same)
	fmt.Println("\ndifferent relation:")
	diff := score(pairs.Different)

	fmt.Printf("\n%-9s %-22s %s\n", "threshold", "same merged", "different merged (wrong)")
	for _, th := range []float64{0.7, 0.75, 0.8, 0.85, 0.9, 0.95} {
		fmt.Printf("%-9.2f %-22s %s\n", th, frac(same, th), frac(diff, th))
	}
	sort.Float64s(diff)
	fmt.Printf("\nhighest different-relation score: %.3f\n", diff[len(diff)-1])
}

func frac(scores []float64, th float64) string {
	n := 0
	for _, s := range scores {
		if s >= th {
			n++
		}
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", n, len(scores), 100*float64(n)/float64(len(scores)))
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func ollamaEmbed(base, model, text string) []float32 {
	body, _ := json.Marshal(map[string]string{"model": model, "prompt": text})
	resp, err := http.Post(strings.TrimRight(base, "/")+"/api/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Embedding) == 0 {
		log.Fatalf("ollama: %v", err)
	}
	return out.Embedding
}
