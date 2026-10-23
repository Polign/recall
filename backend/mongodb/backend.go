// Package mongodb implements Recall's Backend on MongoDB. Each Recall
// collection is one MongoDB collection, created on the first write, in a
// database of its own (recall by default) beside two bookkeeping
// collections: recall_collections, which maps collection names to MongoDB
// collections and counts each collection's writes, and recall_leases, which
// holds agent leases.
//
// It runs on any MongoDB 6.0 or later, Atlas or not. Vector search is exact:
// the cosine similarity is computed in the server over the events the
// filter keeps, which needs no search index and is right for agent memory,
// where a collection holds thousands of events, not millions.
//
// Metadata is stored twice: whole, as JSON, so it reads back exactly as it
// was written, and as a list of {k, v} pairs of its scalar fields, which is
// what filters match. Recall's field names may contain dots, which MongoDB
// would read as paths; as pair keys they are plain strings.
//
// On a replica set or a sharded cluster, an agent write fenced by its lease
// runs in a transaction that also writes the lease document, so a takeover
// and a late write conflict and only one commits. A standalone server has no
// transactions; the lease is then checked just before the write, which
// narrows the window to one request without closing it.
package mongodb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"

	"github.com/Polign/recall"
)

// DefaultDatabase is where the collections live unless the connection string
// or Config.Database names another.
const DefaultDatabase = "recall"

const (
	registryName = "recall_collections"
	leasesName   = "recall_leases"
)

// Config selects a server and a database.
type Config struct {
	// URL is a MongoDB connection string, such as
	// mongodb://user:password@localhost:27017/recall. Its path names the
	// database when Database is empty.
	URL string
	// Password overrides the connection string's password when set.
	Password string
	// Database is the database the collections are created in; empty means
	// the connection string's, then DefaultDatabase.
	Database string
	// Client is a connected client to use instead of opening one from URL.
	// The backend never disconnects a supplied client.
	Client *mongo.Client
}

// Backend is a reusable MongoDB client, safe for concurrent calls.
type Backend struct {
	client *mongo.Client
	owned  bool
	db     *mongo.Database

	mu          sync.Mutex
	ready       bool
	txn         bool // the deployment supports multi-document transactions
	collections map[string]collectionInfo
}

type collectionInfo struct {
	coll *mongo.Collection
	dim  int
}

var (
	_ recall.Backend           = (*Backend)(nil)
	_ recall.GetBackend        = (*Backend)(nil)
	_ recall.TextSearchBackend = (*Backend)(nil)
	_ recall.WatermarkBackend  = (*Backend)(nil)
	_ recall.LeaseBackend      = (*Backend)(nil)
	_ recall.FencedBackend     = (*Backend)(nil)
)

// Importing this package registers it as the "mongodb" backend. The binary
// reads the connection string from MONGODB_URI and a password from
// MONGODB_PASSWORD when its flags do not set them.
func init() {
	recall.RegisterBackend("mongodb", recall.BackendDriver{
		Open: func(o recall.BackendOptions) (recall.Backend, error) {
			return New(Config{URL: o.URL, Password: o.APIKey, Database: o.Params["database"]})
		},
		DefaultURL: "mongodb://localhost:27017/" + DefaultDatabase,
		URLEnv:     "MONGODB_URI",
		KeyEnv:     "MONGODB_PASSWORD",
		// Option on the connection string, for a URL whose path cannot name
		// the database: ?database=memory.
		Params: []string{"database"},
	})
}

var databaseRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// New validates the configuration and creates a backend. Connecting is
// lazy: the first operation reaches the server and creates the bookkeeping
// collections if they are missing.
func New(cfg Config) (*Backend, error) {
	client, owned := cfg.Client, cfg.Client == nil
	database := cfg.Database
	if owned {
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("mongodb: a connection string is required")
		}
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "mongodb" && u.Scheme != "mongodb+srv") {
			return nil, fmt.Errorf("mongodb: connection string must start with mongodb:// or mongodb+srv://")
		}
		if database == "" {
			database = strings.Trim(u.Path, "/")
		}
		opts := options.Client().ApplyURI(cfg.URL)
		if cfg.Password != "" {
			cred := options.Credential{Password: cfg.Password, PasswordSet: true}
			if opts.Auth != nil {
				cred = *opts.Auth
				cred.Password, cred.PasswordSet = cfg.Password, true
			}
			if u.User != nil && cred.Username == "" {
				cred.Username = u.User.Username()
			}
			opts.SetAuth(cred)
		}
		if client, err = mongo.Connect(opts); err != nil {
			return nil, fmt.Errorf("mongodb: client: %w", err)
		}
	}
	if database == "" {
		database = DefaultDatabase
	}
	if !databaseRE.MatchString(database) {
		return nil, fmt.Errorf("mongodb: database must be 1-63 letters, digits, '_' or '-', not %q", database)
	}
	return &Backend{client: client, owned: owned, db: client.Database(database), collections: map[string]collectionInfo{}}, nil
}

// Close disconnects a client the backend opened. It does nothing for a
// client supplied through Config.
func (b *Backend) Close() {
	if b.owned {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = b.client.Disconnect(ctx)
	}
}

// Database is the database the backend uses, for callers that want to read
// the collections themselves.
func (b *Backend) Database() *mongo.Database { return b.db }

// CollectionName is the MongoDB collection a Recall collection is stored
// in: "rc_" and the name itself when it is plain and short, and otherwise a
// cleaned form with a hash suffix, so distinct collections never share one.
// The mapping is also recorded in recall_collections.
func CollectionName(collection string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, collection)
	if clean == collection && len(clean) <= 100 {
		return "rc_" + clean
	}
	if len(clean) > 100 {
		clean = clean[:100]
	}
	sum := sha256.Sum256([]byte(collection))
	return "rc_" + clean + "_" + hex.EncodeToString(sum[:4])
}

func checkCollection(collection string) error {
	if strings.TrimSpace(collection) == "" || len(collection) > 512 {
		return fmt.Errorf("mongodb: collection is required and at most 512 characters")
	}
	return nil
}

// ensureReady creates the bookkeeping indexes once per process and learns
// whether the deployment can run transactions.
func (b *Backend) ensureReady(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready {
		return nil
	}
	var hello struct {
		SetName string `bson:"setName"`
		Msg     string `bson:"msg"`
	}
	if err := b.db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return fmt.Errorf("mongodb: connect: %w", err)
	}
	if _, err := b.db.Collection(registryName).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "coll", Value: 1}}, Options: options.Index().SetUnique(true),
	}); err != nil {
		return fmt.Errorf("mongodb: bootstrap: %w", err)
	}
	b.txn = hello.SetName != "" || hello.Msg == "isdbgrid"
	b.ready = true
	return nil
}

// Transactions reports whether the server supports multi-document
// transactions, which is what makes fenced writes atomic. It reaches the
// server on first use.
func (b *Backend) Transactions(ctx context.Context) (bool, error) {
	if err := b.ensureReady(ctx); err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.txn, nil
}

type registryDoc struct {
	Name     string `bson:"_id"`
	Coll     string `bson:"coll"`
	Dim      int    `bson:"dim"`
	Nonce    string `bson:"nonce"`
	Revision int64  `bson:"revision"`
}

// lookup finds a collection, or reports it missing without creating it.
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
	var reg registryDoc
	err := b.db.Collection(registryName).FindOne(ctx, bson.D{{Key: "_id", Value: collection}}).Decode(&reg)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return collectionInfo{}, false, nil
	}
	if err != nil {
		return collectionInfo{}, false, fmt.Errorf("mongodb: look up collection: %w", err)
	}
	info = collectionInfo{coll: b.db.Collection(reg.Coll), dim: reg.Dim}
	b.mu.Lock()
	b.collections[collection] = info
	b.mu.Unlock()
	return info, true, nil
}

// ensureCollection finds or creates a collection for vectors of dim.
func (b *Backend) ensureCollection(ctx context.Context, collection string, dim int) (collectionInfo, error) {
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || ok {
		return info, err
	}
	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return collectionInfo{}, err
	}
	reg := registryDoc{Name: collection, Coll: CollectionName(collection), Dim: dim, Nonce: hex.EncodeToString(nb[:])}
	// The registry document is the creation: whoever inserts it first wins,
	// and everyone else reads the winner's dimension.
	if _, err := b.db.Collection(registryName).InsertOne(ctx, reg); err != nil && !mongo.IsDuplicateKeyError(err) {
		return collectionInfo{}, fmt.Errorf("mongodb: register collection: %w", err)
	}
	if err := b.db.Collection(registryName).FindOne(ctx, bson.D{{Key: "_id", Value: collection}}).Decode(&reg); err != nil {
		return collectionInfo{}, fmt.Errorf("mongodb: register collection: %w", err)
	}
	coll := b.db.Collection(reg.Coll)
	// Creating an index that exists is a no-op, so racing creators agree.
	_, err = coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "md.k", Value: 1}, {Key: "md.v", Value: 1}}},
		{Keys: bson.D{{Key: "text", Value: "text"}}, Options: options.Index().SetDefaultLanguage("english").SetLanguageOverride("recall_language")},
	})
	if err != nil {
		return collectionInfo{}, fmt.Errorf("mongodb: create collection: %w", err)
	}
	info = collectionInfo{coll: coll, dim: reg.Dim}
	b.mu.Lock()
	b.collections[collection] = info
	b.mu.Unlock()
	return info, nil
}

// event is one stored event.
type event struct {
	ID   string    `bson:"_id"`
	V    []float64 `bson:"v"`
	Norm float64   `bson:"norm"`
	MD   []pair    `bson:"md"`
	M    string    `bson:"m"`
	Text string    `bson:"text"`
}

type pair struct {
	K string `bson:"k"`
	V any    `bson:"v"`
}

func newEvent(id string, values []float32, metadata map[string]any) (event, error) {
	raw, err := json.Marshal(metadataOrEmpty(metadata))
	if err != nil {
		return event{}, fmt.Errorf("mongodb: metadata: %w", err)
	}
	// Pairs are built from the JSON round trip, so what filters see is what
	// a reader gets back: numbers as float64, whatever type they were given.
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		return event{}, fmt.Errorf("mongodb: metadata: %w", err)
	}
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e := event{ID: id, V: make([]float64, len(values)), M: string(raw), MD: []pair{}}
	var sq float64
	for i, v := range values {
		e.V[i] = float64(v)
		sq += float64(v) * float64(v)
	}
	e.Norm = math.Sqrt(sq)
	for _, k := range keys {
		switch v := md[k].(type) {
		case string, bool, float64:
			e.MD = append(e.MD, pair{K: k, V: v})
		}
	}
	if t, ok := md[recall.TextField].(string); ok {
		e.Text = t
	}
	return e, nil
}

func (e event) stored() (recall.StoredVector, error) {
	md, err := decodeMetadata(e.ID, e.M)
	if err != nil {
		return recall.StoredVector{}, err
	}
	values := make([]float32, len(e.V))
	for i, v := range e.V {
		values[i] = float32(v)
	}
	return recall.StoredVector{ID: e.ID, Values: values, Metadata: md}, nil
}

func decodeMetadata(id, raw string) (map[string]any, error) {
	md := map[string]any{}
	if raw == "" {
		return md, nil
	}
	if err := json.Unmarshal([]byte(raw), &md); err != nil {
		return nil, fmt.Errorf("mongodb: event %q metadata is malformed: %w", id, err)
	}
	return md, nil
}

func metadataOrEmpty(md map[string]any) map[string]any {
	if md == nil {
		return map[string]any{}
	}
	return md
}

// Put upserts one immutable event ID, creating the collection on first use,
// then moves the collection's revision. The revision moves after the write,
// so a reader that caches by watermark can at worst cache new data under the
// old revision, which the next write's revision replaces, and never old data
// under the new one.
func (b *Backend) Put(ctx context.Context, collection, id string, values []float32, metadata map[string]any) error {
	return b.put(ctx, collection, id, values, metadata, nil)
}

// PutFenced is Put applied only while the agent's lease is still the
// writer's. On a deployment with transactions the write and an update of the
// lease document commit together, so a takeover that changes the lease
// conflicts with the write and one of them is retried or refused. Without
// transactions the lease is checked just before the write. A fence that does
// not hold is reported as recall.ErrLeaseLost with nothing written.
func (b *Backend) PutFenced(ctx context.Context, collection, id string, values []float32, metadata map[string]any, fence recall.Fence) error {
	if strings.TrimSpace(fence.Lease) == "" || strings.TrimSpace(fence.Holder) == "" || fence.Epoch == 0 {
		return fmt.Errorf("mongodb: a lease fence needs a lease name, a holder and an epoch")
	}
	return b.put(ctx, collection, id, values, metadata, &fence)
}

var errFenced = fmt.Errorf("%w: write fenced", recall.ErrLeaseLost)

// testHookFencedWrite, when set, runs between a fenced write's lease check
// and the write itself: the window a takeover must not get through.
var testHookFencedWrite func()

func (b *Backend) put(ctx context.Context, collection, id string, values []float32, metadata map[string]any, fence *recall.Fence) error {
	if ctx == nil {
		return fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("mongodb: event ID is required")
	}
	if len(values) == 0 {
		return fmt.Errorf("mongodb: refusing to store an event with no vector")
	}
	info, err := b.ensureCollection(ctx, collection, len(values))
	if err != nil {
		return err
	}
	if info.dim != len(values) {
		return fmt.Errorf("mongodb: collection %s holds %d-dimensional vectors, not %d; a new embedder needs a new collection", collection, info.dim, len(values))
	}
	e, err := newEvent(id, values, metadata)
	if err != nil {
		return err
	}
	write := func(ctx context.Context) error {
		if _, err := info.coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}}, e, options.Replace().SetUpsert(true)); err != nil {
			return fmt.Errorf("mongodb: put: %w", err)
		}
		if _, err := b.db.Collection(registryName).UpdateOne(ctx, bson.D{{Key: "_id", Value: collection}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "revision", Value: 1}}}}); err != nil {
			return fmt.Errorf("mongodb: put: %w", err)
		}
		return nil
	}
	if fence == nil {
		return write(ctx)
	}
	held := leaseHeldBy(collection, fence)
	if !b.txn {
		n, err := b.db.Collection(leasesName).CountDocuments(ctx, held)
		if err != nil {
			return fmt.Errorf("mongodb: check lease: %w", err)
		}
		if n == 0 {
			return errFenced
		}
		if testHookFencedWrite != nil {
			testHookFencedWrite()
		}
		return write(ctx)
	}
	sess, err := b.client.StartSession()
	if err != nil {
		return fmt.Errorf("mongodb: session: %w", err)
	}
	defer sess.EndSession(context.Background())
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		// Writing the lease document, not just reading it, is what makes a
		// concurrent takeover conflict with this transaction.
		r, err := b.db.Collection(leasesName).UpdateOne(ctx, held, bson.D{{Key: "$inc", Value: bson.D{{Key: "fenced_writes", Value: 1}}}})
		if err != nil {
			return nil, fmt.Errorf("mongodb: check lease: %w", err)
		}
		if r.MatchedCount == 0 {
			return nil, errFenced
		}
		if testHookFencedWrite != nil {
			testHookFencedWrite()
		}
		return nil, write(ctx)
	})
	return err
}

// leaseHeldBy matches the lease while it is still the fence's: the same
// holder, not released, and not older than the fence's epoch. Renewal moves
// the epoch on, so a newer epoch from the same holder is its own renewal
// racing the write.
func leaseHeldBy(collection string, f *recall.Fence) bson.D {
	return bson.D{
		{Key: "_id", Value: leaseID(collection, f.Lease)},
		{Key: "holder", Value: f.Holder},
		{Key: "epoch", Value: bson.D{{Key: "$gte", Value: int64(f.Epoch)}}},
		{Key: "expires_at", Value: bson.D{{Key: "$gt", Value: released}}},
	}
}

// Watermark is the collection's revision, which moves on every write, behind
// a nonce chosen when the collection was created, so a collection dropped and
// made again never repeats a revision.
func (b *Backend) Watermark(ctx context.Context, collection string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return "", err
	}
	if err := b.ensureReady(ctx); err != nil {
		return "", err
	}
	var reg registryDoc
	err := b.db.Collection(registryName).FindOne(ctx, bson.D{{Key: "_id", Value: collection}}).Decode(&reg)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "unwritten", nil
	}
	if err != nil {
		return "", fmt.Errorf("mongodb: watermark: %w", err)
	}
	return fmt.Sprintf("%s-%d", reg.Nonce, reg.Revision), nil
}

// List returns up to limit exact matches, in id order, and the exact total.
// With transactions both come from one snapshot.
func (b *Backend) List(ctx context.Context, collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	if ctx == nil {
		return nil, 0, fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > recall.MaxHistoryEvents+1 {
		return nil, 0, fmt.Errorf("mongodb: list limit must be in [1, %d]", recall.MaxHistoryEvents+1)
	}
	match, err := translateFilter(filter)
	if err != nil {
		return nil, 0, err
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.StoredVector{}, 0, err
	}
	var (
		out   []recall.StoredVector
		total int64
	)
	read := func(ctx context.Context) error {
		out = make([]recall.StoredVector, 0, min(limit, 1024))
		var err error
		if total, err = info.coll.CountDocuments(ctx, match); err != nil {
			return fmt.Errorf("mongodb: list: %w", err)
		}
		cur, err := info.coll.Find(ctx, match, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
		if err != nil {
			return fmt.Errorf("mongodb: list: %w", err)
		}
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			var e event
			if err := cur.Decode(&e); err != nil {
				return fmt.Errorf("mongodb: list: %w", err)
			}
			v, err := e.stored()
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return cur.Err()
	}
	if err := b.inSnapshot(ctx, read); err != nil {
		return nil, 0, err
	}
	return out, int(total), nil
}

// inSnapshot runs read in a snapshot transaction where the deployment has
// them, so its reads agree with each other, and directly otherwise.
func (b *Backend) inSnapshot(ctx context.Context, read func(context.Context) error) error {
	if !b.txn {
		return read(ctx)
	}
	sess, err := b.client.StartSession()
	if err != nil {
		return fmt.Errorf("mongodb: session: %w", err)
	}
	defer sess.EndSession(context.Background())
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) { return nil, read(ctx) },
		options.Transaction().SetReadConcern(readconcern.Snapshot()))
	return err
}

// Search returns the k nearest events by cosine similarity, computed exactly
// in the server over the events the filter keeps. Score is the similarity
// and Distance 1 minus it.
func (b *Backend) Search(ctx context.Context, collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("mongodb: search limit must be in [1, 10000]")
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("mongodb: a query vector is required")
	}
	match, err := translateFilter(filter)
	if err != nil {
		return nil, err
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.Hit{}, err
	}
	if info.dim != len(values) {
		return nil, fmt.Errorf("mongodb: collection %s holds %d-dimensional vectors, not %d", collection, info.dim, len(values))
	}
	q := make(bson.A, len(values))
	var sq float64
	for i, v := range values {
		q[i] = float64(v)
		sq += float64(v) * float64(v)
	}
	qnorm := math.Sqrt(sq)
	if qnorm == 0 {
		qnorm = 1
	}
	dot := bson.D{{Key: "$reduce", Value: bson.D{
		{Key: "input", Value: bson.D{{Key: "$zip", Value: bson.D{{Key: "inputs", Value: bson.A{"$v", q}}}}}},
		{Key: "initialValue", Value: 0.0},
		{Key: "in", Value: bson.D{{Key: "$add", Value: bson.A{"$$value", bson.D{{Key: "$multiply", Value: bson.A{
			bson.D{{Key: "$arrayElemAt", Value: bson.A{"$$this", 0}}},
			bson.D{{Key: "$arrayElemAt", Value: bson.A{"$$this", 1}}},
		}}}}}}},
	}}}
	score := bson.D{{Key: "$cond", Value: bson.A{
		bson.D{{Key: "$gt", Value: bson.A{"$norm", 0}}},
		bson.D{{Key: "$divide", Value: bson.A{dot, bson.D{{Key: "$multiply", Value: bson.A{"$norm", qnorm}}}}}},
		0.0,
	}}}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$project", Value: bson.D{{Key: "m", Value: 1}, {Key: "score", Value: score}}}},
		{{Key: "$sort", Value: bson.D{{Key: "score", Value: -1}, {Key: "_id", Value: 1}}}},
		{{Key: "$limit", Value: k}},
	}
	return b.hits(ctx, info.coll, pipeline, k, func(s float64) (float32, float32) { return float32(s), float32(1 - s) })
}

// SearchText ranks events by MongoDB's text search over the event text, any
// word matching. Score is the text score; Distance is 1/(1+Score). A query
// with no searchable words returns no hits.
func (b *Backend) SearchText(ctx context.Context, collection, text string, k int, filter map[string]any) ([]recall.Hit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("mongodb: search limit must be in [1, 10000]")
	}
	query := textQuery(text)
	if query == "" {
		return []recall.Hit{}, nil
	}
	match, err := translateFilter(filter)
	if err != nil {
		return nil, err
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.Hit{}, err
	}
	match = append(bson.D{{Key: "$text", Value: bson.D{{Key: "$search", Value: query}}}}, match...)
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$project", Value: bson.D{{Key: "m", Value: 1}, {Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}}},
		{{Key: "$sort", Value: bson.D{{Key: "score", Value: -1}, {Key: "_id", Value: 1}}}},
		{{Key: "$limit", Value: k}},
	}
	return b.hits(ctx, info.coll, pipeline, k, func(s float64) (float32, float32) { return float32(s), float32(1 / (1 + s)) })
}

func (b *Backend) hits(ctx context.Context, coll *mongo.Collection, pipeline mongo.Pipeline, k int, scored func(float64) (float32, float32)) ([]recall.Hit, error) {
	cur, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("mongodb: search: %w", err)
	}
	defer cur.Close(ctx)
	hits := make([]recall.Hit, 0, k)
	for cur.Next(ctx) {
		var row struct {
			ID    string  `bson:"_id"`
			M     string  `bson:"m"`
			Score float64 `bson:"score"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, fmt.Errorf("mongodb: search: %w", err)
		}
		md, err := decodeMetadata(row.ID, row.M)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(row.Score) {
			row.Score = 0
		}
		score, dist := scored(row.Score)
		hits = append(hits, recall.Hit{ID: row.ID, Score: score, Distance: dist, Metadata: md})
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("mongodb: search: %w", err)
	}
	return hits, nil
}

// textQuery turns free text into $text input that matches any of its words:
// the words joined by spaces, with quotes and a leading minus, which $text
// reads as a phrase and a negation, removed.
func textQuery(text string) string {
	var words []string
	for _, w := range strings.Fields(text) {
		w = strings.ReplaceAll(w, `"`, "")
		w = strings.TrimLeft(w, "-")
		if w != "" {
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

// Get reads events by id in request order; unknown ids are omitted.
func (b *Backend) Get(ctx context.Context, collection string, ids []string) ([]recall.StoredVector, error) {
	if ctx == nil {
		return nil, fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > recall.MaxGetEvents {
		return nil, fmt.Errorf("mongodb: get takes 1 to %d ids", recall.MaxGetEvents)
	}
	info, ok, err := b.lookup(ctx, collection)
	if err != nil || !ok {
		return []recall.StoredVector{}, err
	}
	cur, err := info.coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, fmt.Errorf("mongodb: get: %w", err)
	}
	defer cur.Close(ctx)
	byID := map[string]recall.StoredVector{}
	for cur.Next(ctx) {
		var e event
		if err := cur.Decode(&e); err != nil {
			return nil, fmt.Errorf("mongodb: get: %w", err)
		}
		v, err := e.stored()
		if err != nil {
			return nil, err
		}
		byID[e.ID] = v
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("mongodb: get: %w", err)
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

// --- leases ------------------------------------------------------------------

// released is the expiry a released lease is given: always in the past, and
// distinct from any real expiry, so a fence can tell a released lease from a
// live one.
var released = time.Unix(0, 0).UTC()

type leaseDoc struct {
	Holder  string    `bson:"holder"`
	Epoch   int64     `bson:"epoch"`
	Expires time.Time `bson:"expires_at"`
}

// leaseID is unambiguous for any collection and name: the collection's
// length comes first.
func leaseID(collection, name string) string {
	return fmt.Sprintf("%d:%s/%s", len(collection), collection, name)
}

// AcquireLease takes an agent's lease unless a live holder has it. Expiry is
// judged by the server's clock ($$NOW), never the caller's. The epoch only
// ever grows: a fresh lease starts at 1, and a lease taken over after its
// holder let it lapse continues from where it was.
func (b *Backend) AcquireLease(ctx context.Context, collection, name, holder string, ttl time.Duration) (recall.LeaseGrant, error) {
	if ctx == nil {
		return recall.LeaseGrant{}, fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkLease(collection, name, holder, ttl); err != nil {
		return recall.LeaseGrant{}, err
	}
	if err := b.ensureReady(ctx); err != nil {
		return recall.LeaseGrant{}, err
	}
	id := leaseID(collection, name)
	leases := b.db.Collection(leasesName)
	var tb [12]byte
	if _, err := rand.Read(tb[:]); err != nil {
		return recall.LeaseGrant{}, err
	}
	token := hex.EncodeToString(tb[:])
	// One atomic upsert on _id: the server decides whether the lease is free
	// (missing, or lapsed by its own clock) and, only then, takes it and
	// stamps this call's token. The caller holds the lease if the stored
	// token is its own.
	free := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "$eq", Value: bson.A{bson.D{{Key: "$type", Value: "$expires_at"}}, "missing"}}},
		bson.D{{Key: "$lte", Value: bson.A{"$expires_at", "$$NOW"}}},
	}}}
	pick := func(taken, kept any) bson.D {
		return bson.D{{Key: "$cond", Value: bson.A{"$$free", taken, kept}}}
	}
	update := mongo.Pipeline{{{Key: "$replaceWith", Value: bson.D{{Key: "$let", Value: bson.D{
		{Key: "vars", Value: bson.D{{Key: "free", Value: free}}},
		{Key: "in", Value: bson.D{
			{Key: "_id", Value: id},
			{Key: "holder", Value: pick(holder, "$holder")},
			{Key: "epoch", Value: pick(bson.D{{Key: "$add", Value: bson.A{bson.D{{Key: "$ifNull", Value: bson.A{"$epoch", int64(0)}}}, int64(1)}}}, "$epoch")},
			{Key: "expires_at", Value: pick(bson.D{{Key: "$add", Value: bson.A{"$$NOW", ttl.Milliseconds()}}}, "$expires_at")},
			{Key: "token", Value: pick(token, "$token")},
			{Key: "fenced_writes", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$fenced_writes", int64(0)}}}},
		}},
	}}}}}}
	var got struct {
		Holder  string    `bson:"holder"`
		Epoch   int64     `bson:"epoch"`
		Expires time.Time `bson:"expires_at"`
		Token   string    `bson:"token"`
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = leases.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}}, update,
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&got)
		// Two first acquirers can both try to insert; the loser retries and
		// finds the winner's lease.
		if !mongo.IsDuplicateKeyError(err) {
			break
		}
	}
	if err != nil {
		return recall.LeaseGrant{}, fmt.Errorf("mongodb: acquire lease: %w", err)
	}
	if got.Token != token {
		return recall.LeaseGrant{}, &recall.LeaseHeldError{Holder: got.Holder, Expires: got.Expires}
	}
	return recall.LeaseGrant{Epoch: uint64(got.Epoch), Holder: holder, TTL: ttl}, nil
}

// RenewLease extends a held lease and advances its epoch. A lease whose
// epoch has moved past the given one was lost to another holder.
func (b *Backend) RenewLease(ctx context.Context, collection, name, holder string, epoch uint64, ttl time.Duration) (recall.LeaseGrant, error) {
	if ctx == nil {
		return recall.LeaseGrant{}, fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkLease(collection, name, holder, ttl); err != nil {
		return recall.LeaseGrant{}, err
	}
	if err := b.ensureReady(ctx); err != nil {
		return recall.LeaseGrant{}, err
	}
	update := mongo.Pipeline{{{Key: "$set", Value: bson.D{
		{Key: "holder", Value: holder},
		{Key: "epoch", Value: bson.D{{Key: "$add", Value: bson.A{"$epoch", int64(1)}}}},
		{Key: "expires_at", Value: bson.D{{Key: "$add", Value: bson.A{"$$NOW", ttl.Milliseconds()}}}},
	}}}}
	var got leaseDoc
	err := b.db.Collection(leasesName).FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: leaseID(collection, name)}, {Key: "epoch", Value: int64(epoch)}},
		update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&got)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return recall.LeaseGrant{}, recall.ErrLeaseLost
	}
	if err != nil {
		return recall.LeaseGrant{}, fmt.Errorf("mongodb: renew lease: %w", err)
	}
	return recall.LeaseGrant{Epoch: uint64(got.Epoch), Holder: holder, TTL: ttl}, nil
}

// ReleaseLease ends a held lease at once, so the next Acquire need not wait
// for it to lapse. A lease already past the given epoch is left alone.
func (b *Backend) ReleaseLease(ctx context.Context, collection, name, holder string, epoch uint64) error {
	if ctx == nil {
		return fmt.Errorf("mongodb: context must not be nil")
	}
	if err := checkLease(collection, name, holder, time.Second); err != nil {
		return err
	}
	if err := b.ensureReady(ctx); err != nil {
		return err
	}
	_, err := b.db.Collection(leasesName).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: leaseID(collection, name)}, {Key: "epoch", Value: int64(epoch)}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "epoch", Value: int64(1)}}}, {Key: "$set", Value: bson.D{{Key: "expires_at", Value: released}}}})
	if err != nil {
		return fmt.Errorf("mongodb: release lease: %w", err)
	}
	return nil
}

func checkLease(collection, name, holder string, ttl time.Duration) error {
	if err := checkCollection(collection); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(holder) == "" {
		return fmt.Errorf("mongodb: lease name and holder are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("mongodb: lease TTL must be positive")
	}
	return nil
}

// --- filters -----------------------------------------------------------------

var fieldRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// translateFilter turns Recall's filter language into a MongoDB query over
// the metadata pairs. Recall filters are a conjunction of fields, each either
// a scalar that must match exactly or an object of comparison operators
// ($eq, $ne, $gt, $gte, $lt, $lte, $in, $nin). Every condition on a field is
// an $elemMatch on its pair, so the field name is a value, never a path.
// MongoDB compares numbers by value whatever their type, and a range only
// ever matches numbers. A missing field matches $ne and $nin, as it does on
// the other backends.
func translateFilter(filter map[string]any) (bson.D, error) {
	keys := make([]string, 0, len(filter))
	for k := range filter {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var conds bson.A
	elem := func(key string, v any) bson.D {
		return bson.D{{Key: "md", Value: bson.D{{Key: "$elemMatch", Value: bson.D{{Key: "k", Value: key}, {Key: "v", Value: v}}}}}}
	}
	not := func(key string, v any) bson.D {
		return bson.D{{Key: "md", Value: bson.D{{Key: "$not", Value: bson.D{{Key: "$elemMatch", Value: bson.D{{Key: "k", Value: key}, {Key: "v", Value: v}}}}}}}}
	}
	for _, key := range keys {
		if !fieldRE.MatchString(key) {
			return nil, fmt.Errorf("mongodb: filter field %q has characters outside letters, digits, _ . : -", key)
		}
		v := filter[key]
		ops, isOps := v.(map[string]any)
		if !isOps {
			s, err := scalar(key, v)
			if err != nil {
				return nil, err
			}
			conds = append(conds, elem(key, s))
			continue
		}
		opNames := make([]string, 0, len(ops))
		for op := range ops {
			opNames = append(opNames, op)
		}
		sort.Strings(opNames)
		var ranges bson.D
		for _, op := range opNames {
			a := ops[op]
			switch op {
			case "$eq", "$ne":
				s, err := scalar(key, a)
				if err != nil {
					return nil, err
				}
				if op == "$eq" {
					conds = append(conds, elem(key, s))
				} else {
					conds = append(conds, not(key, s))
				}
			case "$gt", "$gte", "$lt", "$lte":
				n, ok := number(a)
				if !ok {
					return nil, fmt.Errorf("mongodb: filter %s %s needs a number", key, op)
				}
				ranges = append(ranges, bson.E{Key: op, Value: n})
			case "$in", "$nin":
				list, ok := a.([]any)
				if !ok || len(list) == 0 {
					return nil, fmt.Errorf("mongodb: filter %s %s needs a non-empty list", key, op)
				}
				vals := make(bson.A, 0, len(list))
				for _, x := range list {
					s, err := scalar(key, x)
					if err != nil {
						return nil, err
					}
					vals = append(vals, s)
				}
				in := bson.D{{Key: "$in", Value: vals}}
				if op == "$in" {
					conds = append(conds, elem(key, in))
				} else {
					conds = append(conds, not(key, in))
				}
			default:
				return nil, fmt.Errorf("mongodb: unsupported filter operator %s on %s", op, key)
			}
		}
		if len(ranges) > 0 {
			conds = append(conds, elem(key, ranges))
		}
	}
	if len(conds) == 0 {
		return bson.D{}, nil
	}
	return bson.D{{Key: "$and", Value: conds}}, nil
}

// scalar checks a filter value and gives numbers one type, as they are
// stored.
func scalar(key string, v any) (any, error) {
	switch v.(type) {
	case string, bool:
		return v, nil
	}
	if n, ok := number(v); ok {
		return n, nil
	}
	return nil, fmt.Errorf("mongodb: filter %s needs a string, number, or bool, not %T", key, v)
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
