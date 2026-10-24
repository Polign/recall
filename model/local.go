package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Polign/recall/internal/recallhome"
	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

// DefaultLocalEmbedModel is the model the local provider runs when a spec
// names none. It runs inside the recall binary, in pure Go, so search by
// meaning needs no key and no service: all-MiniLM-L6-v2 (Apache-2.0), 384
// dimensions, about 90 MB, downloaded once on first use.
const DefaultLocalEmbedModel = "all-minilm-l6-v2"

// maxLocalChars is where a text is first cut for the model, which takes at
// most 512 tokens; a longer text is embedded by its start and still stored
// and searched whole. Characters only estimate tokens: pasted terminal
// output can run near one token per character, so a text the model still
// refuses is cut further (see Embed).
const maxLocalChars = 1000

// minLocalChars is the shortest a refused text is cut to before giving up.
const minLocalChars = 64

type localFile struct {
	path, sha256 string
	size         int64 // bytes, for progress; 0 when small
}

type localModel struct {
	repo, revision string
	dim            int
	onnx           string // the file in files the session runs
	files          []localFile
}

// localModels pins each model to one Hugging Face revision and every file
// to its SHA-256, so a download is exactly the files Recall was tested with.
var localModels = map[string]localModel{
	"all-minilm-l6-v2": {
		repo:     "Xenova/all-MiniLM-L6-v2",
		revision: "751bff37182d3f1213fa05d7196b954e230abad9",
		dim:      384,
		onnx:     "model.onnx",
		files: []localFile{
			{"onnx/model.onnx", "759c3cd2b7fe7e93933ad23c4c9181b7396442a2ed746ec7c1d46192c469c46e", 90387606},
			{"tokenizer.json", "da0e79933b9ed51798a3ae27893d3c5fa4a201126cef75586296df9b4d2c62a0", 0},
			{"tokenizer_config.json", "9261e7d79b44c8195c1cada2b453e55b00aeb81e907a6664974b4d7776172ab3", 0},
			{"special_tokens_map.json", "b6d346be366a7d1d48332dbc9fdf3bf8960b5d879522b7799ddba59e76237ee3", 0},
			{"config.json", "7135149f7cffa1a573466c6e4d8423ed73b62fd2332c575bf738a0d033f70df7", 0},
		},
	},
}

// LocalModelDir is where downloaded models live: $RECALL_MODEL_DIR, else
// models/ under Recall's home ($RECALL_HOME, then ~/.local/share/recall, with
// the spellings before 0.23.0 still honored; see recallhome.Home). Copy a
// model's files there to use it on a machine without internet access.
func LocalModelDir() string {
	if d := os.Getenv("RECALL_MODEL_DIR"); d != "" {
		return d
	}
	if home := recallhome.Home(); home != "" {
		return filepath.Join(home, "models")
	}
	return filepath.Join(".", "recall-models")
}

// LocalModelReady reports whether every file of a local model is already
// downloaded into dir.
func LocalModelReady(name, dir string) bool {
	m, ok := localModels[name]
	if !ok {
		return false
	}
	for _, f := range m.files {
		if _, err := os.Stat(filepath.Join(dir, name, filepath.Base(f.path))); err != nil {
			return false
		}
	}
	return true
}

// LocalModelSize is the download size of a local model in bytes.
func LocalModelSize(name string) int64 {
	var n int64
	for _, f := range localModels[name].files {
		n += f.size
	}
	return n
}

// huggingFace is where model files come from; tests point it elsewhere.
var huggingFace = "https://huggingface.co"

// FetchLocalModel downloads a local model's files into dir/<name>, checking
// each against its pinned SHA-256 and writing it into place only when it
// matches. Files already present are not downloaded again. progress, when
// not nil, is told what is being fetched.
func FetchLocalModel(ctx context.Context, name, dir string, progress io.Writer) error {
	m, ok := localModels[name]
	if !ok {
		return fmt.Errorf("model: no local embedding model %q; the local models are: %s", name, DefaultLocalEmbedModel)
	}
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	for _, f := range m.files {
		dest := filepath.Join(target, filepath.Base(f.path))
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		if progress != nil && f.size > 0 {
			fmt.Fprintf(progress, "Downloading %s (%d MB) for search by meaning...\n", name, f.size>>20)
		}
		url := fmt.Sprintf("%s/%s/resolve/%s/%s", huggingFace, m.repo, m.revision, f.path)
		if err := fetchVerified(ctx, client, url, dest, f.sha256); err != nil {
			return fmt.Errorf("model: download %s: %w", f.path, err)
		}
	}
	return nil
}

func fetchVerified(ctx context.Context, client *http.Client, url, dest, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req) // #nosec G704 -- The URL is a pinned Hugging Face revision, not user input.
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: checksum %s, want %s", url, got, want)
	}
	return os.Rename(tmp.Name(), dest)
}

// LocalEmbedder runs a local model inside this process. It loads the model
// on first use and serializes calls; one embedding takes milliseconds.
type LocalEmbedder struct {
	name, dir string
	model     localModel
	progress  io.Writer

	once    sync.Once
	loadErr error
	mu      sync.Mutex
	session *hugot.Session
	pipe    *pipelines.FeatureExtractionPipeline
}

// NewLocalEmbedder checks the configuration. It downloads nothing until the
// first Embed (or Prepare); progress, when not nil, hears about a download.
func NewLocalEmbedder(cfg EmbedConfig, progress io.Writer) (*LocalEmbedder, error) {
	m, ok := localModels[cfg.Model]
	if !ok {
		return nil, fmt.Errorf("model: no local embedding model %q; the local models are: %s", cfg.Model, DefaultLocalEmbedModel)
	}
	if cfg.Dim != 0 && cfg.Dim != m.dim {
		return nil, fmt.Errorf("model: %s makes %d-dimensional vectors, not %d", cfg.Model, m.dim, cfg.Dim)
	}
	dir := cfg.BaseURL // for a local model, BaseURL is the directory models live in
	if dir == "" {
		dir = LocalModelDir()
	}
	return &LocalEmbedder{name: cfg.Model, dir: dir, model: m, progress: progress}, nil
}

// Spec names the vector space: "local:all-minilm-l6-v2".
func (e *LocalEmbedder) Spec() string { return "local:" + e.name }

// Dim is the size of the vectors.
func (e *LocalEmbedder) Dim() int { return e.model.dim }

// Prepare downloads the model if needed and loads it.
func (e *LocalEmbedder) Prepare(ctx context.Context) error {
	e.once.Do(func() { e.loadErr = e.load(ctx) })
	return e.loadErr
}

func (e *LocalEmbedder) load(ctx context.Context) error {
	if err := FetchLocalModel(ctx, e.name, e.dir, e.progress); err != nil {
		return err
	}
	// The session keeps the context it starts with and refuses every run
	// once that context is done. The first Embed usually comes from a
	// request, so the session must not inherit that request's cancellation,
	// or every later embedding fails with "context canceled".
	s, err := hugot.NewGoSession(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("model: start %s: %w", e.name, err)
	}
	p, err := hugot.NewPipeline(s, hugot.FeatureExtractionConfig{
		ModelPath:    filepath.Join(e.dir, e.name),
		Name:         "recall-" + e.name,
		OnnxFilename: e.model.onnx,
		Options:      []hugot.FeatureExtractionOption{pipelines.WithNormalization()},
	})
	if err != nil {
		_ = s.Destroy()
		return fmt.Errorf("model: load %s: %w", e.name, err)
	}
	e.session, e.pipe = s, p
	return nil
}

// Embed returns the normalized vector for text.
func (e *LocalEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := e.Prepare(ctx); err != nil {
		return nil, err
	}
	text = cutText(text, maxLocalChars)
	e.mu.Lock()
	defer e.mu.Unlock()
	out, err := e.pipe.RunPipeline(ctx, []string{text})
	// The model refuses a text of more than 512 tokens. Each retry keeps
	// the first 60% of what was refused, so the vector still comes from as
	// much of the text's start as fits.
	for err != nil && len(text) > minLocalChars && ctx.Err() == nil {
		text = cutText(text, len(text)*6/10)
		out, err = e.pipe.RunPipeline(ctx, []string{text})
	}
	if err != nil {
		return nil, fmt.Errorf("embedder: %s: %w", e.Spec(), err)
	}
	if len(out.Embeddings) != 1 || len(out.Embeddings[0]) != e.model.dim {
		return nil, errors.New("embedder: " + e.Spec() + ": unexpected output shape")
	}
	return out.Embeddings[0], nil
}

// Close releases the model.
func (e *LocalEmbedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session == nil {
		return nil
	}
	err := e.session.Destroy()
	e.session, e.pipe = nil, nil
	return err
}
