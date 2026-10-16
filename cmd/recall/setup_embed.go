package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall/internal/recallsetup"
	"github.com/Polign/recall/model"
)

// setupEmbedding works out the embedder a setup saves, before the database
// is reachable. A named -embed-model wins. Otherwise a new setup, or one
// given a new extraction model, follows the extraction model: openai and
// ollama embed with their own provider; anthropic, which has no embedding
// API, asks at a terminal (word search, a local Ollama embedder, or Voyage
// when VOYAGE_API_KEY is set) and without one uses a local Ollama embedder
// that is already pulled, else word search. A saved setup that is not
// changing its extraction model keeps its embedder. It returns the spec
// ("" for word search) and whether it was named rather than followed.
func setupEmbedding(cfg recallsetup.Config, flagSpec string, follow bool, in io.Reader, interactive bool, out io.Writer) (string, bool, error) {
	if flagSpec != "" {
		if strings.EqualFold(flagSpec, "lexical") {
			return "", true, nil
		}
		cfg, err := model.ParseEmbed(flagSpec)
		if err != nil {
			return "", false, fmt.Errorf("-embed-model: %w", err)
		}
		return cfg.Spec(), true, nil
	}
	if !follow || cfg.ExtractModel == "" || cfg.Typed {
		return cfg.EmbedModel, false, nil
	}
	local := "local:" + model.DefaultLocalEmbedModel
	if !strings.HasPrefix(cfg.ExtractModel, "anthropic") {
		return model.EmbedFor(cfg.ExtractModel), false, nil
	}
	// Anthropic has no embedding API. The model built into recall needs no
	// key and keeps text on this machine, so it is the default; at a
	// terminal, the alternatives are offered too.
	if !interactive {
		return local, false, nil
	}
	note := "built into recall; nothing leaves this machine"
	if !model.LocalModelReady(model.DefaultLocalEmbedModel, model.LocalModelDir()) {
		note += fmt.Sprintf("; downloads %d MB once", model.LocalModelSize(model.DefaultLocalEmbedModel)>>20)
	}
	choices := []modelChoice{{local, note}}
	if ollamaUp() && slices.Contains(ollamaEmbedModels(), model.DefaultOllamaEmbedModel) {
		choices = append(choices, modelChoice{"ollama:" + model.DefaultOllamaEmbedModel, "your local Ollama; nothing leaves this machine"})
	}
	if os.Getenv("VOYAGE_API_KEY") != "" {
		choices = append(choices, modelChoice{"voyage:" + model.DefaultVoyageEmbedModel, "VOYAGE_API_KEY is set; every remembered passage and every question is sent to Voyage AI and billed"})
	}
	choices = append(choices, modelChoice{"", "word search; matches words, not meaning"})
	spec, err := askChoice(in, out, "\nSearch? Anthropic has no embedding model, so Recall needs another one to search memory by meaning.", choices, 1)
	return spec, false, err
}

// askChoice asks for one of choices by number; an empty answer or end of
// input takes def (1-based).
func askChoice(in io.Reader, out io.Writer, question string, choices []modelChoice, def int) (string, error) {
	fmt.Fprintln(out, question)
	for i, c := range choices {
		fmt.Fprintf(out, "  %d. %s (%s)\n", i+1, embedName(c.spec), c.note)
	}
	reader := bufio.NewReader(in)
	for {
		fmt.Fprintf(out, "Choose a number [%d]: ", def)
		line, err := reader.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" {
			if err != nil {
				fmt.Fprintln(out)
			}
			return choices[def-1].spec, nil
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(choices) {
			return choices[n-1].spec, nil
		}
		fmt.Fprintf(out, "Answer with a number from 1 to %d.\n", len(choices))
		if err != nil {
			return choices[def-1].spec, nil
		}
	}
}

// askYes asks a yes/no question; an empty answer is def, and end of input
// is no, so nothing is done that nobody agreed to.
func askYes(in io.Reader, out io.Writer, question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Fprintf(out, "%s %s ", question, hint)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// prepareEmbedding makes sure the embedder a setup chose can run: a local
// Ollama model is pulled when missing, and a hosted one has its key. A
// followed embedder that cannot run gives way to word search with a note;
// a named one is an error.
func prepareEmbedding(spec string, named bool, out io.Writer) (string, error) {
	if spec == "" {
		return "", nil
	}
	fail := func(err error) (string, error) {
		if named {
			return "", fmt.Errorf("-embed-model %s: %w", spec, err)
		}
		fmt.Fprintf(out, "Not searching by meaning with %s: %v. Using word search; run setup again with -embed-model once that is fixed.\n", spec, err)
		return "", nil
	}
	if _, err := openEmbedding(spec, false); err != nil {
		return fail(err)
	}
	cfg, _ := model.ParseEmbed(spec)
	if cfg.Provider == "local" {
		if err := model.FetchLocalModel(context.Background(), cfg.Model, model.LocalModelDir(), out); err != nil {
			return fail(err)
		}
		return spec, nil
	}
	if cfg.Provider != "ollama" {
		return spec, nil
	}
	if !ollamaUp() {
		return fail(fmt.Errorf("ollama is not running at %s", ollamaHost()))
	}
	if have := ollamaEmbedModels(); slices.Contains(have, cfg.Model) || slices.Contains(have, cfg.Model+":latest") {
		return spec, nil
	}
	fmt.Fprintf(out, "Pulling %s from Ollama for meaning search...\n", cfg.Model)
	if err := ollamaPull(cfg.Model); err != nil {
		return fail(err)
	}
	return spec, nil
}

// describeSearch says in one line how a setup searches memory.
func describeSearch(cfg recallsetup.Config) string {
	if cfg.EmbedModel == "" {
		return "Search: words only. No embedding model is set, so recall matches the words of a question, not its meaning. Run setup again with -embed-model to search by meaning."
	}
	where := "on this machine"
	switch {
	case strings.HasPrefix(cfg.EmbedModel, "openai:"):
		where = "every remembered passage and every question is sent to OpenAI"
	case strings.HasPrefix(cfg.EmbedModel, "voyage:"):
		where = "every remembered passage and every question is sent to Voyage AI"
	}
	return "Search: by meaning with " + cfg.EmbedModel + " (" + where + ")"
}

func ollamaHost() string {
	host := envOr("OLLAMA_HOST", "localhost:11434")
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

type ollamaModel struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// ollamaTags lists the local Ollama models, and reports whether Ollama
// answered within a moment.
func ollamaTags() ([]ollamaModel, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaHost()+"/api/tags", nil)
	if err != nil || !strings.HasPrefix(req.URL.Scheme, "http") {
		return nil, false
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- OLLAMA_HOST is the operator's own setting.
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var tags struct {
		Models []ollamaModel `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&tags) != nil {
		return nil, false
	}
	return tags.Models, true
}

func ollamaUp() bool {
	_, ok := ollamaTags()
	return ok
}

// ollamaEmbedModels lists the local Ollama models that embed, under both
// their full and short names.
func ollamaEmbedModels() []string {
	models, _ := ollamaTags()
	var out []string
	for _, m := range models {
		if slices.Contains(m.Capabilities, "embedding") {
			out = append(out, m.Name, strings.TrimSuffix(m.Name, ":latest"))
		}
	}
	return out
}

// ollamaPull downloads a model into the local Ollama.
func ollamaPull(name string) error {
	body, _ := json.Marshal(map[string]any{"model": name, "stream": false})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaHost()+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- OLLAMA_HOST is the operator's own setting.
	if err != nil {
		return fmt.Errorf("ollama pull %s: %w", name, err)
	}
	defer resp.Body.Close()
	var reply struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply)
	if resp.StatusCode/100 != 2 || reply.Error != "" {
		return fmt.Errorf("ollama pull %s: %s %s", name, resp.Status, reply.Error)
	}
	return nil
}

// settleCollection runs once the database answers. A setup changing its
// embedder copies the memory already saved into the new collection when the
// embedder was named, or when the user says yes at a terminal; otherwise it
// keeps the old collection and embedder and says how to copy later. The
// collection a setup ends with must hold vectors its embedder makes.
func settleCollection(c *api, previous recallsetup.Config, cfg *recallsetup.Config, switching, named bool, out io.Writer) error {
	backend, err := openMemoryBackend(c)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if switching && previous.Collection != cfg.Collection {
		if held, err := storedDim(ctx, backend, previous.Collection); err != nil {
			return err
		} else if held > 0 {
			question := fmt.Sprintf("Copy the memory in %s into %s, searched by meaning with %s? The old collection is kept.",
				previous.Collection, cfg.Collection, embedName(cfg.EmbedModel))
			copyIt := named || isTerminal(os.Stdin) && askYes(os.Stdin, out, question, true)
			if !copyIt {
				fmt.Fprintf(out, "Keeping %s with %s search. To copy it later: recall reembed -embed-model %s\n",
					previous.Collection, describeKind(previous.EmbedModel), embedName(cfg.EmbedModel))
				cfg.Collection, cfg.EmbedModel = previous.Collection, previous.EmbedModel
				return nil
			}
			src, err := openEmbedding(previous.EmbedModel, false)
			if err != nil {
				return err
			}
			dst, err := openEmbedding(cfg.EmbedModel, false)
			if err != nil {
				return err
			}
			n, err := reembed(ctx, backend, previous.Collection, src, cfg.Collection, dst)
			if err != nil {
				return fmt.Errorf("copy memory to %s: %w", cfg.Collection, err)
			}
			fmt.Fprintf(out, "Copied %d events from %s to %s; %s is kept as it was.\n", n, previous.Collection, cfg.Collection, previous.Collection)
			return nil
		}
	}
	emb, err := openEmbedding(cfg.EmbedModel, false)
	if err != nil {
		return err
	}
	_, _, err = fitEmbedding(ctx, backend, cfg.Collection, emb)
	return err
}

func describeKind(spec string) string {
	if spec == "" {
		return "word"
	}
	return spec
}
