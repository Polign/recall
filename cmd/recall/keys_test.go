package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// clock is a settable time source for the key store.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestKeyStore(t *testing.T, db *multiDB, cacheTTL time.Duration) (*keyStore, *clock) {
	t.Helper()
	ck := &clock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	k := newKeyStore(db, "memories", cacheTTL)
	k.now = ck.now
	return k, ck
}

func TestKeyID(t *testing.T) {
	token, id, err := newKeyToken()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := keyID(token)
	if !ok || got != id || len(id) != 16 {
		t.Fatalf("token %s: id %q ok %v (want %q)", token, got, ok, id)
	}
	for _, bad := range []string{"", "plgn_ab_cd", "rcl_", "rcl__x", "rcl_zz_secret", "rcl_abcd"} {
		if _, ok := keyID(bad); ok {
			t.Errorf("%q parsed as a key", bad)
		}
	}
}

func TestKeyStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newMultiDB()
	k, ck := newTestKeyStore(t, db, 0)
	token, rec, err := k.create(ctx, "acme:sandbox-1", "sandbox-1", "sandbox-1", "test", false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "rcl_") || rec.TTL != 15*time.Minute || rec.ExpiresAt != ck.t.Add(15*time.Minute) {
		t.Fatalf("token %s record %+v", token, rec)
	}
	p, err := k.Authenticate(ctx, token)
	if err != nil || p.Namespace != "acme:sandbox-1" || p.Agent != "sandbox-1" || p.Session != "sandbox-1" || p.KeyID != rec.ID || p.Admin {
		t.Fatalf("principal %+v, %v", p, err)
	}
	// Same id, wrong secret.
	if _, err := k.Authenticate(ctx, "rcl_"+rec.ID+"_notthesecret"); err == nil {
		t.Fatal("wrong secret accepted")
	}
	// Renewal moves the expiry out from now.
	ck.t = ck.t.Add(10 * time.Minute)
	renewed, err := k.renew(ctx, rec.ID)
	if err != nil || renewed.ExpiresAt != ck.t.Add(15*time.Minute) {
		t.Fatalf("renew: %+v %v", renewed, err)
	}
	// Past the expiry the key is refused.
	ck.t = ck.t.Add(16 * time.Minute)
	if _, err := k.Authenticate(ctx, token); err == nil {
		t.Fatal("expired key accepted")
	}
	ck.t = ck.t.Add(-10 * time.Minute)
	if _, err := k.Authenticate(ctx, token); err != nil {
		t.Fatalf("live key refused: %v", err)
	}
	// Revocation is final: a later rewrite of the record cannot undo it.
	if err := k.revoke(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Authenticate(ctx, token); err == nil {
		t.Fatal("revoked key accepted")
	}
	if _, err := k.renew(ctx, rec.ID); err == nil {
		t.Fatal("revoked key renewed")
	}
	live := rec
	live.ExpiresAt = ck.t.Add(time.Hour)
	if err := k.put(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Authenticate(ctx, token); err == nil {
		t.Fatal("tombstone lost to a rewritten record")
	}
	if err := k.revoke(ctx, rec.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if err := k.revoke(ctx, "0000000000000000"); err == nil {
		t.Fatal("revoking an unknown id succeeded")
	}
	// Listing shows state without hashes.
	admin, _, err := k.create(ctx, "", "", "", "ops", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := k.list(ctx)
	if err != nil || len(recs) != 2 {
		t.Fatalf("list: %d %v", len(recs), err)
	}
	if !recs[0].Revoked || recs[1].Revoked || !recs[1].Admin {
		t.Fatalf("list states: %+v", recs)
	}
	p, err = k.Authenticate(ctx, admin)
	if err != nil || !p.Admin || p.Namespace != "" {
		t.Fatalf("admin principal %+v %v", p, err)
	}
	if _, _, err := k.create(ctx, "acme", "", "", "", true, 0); err == nil {
		t.Fatal("admin key with a namespace accepted")
	}
	if _, _, err := k.create(ctx, ":bad", "", "", "", false, 0); err == nil {
		t.Fatal("bad namespace accepted")
	}
}

func TestKeyStoreCacheBoundsRevocation(t *testing.T) {
	ctx := context.Background()
	db := newMultiDB()
	a, ck := newTestKeyStore(t, db, 30*time.Second)
	b := newKeyStore(db, "memories", 0)
	b.now = ck.now
	token, rec, err := a.create(ctx, "acme", "", "", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}
	// Another server revokes; this one's cache still says yes until the TTL.
	if err := b.revoke(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, token); err != nil {
		t.Fatal("cached check refused inside the TTL; the cache is not doing its job")
	}
	ck.t = ck.t.Add(31 * time.Second)
	if _, err := a.Authenticate(ctx, token); err == nil {
		t.Fatal("revoked key still accepted after the cache TTL")
	}
}

func TestKeyStoreAdoptIsIdempotent(t *testing.T) {
	ctx := context.Background()
	k, _ := newTestKeyStore(t, newMultiDB(), 0)
	token, _, err := newKeyToken()
	if err != nil {
		t.Fatal(err)
	}
	rec, created, err := k.adopt(ctx, token, "", "bootstrap", true)
	if err != nil || !created || !rec.Admin {
		t.Fatalf("%+v %v %v", rec, created, err)
	}
	again, created, err := k.adopt(ctx, token, "acme", "rebind attempt", false)
	if err != nil || created || !again.Admin || again.Namespace != "" {
		t.Fatalf("adopt rebound the key: %+v %v %v", again, created, err)
	}
	if _, _, err := k.adopt(ctx, "plgn_x_y", "", "", true); err == nil {
		t.Fatal("foreign key adopted")
	}
}

func TestServeKeyEndpoints(t *testing.T) {
	ctx := context.Background()
	db := newMultiDB()
	keys, ck := newTestKeyStore(t, db, 0)
	admin, _, err := keys.create(ctx, "", "", "", "ops", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	tenants := newTenantServers(serveConfig{backend: db, registry: recall.DefaultRegistry(), embedder: recall.LexicalEmbedder{}, base: "memories"})
	srv := newTestServerWith(t, keys, tenants, keys)

	// An admin key manages keys and nothing else.
	status, body := post(t, srv.URL+"/v1/recall", admin, `{}`)
	if status != http.StatusForbidden {
		t.Fatalf("admin recall: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/keys", admin, `{"namespace":"acme:sandbox-1","agent":"sandbox-1","session":"sandbox-1","expires_in":"15m","note":"demo"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"key":"rcl_`) || strings.Contains(body, `"hash"`) {
		t.Fatalf("create: %d %s", status, body)
	}
	token := extractJSONString(t, body, "key")
	id := extractJSONString(t, body, "id")

	// The new key works in its namespace and cannot manage keys.
	if status, body := post(t, srv.URL+"/v1/remember", token, `{"subject":"user","predicate":"prefers_editor","value":"vim"}`); status != http.StatusOK {
		t.Fatalf("remember with minted key: %d %s", status, body)
	}
	if status, _ := post(t, srv.URL+"/v1/keys", token, `{"namespace":"globex"}`); status != http.StatusForbidden {
		t.Fatalf("non-admin minted a key: %d", status)
	}
	// It renews itself.
	ck.t = ck.t.Add(10 * time.Minute)
	status, body = post(t, srv.URL+"/v1/keys/renew", token, ``)
	if status != http.StatusOK || !strings.Contains(body, ck.t.Add(15*time.Minute).UTC().Format(time.RFC3339)) {
		t.Fatalf("renew: %d %s", status, body)
	}
	// Admin lists it, then revokes it.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	listed := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(listed, id) || strings.Contains(listed, "hash") {
		t.Fatalf("list: %d %s", resp.StatusCode, listed)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/keys/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if status, body := post(t, srv.URL+"/v1/recall", token, `{}`); status != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d %s", status, body)
	}
	if status, body := post(t, srv.URL+"/v1/keys", admin, `{"namespace":"acme","expires_in":"soon"}`); status != http.StatusBadRequest || !strings.Contains(body, "expires_in") {
		t.Fatalf("bad expires_in: %d %s", status, body)
	}
}
