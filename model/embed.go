package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default embedding models. Each follows the extractor's provider, so a
// setup that already sends text to a provider embeds with the same one.
// text-embedding-3-small at 512 dimensions is what the LongMemEval
// user-facts run measured (Recall lexical 88.8, with this embedder 92.9).
const (
	DefaultOpenAIEmbedModel = "text-embedding-3-small"
	DefaultOpenAIEmbedDim   = 512
	DefaultOllamaEmbedModel = "nomic-embed-text"
	DefaultVoyageEmbedModel = "voyage-3.5"
)

// maxEmbedChars keeps one request under the providers' input limits
// (text-embedding-3 takes 8191 tokens). A longer text is embedded by its
// start; the whole text is still stored and searched by words.
const maxEmbedChars = 24000

// EmbedConfig selects an embedding model. Empty BaseURL and APIKey are read
// from the provider's usual environment variables.
type EmbedConfig struct {
	// Provider is "openai", "ollama", or "voyage". openai also serves any
	// endpoint that speaks the OpenAI embeddings API, through BaseURL or
	// OPENAI_BASE_URL.
	Provider string
	Model    string
	// Dim asks the model for vectors of this size, where it can shorten
	// them (text-embedding-3, voyage-3.5); 0 takes the model's own size.
	Dim        int
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// Spec is the configuration as ParseEmbed reads it, such as
// "openai:text-embedding-3-small@512". It names the vector space, so two
// configurations with the same Spec embed compatibly.
func (c EmbedConfig) Spec() string {
	s := c.Provider + ":" + c.Model
	if c.Dim > 0 {
		s += "@" + strconv.Itoa(c.Dim)
	}
	return s
}

// KnownDim is the size of the vectors this configuration returns, or 0 when
// the model is not one Recall knows.
func (c EmbedConfig) KnownDim() int {
	if c.Dim > 0 {
		return c.Dim
	}
	switch c.Model {
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-3-large":
		return 3072
	case "nomic-embed-text", "nomic-embed-text:latest":
		return 768
	case "voyage-3.5", "voyage-3.5-lite", "voyage-3-large":
		return 1024
	}
	return 0
}

// ParseEmbed reads a "provider:model[@dim]" spec, such as
// "openai:text-embedding-3-small@512", "ollama:nomic-embed-text", or
// "voyage:voyage-3.5". A provider alone takes its default model.
func ParseEmbed(spec string) (EmbedConfig, error) {
	provider, rest, _ := strings.Cut(strings.TrimSpace(spec), ":")
	name, dim, hasDim := strings.Cut(strings.TrimSpace(rest), "@")
	cfg := EmbedConfig{Provider: strings.ToLower(strings.TrimSpace(provider)), Model: strings.TrimSpace(name)}
	if hasDim {
		n, err := strconv.Atoi(strings.TrimSpace(dim))
		if err != nil || n <= 0 {
			return EmbedConfig{}, fmt.Errorf("model: %q: the size after @ must be a positive number", spec)
		}
		cfg.Dim = n
	}
	switch cfg.Provider {
	case "openai":
		if cfg.Model == "" {
			cfg.Model = DefaultOpenAIEmbedModel
			if cfg.Dim == 0 {
				cfg.Dim = DefaultOpenAIEmbedDim
			}
		}
	case "ollama":
		if cfg.Model == "" {
			cfg.Model = DefaultOllamaEmbedModel
		}
	case "voyage":
		if cfg.Model == "" {
			cfg.Model = DefaultVoyageEmbedModel
		}
	default:
		return EmbedConfig{}, fmt.Errorf("model: unknown embedding provider in %q; use openai:<model>, ollama:<model>, or voyage:<model>", spec)
	}
	return cfg, nil
}

// EmbedFor is the embedding model that follows an extraction model spec:
// openai:* embeds with text-embedding-3-small at 512 dimensions and
// ollama:* with nomic-embed-text. It returns "" for anthropic:*, which has
// no embedding API, for an openai spec pointed at another endpoint by
// OPENAI_BASE_URL, which may not serve embeddings, and for no model.
func EmbedFor(extractSpec string) string {
	cfg, err := Parse(extractSpec)
	if err != nil || strings.TrimSpace(extractSpec) == "" {
		return ""
	}
	switch cfg.Provider {
	case "openai":
		if base := os.Getenv("OPENAI_BASE_URL"); base != "" && !strings.HasPrefix(base, "https://api.openai.com") {
			return ""
		}
		return "openai:" + DefaultOpenAIEmbedModel + "@" + strconv.Itoa(DefaultOpenAIEmbedDim)
	case "ollama":
		return "ollama:" + DefaultOllamaEmbedModel
	}
	return ""
}

// Embedder is a recall.Embedder backed by an embeddings API. OpenAI, Ollama
// (through its OpenAI-compatible endpoint) and Voyage all take the same
// request and answer in the same shape.
type Embedder struct {
	cfg  EmbedConfig
	base string
}

// NewEmbedder checks the configuration and builds the embedder. It makes no
// request until the first Embed.
func NewEmbedder(cfg EmbedConfig) (*Embedder, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("model: %s needs an embedding model name", cfg.Provider)
	}
	switch cfg.Provider {
	case "openai":
		if cfg.BaseURL == "" {
			cfg.BaseURL = os.Getenv("OPENAI_BASE_URL")
		}
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		}
		if cfg.BaseURL == "" {
			if cfg.APIKey == "" {
				return nil, fmt.Errorf("model: openai embeddings need OPENAI_API_KEY")
			}
			cfg.BaseURL = "https://api.openai.com/v1"
		}
	case "ollama":
		if cfg.BaseURL == "" {
			cfg.BaseURL = ollamaBase(os.Getenv("OLLAMA_HOST"))
		}
	case "voyage":
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("VOYAGE_API_KEY")
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("model: voyage embeddings need VOYAGE_API_KEY")
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.voyageai.com/v1"
		}
	default:
		return nil, fmt.Errorf("model: unknown embedding provider %q", cfg.Provider)
	}
	return &Embedder{cfg: cfg, base: strings.TrimRight(cfg.BaseURL, "/")}, nil
}

// Spec names the vector space this embedder writes; see EmbedConfig.Spec.
func (e *Embedder) Spec() string { return e.cfg.Spec() }

// Dim is the size of the vectors it returns, or 0 when unknown.
func (e *Embedder) Dim() int { return e.cfg.KnownDim() }

// Embed returns the vector for text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if len(text) > maxEmbedChars {
		text = text[:maxEmbedChars]
	}
	req := map[string]any{"model": e.cfg.Model, "input": []string{text}}
	if e.cfg.Dim > 0 {
		switch e.cfg.Provider {
		case "openai":
			req["dimensions"] = e.cfg.Dim
		case "voyage":
			req["output_dimension"] = e.cfg.Dim
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.cfg.HTTPClient.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("embedder: %s: %w", e.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("embedder: %s: %w", e.base, err)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Error  json.RawMessage `json:"error"`
		Detail string          `json:"detail"`
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw[:min(len(raw), 500)]))
		if json.Unmarshal(raw, &out) == nil {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(out.Error, &e) == nil && e.Message != "" {
				msg = e.Message
			} else if out.Detail != "" {
				msg = out.Detail
			}
		}
		if resp.StatusCode == http.StatusNotFound && e.cfg.Provider == "ollama" {
			msg += " (run: ollama pull " + e.cfg.Model + ")"
		}
		return nil, fmt.Errorf("embedder: %s: %s: %s", e.cfg.Spec(), resp.Status, msg)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedder: %s: malformed reply: %w", e.cfg.Spec(), err)
	}
	if len(out.Data) != 1 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedder: %s: reply has no vector", e.cfg.Spec())
	}
	v := out.Data[0].Embedding
	if d := e.Dim(); d > 0 && len(v) != d {
		return nil, fmt.Errorf("embedder: %s returned %d dimensions, expected %d", e.cfg.Spec(), len(v), d)
	}
	return v, nil
}
