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
	keyTTL     time.Duration
	keys       *keyStore
}

type kubeAuthConfig struct {
	apiServer, caFile, tokenFile, audience, tenantLabel, agentLabel string
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
		apiServer:   strings.TrimRight(apiServer, "/"),
		client:      &http.Client{Transport: transport, Timeout: 10 * time.Second},
		tokenFile:   cfg.tokenFile,
		audience:    audience,
		tenantLabel: cfg.tenantLabel,
		agentLabel:  cfg.agentLabel,
		keyTTL:      ttl,
		keys:        keys,
	}, nil
}

// workload is what a reviewed token says about its bearer.
type workload struct {
	namespace, serviceAccount, pod string
}

var errTokenRejected = errors.New("token rejected")

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

// agent decides which agent a workload is: the pod's name, or the value of
// the agent label on the pod when one is configured and present.
func (k *kubeAuth) agent(ctx context.Context, w workload) (string, error) {
	if k.agentLabel == "" {
		return w.pod, nil
	}
	var pod struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := k.call(ctx, http.MethodGet, "/api/v1/namespaces/"+w.namespace+"/pods/"+w.pod, nil, &pod); err != nil {
		return "", err
	}
	if name := pod.Metadata.Labels[k.agentLabel]; name != "" {
		return name, nil
	}
	return w.pod, nil
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
