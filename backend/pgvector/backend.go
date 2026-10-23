// Package pgvector implements Recall's context-aware Backend on PostgreSQL
// with the pgvector extension. Each Recall collection is one table, created
// on the first write with the dimension of the first vector written to it,
// in a schema of its own (recall by default) beside two bookkeeping tables:
// recall_collections, which maps collection names to tables and counts each
// collection's writes, and recall_leases, which holds agent leases.
//
// Postgres gives this backend everything Recall can use: exact listings from
// one snapshot, a per-collection revision that changes on every write (the
// watermark behind Recall's cached reads), row locks for agent leases, and
// full-text search over each event's text, fused with the vector search.
//
// Every write increments its collection's revision under a row lock, which
// serializes writers to one collection. Agent memory is written at the pace
// of a conversation, where that costs nothing; a bulk loader would notice.
package pgvector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Polign/recall"
)

// DefaultSchema is where the tables live unless Config.Schema says otherwise.
const DefaultSchema = "recall"

// Config selects a database and how the backend lays its tables out.
type Config struct {
	// URL is a PostgreSQL connection string, such as
	// postgres://user:password@localhost:5432/recall. libpq's PG* environment
	// variables fill in what it leaves out.
	URL string
	// Password overrides the connection string's password when set.
	Password string
	// Schema is the schema the tables are created in; empty means DefaultSchema.
	Schema string
	// TextSearchConfig is the PostgreSQL text search configuration the event
	// text is indexed with, such as english (the default) or simple. Tables
	// keep the configuration they were created with.
	TextSearchConfig string
	// Pool is a connection pool to use instead of opening one from URL. The
	// backend never closes a supplied pool.
	Pool *pgxpool.Pool
}

// Backend is a reusable PostgreSQL client, safe for concurrent calls.
type Backend struct {
	pool   *pgxpool.Pool
	owned  bool // the backend opened pool and closes it
	schema string
	tsCfg  string

	// mu serializes schema and table creation inside this process; an
	// advisory lock does the same across processes.
	mu          sync.Mutex
	ready       bool
	iterative   bool // pgvector 0.8.0 or later: HNSW scans can keep going until a filter is satisfied
	collections map[string]collectionInfo
}

type collectionInfo struct {
	table string
	dim   int
}

var (
	_ recall.Backend           = (*Backend)(nil)
	_ recall.GetBackend        = (*Backend)(nil)
	_ recall.TextSearchBackend = (*Backend)(nil)
	_ recall.WatermarkBackend  = (*Backend)(nil)
	_ recall.LeaseBackend      = (*Backend)(nil)
)

// Importing this package registers it as the "pgvector" backend. The binary
// reads the connection string from DATABASE_URL and a password from
// PGPASSWORD when its flags do not set them.
func init() {
	recall.RegisterBackend("pgvector", recall.BackendDriver{
		Open: func(o recall.BackendOptions) (recall.Backend, error) {
			return New(Config{URL: o.URL, Password: o.APIKey, Schema: o.Params["schema"], TextSearchConfig: o.Params["text_search"]})
		},
		DefaultURL: "postgres://localhost:5432/postgres",
		URLEnv:     "DATABASE_URL",
		KeyEnv:     "PGPASSWORD",
		// Options on the connection string: ?schema=memory&text_search=simple.
		Params: []string{"schema", "text_search"},
	})
}

var identifierRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// New validates the configuration and creates a backend. It opens no
// connection; the first operation does, and creates the schema and
// bookkeeping tables if they are missing.
func New(cfg Config) (*Backend, error) {
	schema := cfg.Schema
	if schema == "" {
		schema = DefaultSchema
	}
	if !identifierRE.MatchString(schema) || len(schema) > 63 {
		return nil, fmt.Errorf("pgvector: schema must be a lower-case identifier, not %q", schema)
	}
	tsCfg := cfg.TextSearchConfig
	if tsCfg == "" {
		tsCfg = "english"
	}
	if !identifierRE.MatchString(tsCfg) {
		return nil, fmt.Errorf("pgvector: text search configuration must be an identifier, not %q", tsCfg)
	}
	pool, owned := cfg.Pool, cfg.Pool == nil
	if owned {
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("pgvector: a connection string is required")
		}
		pc, err := pgxpool.ParseConfig(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("pgvector: connection string: %w", err)
		}
		if cfg.Password != "" {
			pc.ConnConfig.Password = cfg.Password
		}
		pool, err = pgxpool.NewWithConfig(context.Background(), pc)
		if err != nil {
			return nil, fmt.Errorf("pgvector: pool: %w", err)
		}
	}
	return &Backend{pool: pool, owned: owned, schema: schema, tsCfg: tsCfg, collections: map[string]collectionInfo{}}, nil
}

// Close releases the pool the backend opened. It does nothing for a pool
// supplied through Config.
func (b *Backend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.owned {
		b.pool.Close()
	}
}

// Pool is the connection pool the backend uses, for callers that want to
// query the tables themselves.
func (b *Backend) Pool() *pgxpool.Pool { return b.pool }

// TableName is the identifier a collection's table gets: the name itself when
// it is already a plain lower-case identifier of at most 48 characters, and
// otherwise a cleaned form with a hash suffix, so distinct collections never
// share a table. The mapping is also recorded in recall_collections.
func TableName(collection string) string {
	clean := strings.ToLower(collection)
	clean = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, clean)
	if clean == collection && identifierRE.MatchString(clean) && len(clean) <= 48 && !reserved[clean] {
		return clean
	}
	sum := sha256.Sum256([]byte(collection))
	if len(clean) > 48 {
		clean = clean[:48]
	}
	clean = strings.TrimRight(clean, "_")
	switch {
	case clean == "":
		clean = "c"
	case clean[0] >= '0' && clean[0] <= '9':
		clean = "c_" + clean
	}
	return clean + "_" + hex.EncodeToString(sum[:4])
}

var reserved = map[string]bool{"recall_collections": true, "recall_leases": true}

func (b *Backend) qualified(table string) string {
	return pgx.Identifier{b.schema, table}.Sanitize()
}

func checkCollection(collection string) error {
	if strings.TrimSpace(collection) == "" || len(collection) > 512 {
		return fmt.Errorf("pgvector: collection is required and at most 512 characters")
	}
	return nil
}

// ensureReady creates the schema and bookkeeping tables once per process.
func (b *Backend) ensureReady(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready {
		return nil
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgvector: connect: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize bootstrap across processes; the lock goes with the transaction.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "recall:"+b.schema); err != nil {
		return fmt.Errorf("pgvector: bootstrap: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		// Creating the extension needs a privilege the role may lack; what
		// matters is that it exists.
		var installed bool
		if e := b.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')").Scan(&installed); e != nil || !installed {
			return fmt.Errorf("pgvector: the vector extension is not installed and could not be created (%v); install pgvector and run CREATE EXTENSION vector as a role that may", err)
		}
		// The failed statement aborted the transaction; start over without it.
		_ = tx.Rollback(ctx)
		if tx, err = b.pool.Begin(ctx); err != nil {
			return fmt.Errorf("pgvector: connect: %w", err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "recall:"+b.schema); err != nil {
			return fmt.Errorf("pgvector: bootstrap: %w", err)
		}
	}
	stmts := []string{
		"CREATE SCHEMA IF NOT EXISTS " + pgx.Identifier{b.schema}.Sanitize(),
		`CREATE TABLE IF NOT EXISTS ` + b.qualified("recall_collections") + ` (
			name text PRIMARY KEY,
			tbl text NOT NULL UNIQUE,
			dim integer NOT NULL,
			nonce bigint NOT NULL,
			revision bigint NOT NULL DEFAULT 0,
			created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS ` + b.qualified("recall_leases") + ` (
			collection text NOT NULL,
			name text NOT NULL,
			holder text NOT NULL,
			epoch bigint NOT NULL,
			expires_at timestamptz NOT NULL,
			PRIMARY KEY (collection, name)
		)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("pgvector: bootstrap: %w", err)
		}
	}
	var version string
	if err := tx.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = 'vector'").Scan(&version); err != nil {
		return fmt.Errorf("pgvector: read extension version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgvector: bootstrap: %w", err)
	}
	b.iterative = atLeast(version, 0, 8)
	b.ready = true
	return nil
}

// atLeast reports whether a dotted version is major.minor or later.
func atLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}

// lookup finds a collection's table, or reports it missing without creating it.
func (b *Backend) lookup(ctx context.Context, collection string) (collectionInfo, bool, error) {
	if err := b.ensureReady(ctx); err != nil {
		return collectionInfo{}, false, err
	}
	b.mu.Lock()
	info, ok := b.collections[collection]
	b.mu.Unlock()
	if ok {
		return info, true, nil
	}
	err := b.pool.QueryRow(ctx, "SELECT tbl, dim FROM "+b.qualified("recall_collections")+" WHERE name = $1", collection).Scan(&info.table, &info.dim)
	if errors.Is(err, pgx.ErrNoRows) {
		return collectionInfo{}, false, nil
	}
	if err != nil {
		return collectionInfo{}, false, fmt.Errorf("pgvector: look up collection: %w", err)
	}
	b.mu.Lock()
	b.collections[collection] = info
	b.mu.Unlock()
	return info, true, nil
}

// ensureCollection finds or creates a collection's table for vectors of dim.
func (b *Backend) ensureCollection(ctx context.Context, collection string, dim int) (collectionInfo, error) {
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || ok {
		return info, err
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return collectionInfo{}, fmt.Errorf("pgvector: connect: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "recall:"+b.schema+":"+collection); err != nil {
		return collectionInfo{}, fmt.Errorf("pgvector: create collection: %w", err)
	}
	// Another process may have created it while this one waited for the lock.
	err = tx.QueryRow(ctx, "SELECT tbl, dim FROM "+b.qualified("recall_collections")+" WHERE name = $1", collection).Scan(&info.table, &info.dim)
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return collectionInfo{}, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		info = collectionInfo{table: TableName(collection), dim: dim}
		var nb [8]byte
		if _, err := rand.Read(nb[:]); err != nil {
			return collectionInfo{}, err
		}
		nonce := int64(binary.BigEndian.Uint64(nb[:]) >> 1)
		if _, err := tx.Exec(ctx, "INSERT INTO "+b.qualified("recall_collections")+" (name, tbl, dim, nonce) VALUES ($1, $2, $3, $4)", collection, info.table, dim, nonce); err != nil {
			return collectionInfo{}, fmt.Errorf("pgvector: register collection: %w", err)
		}
		t := b.qualified(info.table)
		ix := func(suffix string) string { return pgx.Identifier{info.table + "_" + suffix}.Sanitize() }
		stmts := []string{
			fmt.Sprintf(`CREATE TABLE %s (
				id text PRIMARY KEY,
				embedding vector(%d) NOT NULL,
				metadata jsonb NOT NULL,
				tsv tsvector GENERATED ALWAYS AS (to_tsvector('%s'::regconfig, coalesce(metadata->>'%s', ''))) STORED
			)`, t, dim, b.tsCfg, recall.TextField),
			fmt.Sprintf("CREATE INDEX %s ON %s USING hnsw (embedding vector_cosine_ops)", ix("embedding"), t),
			fmt.Sprintf("CREATE INDEX %s ON %s USING gin (metadata jsonb_path_ops)", ix("metadata"), t),
			fmt.Sprintf("CREATE INDEX %s ON %s USING gin (tsv)", ix("tsv"), t),
			fmt.Sprintf("CREATE INDEX %s ON %s (%s)", ix("observed"), t, numberExpr("observed_ms")),
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return collectionInfo{}, fmt.Errorf("pgvector: create collection: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return collectionInfo{}, fmt.Errorf("pgvector: create collection: %w", err)
		}
	default:
		return collectionInfo{}, fmt.Errorf("pgvector: look up collection: %w", err)
	}
	b.mu.Lock()
	b.collections[collection] = info
	b.mu.Unlock()
	return info, nil
}

// Put upserts one immutable event ID, creating the collection on first use,
// and moves the collection's revision in the same transaction.
func (b *Backend) Put(ctx context.Context, collection, id string, values []float32, metadata map[string]any) error {
	return b.put(ctx, collection, id, values, metadata, nil)
}

// PutFenced is Put applied only while the agent's lease is still the
// writer's: the lease row is read under FOR SHARE in the write's own
// transaction, so a takeover waits for the write to commit or the write sees
// the takeover, never both. A fence that no longer holds is reported as
// recall.ErrLeaseLost with nothing written.
func (b *Backend) PutFenced(ctx context.Context, collection, id string, values []float32, metadata map[string]any, fence recall.Fence) error {
	if strings.TrimSpace(fence.Lease) == "" || strings.TrimSpace(fence.Holder) == "" || fence.Epoch == 0 {
		return fmt.Errorf("pgvector: a lease fence needs a lease name, a holder and an epoch")
	}
	return b.put(ctx, collection, id, values, metadata, &fence)
}

func (b *Backend) put(ctx context.Context, collection, id string, values []float32, metadata map[string]any, fence *recall.Fence) error {
	if ctx == nil {
		return fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("pgvector: event ID is required")
	}
	if len(values) == 0 {
		return fmt.Errorf("pgvector: refusing to store an event with no vector")
	}
	info, err := b.ensureCollection(ctx, collection, len(values))
	if err != nil {
		return err
	}
	if info.dim != len(values) {
		return fmt.Errorf("pgvector: collection %s holds %d-dimensional vectors, not %d; a new embedder needs a new collection", collection, info.dim, len(values))
	}
	md, err := json.Marshal(metadataOrEmpty(metadata))
	if err != nil {
		return fmt.Errorf("pgvector: metadata: %w", err)
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgvector: connect: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if fence != nil {
		// Renewal moves the epoch on, so a newer epoch from the same holder
		// is its own renewal racing this write. Release leaves the holder but
		// sets expires_at to the epoch's start, which also fails the check.
		var held bool
		err := tx.QueryRow(ctx, "SELECT holder = $3 AND epoch >= $4 AND expires_at > to_timestamp(0) FROM "+b.qualified("recall_leases")+
			" WHERE collection = $1 AND name = $2 FOR SHARE", collection, fence.Lease, fence.Holder, int64(fence.Epoch)).Scan(&held)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !held) {
			return fmt.Errorf("%w: write fenced", recall.ErrLeaseLost)
		}
		if err != nil {
			return fmt.Errorf("pgvector: check lease: %w", err)
		}
	}
	// The revision row lock orders writers; the insert then commits with it.
	if _, err := tx.Exec(ctx, "UPDATE "+b.qualified("recall_collections")+" SET revision = revision + 1 WHERE name = $1", collection); err != nil {
		return fmt.Errorf("pgvector: put: %w", err)
	}
	_, err = tx.Exec(ctx, "INSERT INTO "+b.qualified(info.table)+` (id, embedding, metadata) VALUES ($1, $2::vector, $3::jsonb)
		ON CONFLICT (id) DO UPDATE SET embedding = EXCLUDED.embedding, metadata = EXCLUDED.metadata`, id, formatVector(values), string(md))
	if err != nil {
		return fmt.Errorf("pgvector: put: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgvector: put: %w", err)
	}
	return nil
}

func metadataOrEmpty(md map[string]any) map[string]any {
	if md == nil {
		return map[string]any{}
	}
	return md
}

// Watermark is the collection's revision, which moves on every write, behind
// a nonce chosen when the collection was created, so a collection dropped and
// made again never repeats a revision. A collection nothing has written to
// has a watermark too, which its first write changes.
func (b *Backend) Watermark(ctx context.Context, collection string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return "", err
	}
	if err := b.ensureReady(ctx); err != nil {
		return "", err
	}
	var nonce, revision int64
	err := b.pool.QueryRow(ctx, "SELECT nonce, revision FROM "+b.qualified("recall_collections")+" WHERE name = $1", collection).Scan(&nonce, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return "unwritten", nil
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: watermark: %w", err)
	}
	return strconv.FormatInt(nonce, 36) + "-" + strconv.FormatInt(revision, 10), nil
}

// List returns up to limit exact matches, in id order, and the exact total,
// both from one snapshot.
func (b *Backend) List(ctx context.Context, collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	if ctx == nil {
		return nil, 0, fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > recall.MaxHistoryEvents+1 {
		return nil, 0, fmt.Errorf("pgvector: list limit must be in [1, %d]", recall.MaxHistoryEvents+1)
	}
	where, args, err := translateFilter(filter, 1)
	if err != nil {
		return nil, 0, err
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.StoredVector{}, 0, err
	}
	args = append(args, limit)
	q := fmt.Sprintf("SELECT id, embedding::text, metadata, count(*) OVER () FROM %s%s ORDER BY id LIMIT $%d", b.qualified(info.table), where, len(args))
	rows, err := b.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("pgvector: list: %w", err)
	}
	defer rows.Close()
	out := make([]recall.StoredVector, 0, limit)
	total := 0
	for rows.Next() {
		var (
			id, vec string
			md      map[string]any
			n       int64
		)
		if err := rows.Scan(&id, &vec, &md, &n); err != nil {
			return nil, 0, fmt.Errorf("pgvector: list: %w", err)
		}
		values, err := parseVector(vec)
		if err != nil {
			return nil, 0, err
		}
		total = int(n)
		out = append(out, recall.StoredVector{ID: id, Values: values, Metadata: md})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("pgvector: list: %w", err)
	}
	return out, total, nil
}

// Search returns the k nearest events by cosine distance. Score is 1 minus
// the distance.
func (b *Backend) Search(ctx context.Context, collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("pgvector: search limit must be in [1, 10000]")
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("pgvector: a query vector is required")
	}
	where, args, err := translateFilter(filter, 2)
	if err != nil {
		return nil, err
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.Hit{}, err
	}
	if info.dim != len(values) {
		return nil, fmt.Errorf("pgvector: collection %s holds %d-dimensional vectors, not %d", collection, info.dim, len(values))
	}
	args = append([]any{formatVector(values)}, args...)
	args = append(args, k)
	q := fmt.Sprintf("SELECT id, metadata, embedding <=> $1::vector AS distance FROM %s%s ORDER BY distance LIMIT $%d", b.qualified(info.table), where, len(args))
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgvector: connect: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The index scan must look at enough candidates to fill k after the
	// filter; pgvector 0.8 keeps scanning until it has, older ones only as
	// far as ef_search.
	if _, err := tx.Exec(ctx, "SET LOCAL hnsw.ef_search = "+strconv.Itoa(max(40, min(k, 1000)))); err != nil {
		return nil, fmt.Errorf("pgvector: search: %w", err)
	}
	if b.iterative {
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = relaxed_order"); err != nil {
			return nil, fmt.Errorf("pgvector: search: %w", err)
		}
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgvector: search: %w", err)
	}
	defer rows.Close()
	hits := make([]recall.Hit, 0, k)
	for rows.Next() {
		var (
			id   string
			md   map[string]any
			dist float64
		)
		if err := rows.Scan(&id, &md, &dist); err != nil {
			return nil, fmt.Errorf("pgvector: search: %w", err)
		}
		if math.IsNaN(dist) {
			dist = 1
		}
		hits = append(hits, recall.Hit{ID: id, Distance: float32(dist), Score: float32(1 - dist), Metadata: md})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgvector: search: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("pgvector: search: %w", err)
	}
	return hits, nil
}

// SearchText ranks events by PostgreSQL full-text match of the query's words
// against the event text, any word matching. Score is ts_rank_cd; Distance is
// 1/(1+Score). A query with no searchable words returns no hits.
func (b *Backend) SearchText(ctx context.Context, collection, text string, k int, filter map[string]any) ([]recall.Hit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("pgvector: search limit must be in [1, 10000]")
	}
	query := textQuery(text)
	if query == "" {
		return []recall.Hit{}, nil
	}
	where, args, err := translateFilter(filter, 2)
	if err != nil {
		return nil, err
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.Hit{}, err
	}
	args = append([]any{query}, args...)
	args = append(args, k)
	cond := " WHERE tsv @@ q"
	if where != "" {
		cond += " AND" + strings.TrimPrefix(where, " WHERE")
	}
	q := fmt.Sprintf("SELECT id, metadata, ts_rank_cd(tsv, q) AS score FROM %s, websearch_to_tsquery('%s'::regconfig, $1) q%s ORDER BY score DESC, id LIMIT $%d", b.qualified(info.table), b.tsCfg, cond, len(args))
	rows, err := b.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgvector: text search: %w", err)
	}
	defer rows.Close()
	hits := make([]recall.Hit, 0, k)
	for rows.Next() {
		var (
			id    string
			md    map[string]any
			score float32
		)
		if err := rows.Scan(&id, &md, &score); err != nil {
			return nil, fmt.Errorf("pgvector: text search: %w", err)
		}
		hits = append(hits, recall.Hit{ID: id, Score: score, Distance: 1 / (1 + score), Metadata: md})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgvector: text search: %w", err)
	}
	return hits, nil
}

// textQuery turns free text into websearch_to_tsquery input that matches any
// of its words: the words joined by "or", with the characters that syntax
// gives meaning to (quotes, a leading minus) removed. websearch_to_tsquery
// never fails on its input, so the rest can pass through.
func textQuery(text string) string {
	var words []string
	for _, w := range strings.Fields(text) {
		w = strings.ReplaceAll(w, `"`, "")
		w = strings.TrimLeft(w, "-")
		if w != "" && !strings.EqualFold(w, "or") {
			words = append(words, w)
		}
	}
	return strings.Join(words, " or ")
}

// Get reads events by id in request order; unknown ids are omitted.
func (b *Backend) Get(ctx context.Context, collection string, ids []string) ([]recall.StoredVector, error) {
	if ctx == nil {
		return nil, fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > recall.MaxGetEvents {
		return nil, fmt.Errorf("pgvector: get takes 1 to %d ids", recall.MaxGetEvents)
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.StoredVector{}, err
	}
	rows, err := b.pool.Query(ctx, "SELECT id, embedding::text, metadata FROM "+b.qualified(info.table)+" WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("pgvector: get: %w", err)
	}
	defer rows.Close()
	byID := map[string]recall.StoredVector{}
	for rows.Next() {
		var (
			id, vec string
			md      map[string]any
		)
		if err := rows.Scan(&id, &vec, &md); err != nil {
			return nil, fmt.Errorf("pgvector: get: %w", err)
		}
		values, err := parseVector(vec)
		if err != nil {
			return nil, err
		}
		byID[id] = recall.StoredVector{ID: id, Values: values, Metadata: md}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgvector: get: %w", err)
	}
	out := make([]recall.StoredVector, 0, len(byID))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			out = append(out, v)
			delete(byID, id)
		}
	}
	return out, nil
}

// AcquireLease takes an agent's lease unless a live holder has it. The epoch
// only ever grows: a fresh lease starts at 1, and a lease taken over after
// its holder let it lapse continues from where the chain was.
func (b *Backend) AcquireLease(ctx context.Context, collection, name, holder string, ttl time.Duration) (recall.LeaseGrant, error) {
	if ctx == nil {
		return recall.LeaseGrant{}, fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkLease(collection, name, holder, ttl); err != nil {
		return recall.LeaseGrant{}, err
	}
	if err := b.ensureReady(ctx); err != nil {
		return recall.LeaseGrant{}, err
	}
	leases := b.qualified("recall_leases")
	var epoch int64
	err := b.pool.QueryRow(ctx, `INSERT INTO `+leases+` (collection, name, holder, epoch, expires_at)
		VALUES ($1, $2, $3, 1, clock_timestamp() + $4 * interval '1 millisecond')
		ON CONFLICT (collection, name) DO UPDATE
		SET holder = EXCLUDED.holder, epoch = `+leases+`.epoch + 1, expires_at = EXCLUDED.expires_at
		WHERE `+leases+`.expires_at <= clock_timestamp()
		RETURNING epoch`, collection, name, holder, ttl.Milliseconds()).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		var held recall.LeaseHeldError
		if e := b.pool.QueryRow(ctx, "SELECT holder, expires_at FROM "+leases+" WHERE collection = $1 AND name = $2", collection, name).Scan(&held.Holder, &held.Expires); e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return recall.LeaseGrant{}, fmt.Errorf("pgvector: acquire lease: %w", e)
		}
		return recall.LeaseGrant{}, &held
	}
	if err != nil {
		return recall.LeaseGrant{}, fmt.Errorf("pgvector: acquire lease: %w", err)
	}
	return recall.LeaseGrant{Epoch: uint64(epoch), Holder: holder, TTL: ttl}, nil
}

// RenewLease extends a held lease and advances its epoch. A lease whose
// chain has moved past the given epoch was lost to another holder.
func (b *Backend) RenewLease(ctx context.Context, collection, name, holder string, epoch uint64, ttl time.Duration) (recall.LeaseGrant, error) {
	if ctx == nil {
		return recall.LeaseGrant{}, fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkLease(collection, name, holder, ttl); err != nil {
		return recall.LeaseGrant{}, err
	}
	if err := b.ensureReady(ctx); err != nil {
		return recall.LeaseGrant{}, err
	}
	var next int64
	err := b.pool.QueryRow(ctx, "UPDATE "+b.qualified("recall_leases")+` SET epoch = epoch + 1, holder = $3, expires_at = clock_timestamp() + $5 * interval '1 millisecond'
		WHERE collection = $1 AND name = $2 AND epoch = $4 RETURNING epoch`, collection, name, holder, int64(epoch), ttl.Milliseconds()).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return recall.LeaseGrant{}, recall.ErrLeaseLost
	}
	if err != nil {
		return recall.LeaseGrant{}, fmt.Errorf("pgvector: renew lease: %w", err)
	}
	return recall.LeaseGrant{Epoch: uint64(next), Holder: holder, TTL: ttl}, nil
}

// ReleaseLease ends a held lease at once, so the next Acquire need not wait
// for it to lapse. A lease already past the given epoch is left alone.
func (b *Backend) ReleaseLease(ctx context.Context, collection, name, holder string, epoch uint64) error {
	if ctx == nil {
		return fmt.Errorf("pgvector: context must not be nil")
	}
	if err := checkLease(collection, name, holder, time.Second); err != nil {
		return err
	}
	if err := b.ensureReady(ctx); err != nil {
		return err
	}
	_, err := b.pool.Exec(ctx, "UPDATE "+b.qualified("recall_leases")+" SET epoch = epoch + 1, expires_at = to_timestamp(0) WHERE collection = $1 AND name = $2 AND epoch = $3", collection, name, int64(epoch))
	if err != nil {
		return fmt.Errorf("pgvector: release lease: %w", err)
	}
	return nil
}

func checkLease(collection, name, holder string, ttl time.Duration) error {
	if err := checkCollection(collection); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(holder) == "" {
		return fmt.Errorf("pgvector: lease name and holder are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("pgvector: lease TTL must be positive")
	}
	return nil
}

// formatVector writes a vector in pgvector's text form.
func formatVector(values []float32) string {
	var sb strings.Builder
	sb.Grow(len(values) * 10)
	sb.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// parseVector reads pgvector's text form.
func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("pgvector: malformed vector %q", s)
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return []float32{}, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("pgvector: malformed vector component %q", p)
		}
		out[i] = float32(f)
	}
	return out, nil
}

var fieldRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// numberExpr is the SQL for a metadata field as a number, or NULL when the
// field is missing or not a number. Written the same way in the index and in
// every query, so the planner can match them.
func numberExpr(key string) string {
	return fmt.Sprintf("(CASE WHEN jsonb_typeof(metadata->'%s') = 'number' THEN (metadata->'%s')::float8 END)", key, key)
}

// translateFilter turns Recall's filter language into a WHERE clause and its
// arguments, numbered from first. Recall filters are a conjunction of fields,
// each either a scalar that must match exactly or an object of comparison
// operators ($eq, $ne, $gt, $gte, $lt, $lte, $in, $nin). Equality is jsonb
// containment, which the GIN index serves and which compares numbers by
// value; ranges compare the field as a number. An empty filter gives an
// empty clause.
func translateFilter(filter map[string]any, first int) (string, []any, error) {
	keys := make([]string, 0, len(filter))
	for k := range filter {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(first+len(args)-1)
	}
	contains := func(key string, v any) (string, error) {
		switch v.(type) {
		case string, bool:
		default:
			if _, ok := number(v); !ok {
				return "", fmt.Errorf("pgvector: filter %s needs a string, number, or bool, not %T", key, v)
			}
		}
		j, err := json.Marshal(map[string]any{key: v})
		if err != nil {
			return "", err
		}
		return "metadata @> " + arg(string(j)) + "::jsonb", nil
	}
	for _, key := range keys {
		if !fieldRE.MatchString(key) {
			return "", nil, fmt.Errorf("pgvector: filter field %q has characters outside letters, digits, _ . : -", key)
		}
		v := filter[key]
		ops, isOps := v.(map[string]any)
		if !isOps {
			c, err := contains(key, v)
			if err != nil {
				return "", nil, err
			}
			conds = append(conds, c)
			continue
		}
		opNames := make([]string, 0, len(ops))
		for op := range ops {
			opNames = append(opNames, op)
		}
		sort.Strings(opNames)
		for _, op := range opNames {
			a := ops[op]
			switch op {
			case "$eq", "$ne":
				c, err := contains(key, a)
				if err != nil {
					return "", nil, err
				}
				if op == "$ne" {
					c = "NOT (" + c + ")"
				}
				conds = append(conds, c)
			case "$gt", "$gte", "$lt", "$lte":
				n, ok := number(a)
				if !ok {
					return "", nil, fmt.Errorf("pgvector: filter %s %s needs a number", key, op)
				}
				cmp := map[string]string{"$gt": ">", "$gte": ">=", "$lt": "<", "$lte": "<="}[op]
				conds = append(conds, numberExpr(key)+" "+cmp+" "+arg(n)+"::float8")
			case "$in", "$nin":
				list, ok := a.([]any)
				if !ok || len(list) == 0 {
					return "", nil, fmt.Errorf("pgvector: filter %s %s needs a non-empty list", key, op)
				}
				alts := make([]string, 0, len(list))
				for _, x := range list {
					c, err := contains(key, x)
					if err != nil {
						return "", nil, err
					}
					alts = append(alts, c)
				}
				c := "(" + strings.Join(alts, " OR ") + ")"
				if op == "$nin" {
					c = "NOT " + c
				}
				conds = append(conds, c)
			default:
				return "", nil, fmt.Errorf("pgvector: unsupported filter operator %s on %s", op, key)
			}
		}
	}
	if len(conds) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
