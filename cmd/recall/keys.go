package main

// Recall keys: the credentials `recall serve` issues and checks.
//
// A key is "rcl_<id>_<secret>". The store holds the SHA-256 of the whole
// key beside its namespace, so a leaked store yields no credentials. Records
// live in a control collection in the same backend as the memory, and need
// nothing from it beyond Put and Get:
//
//   - a key record is written once under "key:<id>";
//   - renewal rewrites that record with a later expiry;
//   - revocation writes "revoked:<id>", a tombstone that is never overwritten.
//
// Every check reads both. Last-writer-wins on the key record therefore cannot
// resurrect a revoked key, because the tombstone is a different record, so
// the store needs no compare-and-swap and works on every backend.
//
// Checks are cached for a bounded time, so a revoked or expired key is refused
// within that time rather than at once; the server prints the bound at start.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Polign/recall"
)

const keyPrefix = "rcl_"

// keyVector is the vector stored with every control record. The control
// collection holds no memory; the vector exists because every backend wants
// one, and it is non-zero because some refuse a zero vector.
var keyVector = func() []float32 { v := make([]float32, 256); v[0] = 1; return v }()

// keyRecord is what the store holds about one key.
type keyRecord struct {
	ID        string
	Hash      string
	Namespace string
	Agent     string
	Session   string
	Note      string
	Admin     bool
	CreatedAt time.Time
	// ExpiresAt is zero for a key that does not expire.
	ExpiresAt time.Time
	// TTL is the lifetime a renewal grants again; zero when the key does not
	// expire.
	TTL time.Duration
	// Revoked is set from the tombstone, not the record.
	Revoked bool
}

func (r keyRecord) metadata() map[string]any {
	md := map[string]any{
		"kind":       "key",
		"key_id":     r.ID,
		"hash":       r.Hash,
		"namespace":  r.Namespace,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if r.Agent != "" {
		md["agent"] = r.Agent
	}
	if r.Session != "" {
		md["session"] = r.Session
	}
	if r.Note != "" {
		md["note"] = r.Note
	}
	if r.Admin {
		md["admin"] = true
	}
	if !r.ExpiresAt.IsZero() {
		md["expires_at"] = r.ExpiresAt.UTC().Format(time.RFC3339Nano)
		md["ttl_ms"] = r.TTL.Milliseconds()
	}
	return md
}

func recordFromMetadata(md map[string]any) (keyRecord, error) {
	str := func(k string) string {
		s, _ := md[k].(string)
		return s
	}
	r := keyRecord{ID: str("key_id"), Hash: str("hash"), Namespace: str("namespace"), Agent: str("agent"), Session: str("session"), Note: str("note")}
	switch v := md["admin"].(type) {
	case bool:
		r.Admin = v
	case string:
		r.Admin = v == "true"
	}
	if r.ID == "" || r.Hash == "" {
		return r, errors.New("key record without id or hash")
	}
	var err error
	if r.CreatedAt, err = parseInstant(str("created_at")); err != nil {
		return r, err
	}
	if s := str("expires_at"); s != "" {
		if r.ExpiresAt, err = parseInstant(s); err != nil {
			return r, err
		}
	}
	switch v := md["ttl_ms"].(type) {
	case float64:
		r.TTL = time.Duration(v) * time.Millisecond
	case int64:
		r.TTL = time.Duration(v) * time.Millisecond
	case int:
		r.TTL = time.Duration(v) * time.Millisecond
	case string:
		var ms int64
		fmt.Sscan(v, &ms)
		r.TTL = time.Duration(ms) * time.Millisecond
	}
	return r, nil
}

func parseInstant(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// newKeyToken mints a fresh key and its record fields.
func newKeyToken() (token, id string, err error) {
	var raw [8 + 24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	id = hex.EncodeToString(raw[:8])
	secret := base64.RawURLEncoding.EncodeToString(raw[8:])
	return keyPrefix + id + "_" + secret, id, nil
}

// keyID extracts the id from "rcl_<id>_<secret>".
func keyID(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, keyPrefix)
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok || id == "" {
		return "", false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return id, true
}

var errKeyExists = errors.New("a key with this id already exists")

// keyStore keeps keys in a control collection and answers authentication.
type keyStore struct {
	backend    recall.Backend
	collection string
	cacheTTL   time.Duration
	now        func() time.Time

	mu    sync.Mutex
	cache map[string]cachedKey
}

type cachedKey struct {
	rec     keyRecord
	found   bool
	checked time.Time
}

func newKeyStore(backend recall.Backend, base string, cacheTTL time.Duration) *keyStore {
	return &keyStore{backend: backend, collection: base + "__keys", cacheTTL: cacheTTL, now: time.Now, cache: map[string]cachedKey{}}
}

// create mints a key for a principal and stores its record. The token is
// returned once and never stored.
func (k *keyStore) create(ctx context.Context, ns, agent, session, note string, admin bool, ttl time.Duration) (string, keyRecord, error) {
	if admin && ns != "" {
		return "", keyRecord{}, errors.New("an admin key carries no namespace")
	}
	if !admin {
		if err := validateNamespace(ns); err != nil {
			return "", keyRecord{}, err
		}
	}
	if ttl < 0 {
		return "", keyRecord{}, errors.New("expiry cannot be negative")
	}
	token, id, err := newKeyToken()
	if err != nil {
		return "", keyRecord{}, err
	}
	rec := keyRecord{ID: id, Hash: hashCredential(token), Namespace: ns, Agent: agent, Session: session, Note: note, Admin: admin, CreatedAt: k.now()}
	if ttl > 0 {
		rec.TTL = ttl
		rec.ExpiresAt = rec.CreatedAt.Add(ttl)
	}
	if err := k.put(ctx, rec); err != nil {
		return "", keyRecord{}, err
	}
	return token, rec, nil
}

// adopt stores a record for a token minted elsewhere, as a bootstrap key
// from a Secret. It is idempotent: a token already on record is left as it
// is, so a restart can never rebind an existing key.
func (k *keyStore) adopt(ctx context.Context, token, ns, note string, admin bool) (keyRecord, bool, error) {
	id, ok := keyID(token)
	if !ok {
		return keyRecord{}, false, fmt.Errorf("not a Recall key: want %s<id>_<secret>", keyPrefix)
	}
	if existing, found, err := k.lookup(ctx, id); err != nil {
		return keyRecord{}, false, err
	} else if found {
		return existing, false, nil
	}
	if !admin {
		if err := validateNamespace(ns); err != nil {
			return keyRecord{}, false, err
		}
	}
	rec := keyRecord{ID: id, Hash: hashCredential(token), Namespace: ns, Note: note, Admin: admin, CreatedAt: k.now()}
	if err := k.put(ctx, rec); err != nil {
		return keyRecord{}, false, err
	}
	return rec, true, nil
}

func (k *keyStore) put(ctx context.Context, rec keyRecord) error {
	if err := k.backend.Put(ctx, k.collection, "key:"+rec.ID, keyVector, rec.metadata()); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	k.forget(rec.ID)
	return nil
}

// revoke writes the tombstone. Revoking twice is harmless.
func (k *keyStore) revoke(ctx context.Context, id string) error {
	if _, found, err := k.lookup(ctx, id); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("no key %s", id)
	}
	md := map[string]any{"kind": "revoked", "key_id": id, "revoked_at": k.now().UTC().Format(time.RFC3339Nano)}
	if err := k.backend.Put(ctx, k.collection, "revoked:"+id, keyVector, md); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	k.forget(id)
	return nil
}

// renew grants the key its lifetime again from now. A key without an expiry
// has nothing to renew; a revoked key stays revoked.
func (k *keyStore) renew(ctx context.Context, id string) (keyRecord, error) {
	rec, found, err := k.lookup(ctx, id)
	if err != nil {
		return keyRecord{}, err
	}
	if !found {
		return keyRecord{}, fmt.Errorf("no key %s", id)
	}
	if rec.Revoked {
		return keyRecord{}, errors.New("the key is revoked")
	}
	if rec.TTL <= 0 {
		return keyRecord{}, errors.New("the key does not expire, so there is nothing to renew")
	}
	rec.ExpiresAt = k.now().Add(rec.TTL)
	if err := k.put(ctx, rec); err != nil {
		return keyRecord{}, err
	}
	return rec, nil
}

// lookup reads a key's record and tombstone from the store, bypassing the
// cache, and fills the cache with what it finds.
func (k *keyStore) lookup(ctx context.Context, id string) (keyRecord, bool, error) {
	getter, ok := k.backend.(recall.GetBackend)
	if !ok {
		return keyRecord{}, false, errors.New("key store: the backend cannot read records by id")
	}
	got, err := getter.Get(ctx, k.collection, []string{"key:" + id, "revoked:" + id})
	if err != nil {
		return keyRecord{}, false, fmt.Errorf("key store: %w", err)
	}
	var rec keyRecord
	found, revoked := false, false
	for _, v := range got {
		switch v.ID {
		case "key:" + id:
			r, err := recordFromMetadata(v.Metadata)
			if err != nil {
				return keyRecord{}, false, fmt.Errorf("key store: %s: %w", id, err)
			}
			rec, found = r, true
		case "revoked:" + id:
			revoked = true
		}
	}
	rec.Revoked = revoked
	k.mu.Lock()
	k.cache[id] = cachedKey{rec: rec, found: found, checked: k.now()}
	k.mu.Unlock()
	return rec, found, nil
}

func (k *keyStore) forget(id string) {
	k.mu.Lock()
	delete(k.cache, id)
	k.mu.Unlock()
}

// Authenticate checks a presented key against the store, through the cache.
func (k *keyStore) Authenticate(ctx context.Context, credential string) (principal, error) {
	id, ok := keyID(credential)
	if !ok {
		return principal{}, errUnauthenticated
	}
	k.mu.Lock()
	c, cached := k.cache[id]
	k.mu.Unlock()
	rec, found := c.rec, c.found
	if !cached || k.now().Sub(c.checked) > k.cacheTTL {
		var err error
		if rec, found, err = k.lookup(ctx, id); err != nil {
			return principal{}, err
		}
	}
	if !found {
		return principal{}, errUnauthenticated
	}
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hashCredential(credential))) != 1 {
		return principal{}, errUnauthenticated
	}
	if rec.Revoked {
		return principal{}, errUnauthenticated
	}
	if !rec.ExpiresAt.IsZero() && !k.now().Before(rec.ExpiresAt) {
		return principal{}, errUnauthenticated
	}
	return principal{Namespace: rec.Namespace, Agent: rec.Agent, Session: rec.Session, Admin: rec.Admin, KeyID: rec.ID}, nil
}

// list returns every key on record, oldest first, with revocations marked.
func (k *keyStore) list(ctx context.Context) ([]keyRecord, error) {
	keys, _, err := k.backend.List(ctx, k.collection, map[string]any{"kind": "key"}, recall.MaxExport)
	if err != nil {
		return nil, fmt.Errorf("key store: %w", err)
	}
	tombs, _, err := k.backend.List(ctx, k.collection, map[string]any{"kind": "revoked"}, recall.MaxExport)
	if err != nil {
		return nil, fmt.Errorf("key store: %w", err)
	}
	revoked := map[string]bool{}
	for _, t := range tombs {
		id, _ := t.Metadata["key_id"].(string)
		revoked[id] = true
	}
	out := make([]keyRecord, 0, len(keys))
	for _, v := range keys {
		r, err := recordFromMetadata(v.Metadata)
		if err != nil {
			continue
		}
		r.Revoked = revoked[r.ID]
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// authChain tries authenticators in order; the first that knows the
// credential wins, and the first that fails for another reason stops it.
type authChain []authenticator

func (c authChain) Authenticate(ctx context.Context, credential string) (principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(ctx, credential)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, errUnauthenticated) {
			return principal{}, err
		}
	}
	return principal{}, errUnauthenticated
}

// cmdKey manages keys straight against the backend: recall key create | list | revoke.
func cmdKey(c *api, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("key: want create, list or revoke")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("key "+sub, flag.ContinueOnError)
	collection := fs.String("collection", envOr("POLIGN_COLLECTION", "recall_lexical_v1"), "collection name prefix the server was started with")
	ns := fs.String("namespace", "", "namespace the key reaches (create)")
	agent := fs.String("agent", "", "fix the key to one agent_id (create)")
	session := fs.String("session", "", "session the key carries (create)")
	note := fs.String("note", "", "what the key is for (create)")
	admin := fs.Bool("admin", false, "mint an admin key, which manages keys over HTTP and reaches no namespace (create)")
	expires := fs.Duration("expires", 0, "lifetime, such as 15m or 168h; renewable; 0 never expires (create)")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	name := c.backend
	if name == "" {
		name = setupBackend
	}
	backend, err := recall.OpenBackend(name, recall.BackendOptions{URL: c.base, APIKey: c.key})
	if err != nil {
		return fmt.Errorf("key: backend: %w", err)
	}
	store := newKeyStore(backend, *collection, 0)
	ctx := context.Background()
	switch sub {
	case "create":
		if fs.NArg() != 0 {
			return fmt.Errorf("key create: unexpected arguments %v", fs.Args())
		}
		if !*admin && *ns == "" {
			return errors.New("key create: -namespace is required, or -admin")
		}
		token, rec, err := store.create(ctx, *ns, *agent, *session, *note, *admin, *expires)
		if err != nil {
			return fmt.Errorf("key create: %w", err)
		}
		fmt.Fprintf(out, "key: %s\nid: %s\n", token, rec.ID)
		if rec.Admin {
			fmt.Fprintln(out, "admin: true")
		} else {
			fmt.Fprintf(out, "namespace: %s\n", rec.Namespace)
		}
		if !rec.ExpiresAt.IsZero() {
			fmt.Fprintf(out, "expires: %s\n", rec.ExpiresAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(os.Stderr, "The key is shown once; the store holds only its hash.")
		return nil
	case "list":
		recs, err := store.list(ctx)
		if err != nil {
			return fmt.Errorf("key list: %w", err)
		}
		for _, r := range recs {
			state := "active"
			switch {
			case r.Revoked:
				state = "revoked"
			case !r.ExpiresAt.IsZero() && !time.Now().Before(r.ExpiresAt):
				state = "expired"
			}
			scope := r.Namespace
			if r.Admin {
				scope = "(admin)"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", r.ID, state, scope, r.CreatedAt.UTC().Format(time.RFC3339), r.Note)
		}
		return nil
	case "revoke":
		if fs.NArg() != 1 {
			return errors.New("key revoke: want one key id")
		}
		if err := store.revoke(ctx, fs.Arg(0)); err != nil {
			return fmt.Errorf("key revoke: %w", err)
		}
		fmt.Fprintf(out, "revoked %s\n", fs.Arg(0))
		return nil
	}
	return fmt.Errorf("key: unknown subcommand %q (want create, list or revoke)", sub)
}
