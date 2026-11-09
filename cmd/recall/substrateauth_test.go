package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIssuer serves a discovery document and a key set over TLS, the way
// ate-idp-server does.
type fakeIssuer struct {
	srv     *httptest.Server
	keys    []map[string]any
	fetches atomic.Int32
	// claimIssuer overrides the issuer the discovery document names.
	claimIssuer string
	// down makes the key set answer 503.
	down atomic.Bool
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := f.srv.URL
		if f.claimIssuer != "" {
			iss = f.claimIssuer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "jwks_uri": f.srv.URL + "/openid/v1/jwks"})
	})
	mux.HandleFunc("/openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		if f.down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": f.keys})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// caFile writes the issuer's certificate where -substrate-ca-file reads it.
func (f *fakeIssuer) caFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIssuer) addEC(t *testing.T, kid string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	f.keys = append(f.keys, map[string]any{"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256", "x": b64(raw[1:33]), "y": b64(raw[33:])})
	return key
}

func (f *fakeIssuer) addRSA(t *testing.T, kid string) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.keys = append(f.keys, map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256", "n": b64(key.N.Bytes()), "e": b64([]byte{1, 0, 1})})
	return key
}

// signJWT signs claims with an ES256 or RS256 key.
func signJWT(t *testing.T, key crypto.Signer, alg, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": alg, "kid": kid})
	body, _ := json.Marshal(claims)
	signed := b64(header) + "." + b64(body)
	digest := sha256.Sum256([]byte(signed))
	var sig []byte
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	case *rsa.PrivateKey:
		var err error
		if sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:]); err != nil {
			t.Fatal(err)
		}
	}
	return signed + "." + b64(sig)
}

// actorToken is the claims Substrate mints for actor team-a/agent-1.
func actorToken(issuer string, now time.Time) map[string]any {
	return map[string]any{
		"iss": issuer,
		"sub": "actor/team-a/agent-1",
		"aud": []string{"recall"},
		"exp": now.Add(15 * time.Minute).Unix(),
		"nbf": now.Add(-5 * time.Minute).Unix(),
		"iat": now.Unix(),
		"actor.ate.dev": map[string]string{
			"atespace": "team-a", "actorName": "agent-1", "actorUID": "4f9c3a1e-8d2b-4c77-9e0a-6b5f1d2c3e4a",
		},
	}
}

func newTestSubstrateAuth(t *testing.T, f *fakeIssuer, scope string) *substrateAuth {
	t.Helper()
	s, err := newSubstrateAuth(substrateAuthConfig{issuer: f.srv.URL, caFile: f.caFile(t), scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSubstrateAuthAccepts(t *testing.T) {
	f := newFakeIssuer(t)
	ec := f.addEC(t, "ec-1")
	rs := f.addRSA(t, "rsa-1")
	now := time.Now()

	for _, tc := range []struct {
		name, scope, wantNS string
		token               string
	}{
		{"ES256", "", "team-a:agent-1", signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, now))},
		{"RS256", "actor", "team-a:agent-1", signJWT(t, rs, "RS256", "rsa-1", actorToken(f.srv.URL, now))},
		{"shared atespace", "atespace", "team-a", signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, now))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newTestSubstrateAuth(t, f, tc.scope).Authenticate(context.Background(), tc.token)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if p.Namespace != tc.wantNS || p.Agent != "agent-1" || p.Admin || p.Session != "" {
				t.Fatalf("principal = %+v, want namespace %q agent agent-1", p, tc.wantNS)
			}
		})
	}
}

func TestSubstrateAuthRejects(t *testing.T) {
	f := newFakeIssuer(t)
	ec := f.addEC(t, "ec-1")
	rs := f.addRSA(t, "rsa-1")
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	with := func(change func(c map[string]any)) map[string]any {
		c := actorToken(f.srv.URL, now)
		change(c)
		return c
	}
	good := signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, now))

	for _, tc := range []struct {
		name, token string
	}{
		{"not a JWT", "rk_abc123"},
		{"signed by another key", signJWT(t, other, "ES256", "ec-1", actorToken(f.srv.URL, now))},
		{"unknown kid", signJWT(t, ec, "ES256", "ec-9", actorToken(f.srv.URL, now))},
		{"algorithm does not match the key", signJWT(t, rs, "RS256", "ec-1", actorToken(f.srv.URL, now))},
		{"alg none", b64([]byte(`{"alg":"none","kid":"ec-1"}`)) + "." + good[len(b64([]byte(`{"typ":"JWT","alg":"ES256","kid":"ec-1"}`)))+1:]},
		{"tampered signature", good[:len(good)-70] + "A" + good[len(good)-69:]},
		{"another issuer", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["iss"] = "https://idp.elsewhere.svc" }))},
		{"another audience", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["aud"] = []string{"https://api.example.com"} }))},
		{"expired", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() }))},
		{"no expiry", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { delete(c, "exp") }))},
		{"not yet valid", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["nbf"] = now.Add(time.Minute).Unix() }))},
		{"worker-for-actor", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["sub"] = "worker-for-actor/team-a/agent-1" }))},
		{"subject and claim disagree", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["sub"] = "actor/team-b/agent-1" }))},
		{"no actor claim", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { delete(c, "actor.ate.dev") }))},
		{"extra subject segment", signJWT(t, ec, "ES256", "ec-1", with(func(c map[string]any) { c["sub"] = "actor/team-a/agent-1/x" }))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newTestSubstrateAuth(t, f, "").Authenticate(context.Background(), tc.token)
			if !errors.Is(err, errUnauthenticated) {
				t.Fatalf("Authenticate = %v, want errUnauthenticated", err)
			}
		})
	}
}

// A key added to the issuer is found by one refetch, and tokens naming keys
// the issuer does not publish do not refetch more often than the limit.
func TestSubstrateAuthKeyRefetch(t *testing.T) {
	f := newFakeIssuer(t)
	ec := f.addEC(t, "ec-1")
	s := newTestSubstrateAuth(t, f, "")
	clock := time.Now()
	s.now = func() time.Time { return clock }
	ctx := context.Background()

	if _, err := s.Authenticate(ctx, signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, clock))); err != nil {
		t.Fatalf("first token: %v", err)
	}
	next := f.addEC(t, "ec-2")
	rotated := signJWT(t, next, "ES256", "ec-2", actorToken(f.srv.URL, clock))
	if _, err := s.Authenticate(ctx, rotated); !errors.Is(err, errUnauthenticated) {
		t.Fatalf("new key inside the refetch limit = %v, want errUnauthenticated", err)
	}
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("key set fetched %d times, want 1", got)
	}
	clock = clock.Add(substrateKeyRefetch + time.Second)
	if _, err := s.Authenticate(ctx, rotated); err != nil {
		t.Fatalf("new key after the refetch limit: %v", err)
	}
	if got := f.fetches.Load(); got != 2 {
		t.Fatalf("key set fetched %d times, want 2", got)
	}
}

// An issuer that is down is asked again after the retry wait, and the keys the
// server already holds stay in use meanwhile.
func TestSubstrateAuthIssuerDown(t *testing.T) {
	f := newFakeIssuer(t)
	ec := f.addEC(t, "ec-1")
	s := newTestSubstrateAuth(t, f, "")
	clock := time.Now()
	s.now = func() time.Time { return clock }
	ctx := context.Background()
	token := signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, clock))

	f.down.Store(true)
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, errUnauthenticated) {
		t.Fatalf("issuer down = %v, want errUnauthenticated", err)
	}
	f.down.Store(false)
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, errUnauthenticated) {
		t.Fatalf("inside the retry wait = %v, want errUnauthenticated", err)
	}
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("key set fetched %d times, want 1", got)
	}
	clock = clock.Add(substrateKeyRetry + time.Second)
	if _, err := s.Authenticate(ctx, token); err != nil {
		t.Fatalf("after the retry wait: %v", err)
	}

	// Down again: a token naming an unknown key fails, the known key works.
	f.down.Store(true)
	clock = clock.Add(substrateKeyRefetch + time.Second)
	if _, err := s.Authenticate(ctx, signJWT(t, ec, "ES256", "ec-9", actorToken(f.srv.URL, clock))); !errors.Is(err, errUnauthenticated) {
		t.Fatalf("unknown key with the issuer down = %v, want errUnauthenticated", err)
	}
	if _, err := s.Authenticate(ctx, signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, clock))); err != nil {
		t.Fatalf("known key with the issuer down: %v", err)
	}
}

func TestSubstrateAuthDiscoveryIssuerMismatch(t *testing.T) {
	f := newFakeIssuer(t)
	ec := f.addEC(t, "ec-1")
	f.claimIssuer = "https://idp.elsewhere.svc"
	_, err := newTestSubstrateAuth(t, f, "").Authenticate(context.Background(), signJWT(t, ec, "ES256", "ec-1", actorToken(f.srv.URL, time.Now())))
	if !errors.Is(err, errUnauthenticated) {
		t.Fatalf("Authenticate = %v, want errUnauthenticated", err)
	}
}

func TestNewSubstrateAuthConfig(t *testing.T) {
	for _, tc := range []struct {
		issuer, scope string
		ok            bool
	}{
		{"https://idp.ate-system.svc", "", true},
		{"https://idp.example.com:8443", "atespace", true},
		{"http://idp.ate-system.svc", "", false},
		{"https://idp.ate-system.svc/path", "", false},
		{"https://idp.ate-system.svc?x=1", "", false},
		{"idp.ate-system.svc", "", false},
		{"https://idp.ate-system.svc", "cluster", false},
	} {
		_, err := newSubstrateAuth(substrateAuthConfig{issuer: tc.issuer, scope: tc.scope})
		if (err == nil) != tc.ok {
			t.Errorf("newSubstrateAuth(%q, %q) error = %v, want ok %v", tc.issuer, tc.scope, err, tc.ok)
		}
	}
}
