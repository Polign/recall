package main

// Kubernetes workload identity for recall serve.
//
// A pod proves who it is with a projected service account token whose
// audience is "recall". POST /v1/auth/kubernetes takes that token, asks the
// API server to review it (TokenReview, with the same audience), and mints a
// short-lived Recall key for the workload. Nothing in the key's identity comes
// from the client: the tenant is the service account's Kubernetes namespace,
// or a label the cluster admin put on the service account; the agent and
// session are the pod's name, which the API server reports in the token's
// extra fields for bound tokens. A pod can therefore only ever get a key for
// itself, and with it only ever resume itself.
//
// With agent-sandbox, a pod handed out by a warm pool has a generated name,
// so its name is a poor agent identity. -kubernetes-sandbox-claims follows the
// pod's controller owner references, pod -> Sandbox -> SandboxClaim, and uses
// the claim's name as the agent: deleting a claim and creating it again under
// the same name gives the new pod the old agent's memory. The API server
// writes owner references, so the pod cannot choose them.
//
// The server talks to the API server with its own mounted credentials over
// one HTTPS call per exchange, so no client library is needed.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultKubeTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a path, not a credential
	defaultKubeCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	podNameExtra         = "authentication.kubernetes.io/pod-name"
	sandboxGroup         = "agents.x-k8s.io"
	sandboxClaimGroup    = "extensions.agents.x-k8s.io"
	defaultKubeAudience  = "recall"
	defaultKubeKeyTTL    = 15 * time.Minute
)

// kubeAuth exchanges projected service account tokens for Recall keys.
type kubeAuth struct {
	apiServer string
	client    *http.Client
	// tokenFile holds the server's own credential for the API server; it is
	// read on every call because projected tokens rotate.
	tokenFile string
	audience  string
	// tenantLabel names a label on the service account that carries the
	// tenant. Empty means the service account's namespace is the tenant.
	tenantLabel string
	// agentLabel names a label on the pod that carries the agent's name.
	// Empty means the pod's name is the agent. A pool hands out pods with
	// generated names, so a platform that wants an agent to keep its identity
	// across claims labels the pod (SandboxClaim.additionalPodMetadata) and
	// names the label here. Whoever creates pods in the tenant's namespace
	// chooses the label, so it can only ever name one of the tenant's own
	// agents: the namespace stays <tenant>:<agent>.
	agentLabel string
	// sandboxClaims makes the agent the name of the SandboxClaim that owns
	// the pod's Sandbox, when there is one. It needs get on pods, sandboxes
	// and sandboxclaims.
	sandboxClaims bool
	keyTTL        time.Duration
	keys          *keyStore
}

type kubeAuthConfig struct {
	apiServer, caFile, tokenFile, audience, tenantLabel, agentLabel string
	sandboxClaims                                                   bool
	keyTTL                                                          time.Duration
}

func newKubeAuth(cfg kubeAuthConfig, keys *keyStore) (*kubeAuth, error) {
	if keys == nil {
		return nil, errors.New("kubernetes auth needs the key store")
	}
	apiServer := cfg.apiServer
	if apiServer == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST is unset; give -kubernetes-api")
		}
		if port == "" {
			port = "443"
		}
		apiServer = "https://" + host + ":" + port
	}
	transport := &http.Transport{}
	if cfg.caFile != "" {
		pem, err := os.ReadFile(cfg.caFile)
		if err != nil {
			return nil, fmt.Errorf("kubernetes CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("kubernetes CA: no certificates in %s", cfg.caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	ttl := cfg.keyTTL
	if ttl <= 0 {
		ttl = defaultKubeKeyTTL
	}
	audience := cfg.audience
	if audience == "" {
		audience = defaultKubeAudience
	}
	return &kubeAuth{
		apiServer:     strings.TrimRight(apiServer, "/"),
		client:        &http.Client{Transport: transport, Timeout: 10 * time.Second},
		tokenFile:     cfg.tokenFile,
		audience:      audience,
		tenantLabel:   cfg.tenantLabel,
		agentLabel:    cfg.agentLabel,
		sandboxClaims: cfg.sandboxClaims,
		keyTTL:        ttl,
		keys:          keys,
	}, nil
}

// workload is what a reviewed token says about its bearer.
type workload struct {
	namespace, serviceAccount, pod string
}

var errTokenRejected = errors.New("token rejected")

// errKubeNotFound is a 404 from the API server: the object is gone.
var errKubeNotFound = errors.New("not found")

// review asks the API server whether the token is a live service account
// token for our audience, bound to a pod.
func (k *kubeAuth) review(ctx context.Context, token string) (workload, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec":       map[string]any{"token": token, "audiences": []string{k.audience}},
	})
	var result struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			Error         string   `json:"error"`
			User          struct {
				Username string              `json:"username"`
				Extra    map[string][]string `json:"extra"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := k.call(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", body, &result); err != nil {
		return workload{}, err
	}
	st := result.Status
	if !st.Authenticated {
		return workload{}, fmt.Errorf("%w: %s", errTokenRejected, firstNonEmpty(st.Error, "not authenticated"))
	}
	// When audiences were requested the API server returns the ones the token
	// is valid for; an empty list means the token was not minted for us.
	if len(st.Audiences) > 0 && !contains(st.Audiences, k.audience) {
		return workload{}, fmt.Errorf("%w: token audience is not %q", errTokenRejected, k.audience)
	}
	parts := strings.Split(st.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return workload{}, fmt.Errorf("%w: %q is not a service account", errTokenRejected, st.User.Username)
	}
	pods := st.User.Extra[podNameExtra]
	if len(pods) != 1 || pods[0] == "" {
		return workload{}, fmt.Errorf("%w: the token is not bound to a pod; project it into the pod rather than using a long-lived secret", errTokenRejected)
	}
	return workload{namespace: parts[2], serviceAccount: parts[3], pod: pods[0]}, nil
}

// tenant decides which tenant a workload belongs to.
func (k *kubeAuth) tenant(ctx context.Context, w workload) (string, error) {
	if k.tenantLabel == "" {
		return w.namespace, nil
	}
	var sa struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	path := "/api/v1/namespaces/" + w.namespace + "/serviceaccounts/" + w.serviceAccount
	if err := k.call(ctx, http.MethodGet, path, nil, &sa); err != nil {
		return "", err
	}
	tenant := sa.Metadata.Labels[k.tenantLabel]
	if tenant == "" {
		return "", fmt.Errorf("%w: service account %s/%s carries no %s label", errTokenRejected, w.namespace, w.serviceAccount, k.tenantLabel)
	}
	return tenant, nil
}

// objectMeta is the part of a Kubernetes object the exchange reads.
type objectMeta struct {
	Metadata struct {
		UID             string            `json:"uid"`
		Labels          map[string]string `json:"labels"`
		OwnerReferences []ownerReference  `json:"ownerReferences"`
	} `json:"metadata"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

// controllerOf returns the object's controller owner reference, if any.
func controllerOf(obj objectMeta) (ownerReference, bool) {
	for _, ref := range obj.Metadata.OwnerReferences {
		if ref.Controller {
			return ref, true
		}
	}
	return ownerReference{}, false
}

// refGroup is the API group of an owner reference's apiVersion.
func refGroup(ref ownerReference) string {
	group, _, _ := strings.Cut(ref.APIVersion, "/")
	return group
}

// agent decides which agent a workload is. In order: the SandboxClaim that
// owns the pod's Sandbox when -kubernetes-sandbox-claims is on, the agent
// label on the pod when one is configured and present, the pod's name.
func (k *kubeAuth) agent(ctx context.Context, w workload) (string, error) {
	if k.agentLabel == "" && !k.sandboxClaims {
		return w.pod, nil
	}
	var pod objectMeta
	if err := k.call(ctx, http.MethodGet, "/api/v1/namespaces/"+w.namespace+"/pods/"+w.pod, nil, &pod); err != nil {
		return "", err
	}
	if k.sandboxClaims {
		claim, err := k.sandboxClaim(ctx, w, pod)
		if err != nil || claim != "" {
			return claim, err
		}
	}
	if k.agentLabel != "" {
		if name := pod.Metadata.Labels[k.agentLabel]; name != "" {
			return name, nil
		}
	}
	return w.pod, nil
}

// sandboxClaim follows pod -> Sandbox -> SandboxClaim and returns the claim's
// name. It returns "" when the pod is not a Sandbox's, or the Sandbox has no
// controller (created by hand), so the caller falls back. A Sandbox still held
// by a warm pool is refused: it has no agent yet, and a key minted now would
// carry the pool's generated name into the agent's memory. Each hop checks the
// UID the reference names, so a pod left over from a deleted claim does not
// get a key for a new claim of the same name.
func (k *kubeAuth) sandboxClaim(ctx context.Context, w workload, pod objectMeta) (string, error) {
	ref, ok := controllerOf(pod)
	if !ok || ref.Kind != "Sandbox" || refGroup(ref) != sandboxGroup {
		return "", nil
	}
	var sandbox objectMeta
	err := k.call(ctx, http.MethodGet, "/apis/"+ref.APIVersion+"/namespaces/"+w.namespace+"/sandboxes/"+ref.Name, nil, &sandbox)
	if err != nil && !errors.Is(err, errKubeNotFound) {
		return "", err
	}
	if err != nil || sandbox.Metadata.UID != ref.UID {
		return "", fmt.Errorf("%w: pod %s names Sandbox %s, which is gone or has been replaced", errTokenRejected, w.pod, ref.Name)
	}
	owner, ok := controllerOf(sandbox)
	if !ok {
		return "", nil
	}
	switch {
	case owner.Kind == "SandboxClaim" && refGroup(owner) == sandboxClaimGroup:
	case owner.Kind == "SandboxWarmPool" && refGroup(owner) == sandboxClaimGroup:
		return "", fmt.Errorf("%w: Sandbox %s is still in warm pool %s; exchange the token after it is claimed", errTokenRejected, ref.Name, owner.Name)
	default:
		return "", nil
	}
	var claim objectMeta
	err = k.call(ctx, http.MethodGet, "/apis/"+owner.APIVersion+"/namespaces/"+w.namespace+"/sandboxclaims/"+owner.Name, nil, &claim)
	if err != nil && !errors.Is(err, errKubeNotFound) {
		return "", err
	}
	if err != nil || claim.Metadata.UID != owner.UID {
		return "", fmt.Errorf("%w: Sandbox %s belongs to a deleted SandboxClaim %s", errTokenRejected, ref.Name, owner.Name)
	}
	return owner.Name, nil
}

// exchange turns a projected token into a Recall key for the workload.
func (k *kubeAuth) exchange(ctx context.Context, token string) (string, keyRecord, error) {
	w, err := k.review(ctx, token)
	if err != nil {
		return "", keyRecord{}, err
	}
	tenant, err := k.tenant(ctx, w)
	if err != nil {
		return "", keyRecord{}, err
	}
	agent, err := k.agent(ctx, w)
	if err != nil {
		return "", keyRecord{}, err
	}
	ns := tenant + ":" + agent
	if err := validateNamespace(ns); err != nil {
		return "", keyRecord{}, fmt.Errorf("%w: %v", errTokenRejected, err)
	}
	note := fmt.Sprintf("kubernetes %s/%s pod %s", w.namespace, w.serviceAccount, w.pod)
	// Never renewable: the workload exchanges its token again, so a key
	// copied out of the pod dies at its TTL and a deleted pod's key cannot
	// be extended by anyone.
	return k.keys.mint(ctx, keyRecord{Namespace: ns, Agent: agent, Session: w.pod, Note: note, TTL: k.keyTTL, RenewWith: "token"})
}

// call makes one authenticated request to the API server.
func (k *kubeAuth) call(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.apiServer+path, rd)
	if err != nil {
		return err
	}
	if k.tokenFile != "" {
		own, err := readFileTrimmed(k.tokenFile)
		if err != nil {
			return fmt.Errorf("kubernetes: own token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+string(own))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("kubernetes: %s %s: %w", method, path, errKubeNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("kubernetes: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("kubernetes: %s: %w", path, err)
	}
	return nil
}

// handleKubernetesAuth is POST /v1/auth/kubernetes. It is the one route
// without a bearer key: the body carries the pod's projected token.
func (f *frontDoor) handleKubernetesAuth(w http.ResponseWriter, r *http.Request) {
	if f.kube == nil {
		writeError(w, http.StatusNotFound, "not_found", "this server was not started with -kubernetes")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", `POST {"token": "<projected service account token>"}`)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Token string `json:"token"`
	}
	if err := strictArgs(mustReadAll(r.Body), &req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, "invalid_argument", "body must be {\"token\": \"<projected service account token>\"}")
		return
	}
	token, rec, err := f.kube.exchange(r.Context(), strings.TrimSpace(req.Token))
	switch {
	case errors.Is(err, errTokenRejected):
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	case err != nil:
		// The API server, not the caller, failed; say so without the detail.
		fmt.Fprintln(os.Stderr, "serve: kubernetes auth:", err)
		writeError(w, http.StatusBadGateway, "upstream", "could not verify the token with the API server")
		return
	}
	view := keyView(rec)
	view["key"] = token
	writeJSON(w, http.StatusCreated, view)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
