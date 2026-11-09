package main

// Agent Substrate actor identity for recall serve.
//
// An actor on Agent Substrate holds no credential. Its egress policy tells the
// egress gateway to replace the Authorization header of a request to Recall
// with a short-lived JWT that Substrate minted for that actor:
//
//	rules:
//	- http:
//	    hostnames: ["recall.recall-system.svc"]
//	    effects:
//	      replaceHeaders:
//	      - header: Authorization
//	        prefix: "Bearer "
//	        actorJwt: {audiences: ["recall"], expirationSeconds: 900}
//
// So the token is the bearer on every request, and there is no exchange for a
// Recall key: a key would live in the actor's memory and be written into its
// snapshots. The token is checked against the issuer's OpenID Connect
// discovery document and key set, the way Substrate's "Actor JWTs" guide
// describes, with the standard library only.
//
// Nothing in the principal comes from the actor. The subject is
// actor/<atespace>/<name>; the namespace is <atespace>:<name>, or the atespace
// alone when the server is started to share memory across an atespace's
// actors. The subject names the actor and not one incarnation of it, so an
// actor deleted and created again under the same name reaches the same memory.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultSubstrateAudience = "recall"
	substrateScopeActor      = "actor"
	substrateScopeAtespace   = "atespace"
	// substrateKeyRefetch is the least time between two fetches of the key
	// set, so tokens naming unknown keys cannot make the server hammer the
	// issuer. A new key is published before it signs, so one refetch finds it.
	substrateKeyRefetch = 30 * time.Second
	// maxActorJWT bounds a presented token. A Substrate token with 16
	// audiences of 512 bytes is under 12 KiB.
	maxActorJWT = 16 << 10
)

// substrateAuth verifies Agent Substrate actor JWTs.
type substrateAuth struct {
	issuer   string
	audience string
	// shared makes the namespace the atespace, so its actors share memory.
	shared bool
	// caFile holds the CA bundle for the issuer's certificate. It is read on
	// every fetch because a projected trust bundle rotates. Empty uses the
	// system roots.
	caFile string
	now    func() time.Time

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

type substrateAuthConfig struct {
	issuer, audience, caFile, scope string
}

func newSubstrateAuth(cfg substrateAuthConfig) (*substrateAuth, error) {
	u, err := url.Parse(cfg.issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("issuer %q: want https://<host> with no path, as Substrate's ACTOR_JWT_ISSUER is set", cfg.issuer)
	}
	audience := cfg.audience
	if audience == "" {
		audience = defaultSubstrateAudience
	}
	var shared bool
	switch cfg.scope {
	case "", substrateScopeActor:
	case substrateScopeAtespace:
		shared = true
	default:
		return nil, fmt.Errorf("scope %q: want %s or %s", cfg.scope, substrateScopeActor, substrateScopeAtespace)
	}
	return &substrateAuth{
		issuer:   strings.TrimRight(cfg.issuer, "/"),
		audience: audience,
		shared:   shared,
		caFile:   cfg.caFile,
		now:      time.Now,
	}, nil
}

// actorClaims is the part of an actor JWT the server reads.
type actorClaims struct {
	Iss   string       `json:"iss"`
	Sub   string       `json:"sub"`
	Aud   jwtAudiences `json:"aud"`
	Exp   *int64       `json:"exp"`
	Nbf   *int64       `json:"nbf"`
	Actor *struct {
		Atespace  string `json:"atespace"`
		ActorName string `json:"actorName"`
		ActorUID  string `json:"actorUID"`
	} `json:"actor.ate.dev"`
}

// jwtAudiences reads "aud" as a string or a list of strings; Substrate always
// sends a list.
type jwtAudiences []string

func (a *jwtAudiences) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Authenticate checks a bearer that looks like a JWT. Anything else, and any
// token that does not verify, is errUnauthenticated, so the next authenticator
// in the chain may still know the credential. A failure to reach the issuer is
// logged: the caller only ever sees that the credential was refused.
func (s *substrateAuth) Authenticate(ctx context.Context, credential string) (principal, error) {
	if len(credential) > maxActorJWT || strings.Count(credential, ".") != 2 {
		return principal{}, errUnauthenticated
	}
	parts := strings.Split(credential, ".")
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Kid == "" {
		return principal{}, errUnauthenticated
	}
	var claims actorClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return principal{}, errUnauthenticated
	}
	// The issuer is checked before any key is fetched, so a token from
	// somewhere else costs no request.
	if claims.Iss != s.issuer {
		return principal{}, errUnauthenticated
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return principal{}, errUnauthenticated
	}
	key, err := s.key(ctx, header.Kid)
	if err != nil {
		log.Printf("serve: substrate: %v", err)
		return principal{}, errUnauthenticated
	}
	if key == nil || !verifyJWTSignature(key, header.Alg, parts[0]+"."+parts[1], sig) {
		return principal{}, errUnauthenticated
	}
	now := s.now().Unix()
	if claims.Exp == nil || now >= *claims.Exp {
		return principal{}, errUnauthenticated
	}
	if claims.Nbf != nil && now < *claims.Nbf {
		return principal{}, errUnauthenticated
	}
	if !contains(claims.Aud, s.audience) {
		return principal{}, errUnauthenticated
	}
	// The same keys sign worker-for-actor tokens, which a Substrate component
	// uses when acting for an actor. Only the actor itself reaches its memory.
	rest, ok := strings.CutPrefix(claims.Sub, "actor/")
	if !ok {
		return principal{}, errUnauthenticated
	}
	atespace, name, ok := strings.Cut(rest, "/")
	if !ok || atespace == "" || name == "" || strings.Contains(name, "/") {
		return principal{}, errUnauthenticated
	}
	if claims.Actor == nil || claims.Actor.Atespace != atespace || claims.Actor.ActorName != name {
		return principal{}, errUnauthenticated
	}
	ns := atespace + ":" + name
	if s.shared {
		ns = atespace
	}
	if err := validateNamespace(ns); err != nil {
		return principal{}, errUnauthenticated
	}
	return principal{Namespace: ns, Agent: name}, nil
}

func decodeSegment(seg string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// verifyJWTSignature checks an ES256 or RS256 signature, the two algorithms
// Substrate signs with. The key's type must match the algorithm the header
// names, so a token cannot pick how its key is used.
func verifyJWTSignature(key crypto.PublicKey, alg, signed string, sig []byte) bool {
	digest := sha256.Sum256([]byte(signed))
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if alg != "ES256" || len(sig) != 64 {
			return false
		}
		return ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	case *rsa.PublicKey:
		if alg != "RS256" {
			return false
		}
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil
	}
	return false
}

// key returns the issuer's key with this id, or nil when the issuer publishes
// none. An unknown id refetches the key set, at most once per
// substrateKeyRefetch.
func (s *substrateAuth) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[kid]; ok {
		return k, nil
	}
	if !s.fetched.IsZero() && s.now().Sub(s.fetched) < substrateKeyRefetch {
		return nil, nil
	}
	keys, err := s.fetchKeys(ctx)
	// A failed fetch counts too: an issuer that is down is not asked again on
	// every request.
	s.fetched = s.now()
	if err != nil {
		return nil, err
	}
	s.keys = keys
	return keys[kid], nil
}

// fetchKeys reads the discovery document and then the key set it names.
func (s *substrateAuth) fetchKeys(ctx context.Context) (map[string]crypto.PublicKey, error) {
	client, err := s.client()
	if err != nil {
		return nil, err
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, client, s.issuer+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, err
	}
	if doc.Issuer != s.issuer {
		return nil, fmt.Errorf("discovery document names issuer %q, not %q", doc.Issuer, s.issuer)
	}
	if !strings.HasPrefix(doc.JWKSURI, "https://") {
		return nil, fmt.Errorf("discovery document names key set %q, which is not https", doc.JWKSURI)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, client, doc.JWKSURI, &set); err != nil {
		return nil, err
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := k.publicKey()
		if err != nil {
			// One key this server cannot read does not stop the others.
			log.Printf("serve: substrate: key %q: %v", k.Kid, err)
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s publishes no usable keys", doc.JWKSURI)
	}
	return keys, nil
}

func (s *substrateAuth) client() (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.caFile != "" {
		pem, err := os.ReadFile(s.caFile)
		if err != nil {
			return nil, fmt.Errorf("issuer CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("issuer CA: no certificates in %s", s.caFile)
		}
		cfg.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}, nil
}

func getJSON(ctx context.Context, client *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	return nil
}

// jwk is one entry of a JSON Web Key Set.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("curve %q is not P-256", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != 32 {
			return nil, errors.New("bad x coordinate")
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil || len(y) != 32 {
			return nil, errors.New("bad y coordinate")
		}
		// Parsing the uncompressed point checks that it is on the curve.
		return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil || len(n) < 256 {
			return nil, errors.New("bad or short modulus")
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad exponent")
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
	}
	return nil, fmt.Errorf("key type %q is not EC or RSA", k.Kty)
}
