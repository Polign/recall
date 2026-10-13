package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// fakeAPIServer answers TokenReview and ServiceAccount reads the way the
// Kubernetes API server does, for the tokens it is told about.
type fakeAPIServer struct {
	t *testing.T
	// reviews maps a pod token to the TokenReview status to return.
	reviews map[string]map[string]any
	// labels maps "namespace/serviceaccount" to its labels.
	labels map[string]map[string]string
	// podLabels maps "namespace/pod" to its labels.
	podLabels map[string]map[string]string
	// podOwners maps "namespace/pod" to its owner references.
	podOwners map[string][]ownerReference
	// objects maps an /apis/... path to the object metadata served there.
	objects map[string]map[string]any
	// wantOwnToken is the bearer the server must present.
	wantOwnToken string
	gotAudiences []string
}

func (f *fakeAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.wantOwnToken {
		http.Error(w, "forbidden", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
		var req struct {
			Spec struct {
				Token     string   `json:"token"`
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotAudiences = req.Spec.Audiences
		status, ok := f.reviews[req.Spec.Token]
		if !ok {
			status = map[string]any{"authenticated": false, "error": "token not found"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/")
		if len(parts) != 3 {
			http.NotFound(w, r)
			return
		}
		var labels map[string]string
		var ok bool
		switch parts[1] {
		case "serviceaccounts":
			labels, ok = f.labels[parts[0]+"/"+parts[2]]
		case "pods":
			labels, ok = f.podLabels[parts[0]+"/"+parts[2]]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		meta := map[string]any{"labels": labels}
		if parts[1] == "pods" {
			meta["ownerReferences"] = f.podOwners[parts[0]+"/"+parts[2]]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": meta})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/"):
		meta, ok := f.objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": meta})
	default:
		http.NotFound(w, r)
	}
}

func saStatus(ns, sa, pod string, audiences ...string) map[string]any {
	st := map[string]any{
		"authenticated": true,
		"user": map[string]any{
			"username": "system:serviceaccount:" + ns + ":" + sa,
			"extra":    map[string][]string{podNameExtra: {pod}},
		},
	}
	if pod == "" {
		st["user"].(map[string]any)["extra"] = map[string][]string{}
	}
	if len(audiences) > 0 {
		st["audiences"] = audiences
	}
	return st
}

func newKubeTest(t *testing.T, tenantLabel string) (*httptest.Server, *fakeAPIServer, *keyStore) {
	return newKubeTestWith(t, tenantLabel, "")
}

func newKubeTestWith(t *testing.T, tenantLabel, agentLabel string) (*httptest.Server, *fakeAPIServer, *keyStore) {
	return newKubeTestConfig(t, kubeAuthConfig{tenantLabel: tenantLabel, agentLabel: agentLabel})
}

func newKubeTestConfig(t *testing.T, cfg kubeAuthConfig) (*httptest.Server, *fakeAPIServer, *keyStore) {
	t.Helper()
	api := &fakeAPIServer{t: t, wantOwnToken: "own-token",
		labels:    map[string]map[string]string{"agents/runner": {"recall.polign.com/tenant": "acme"}, "agents/unlabeled": {}},
		podLabels: map[string]map[string]string{"agents/pool-x7k2q": {"recall.polign.com/agent": "run-1"}, "agents/sandbox-1": {}}}
	api.reviews = map[string]map[string]any{
		"pod-token":        saStatus("agents", "runner", "sandbox-1", "recall"),
		"other-aud":        saStatus("agents", "runner", "sandbox-1", "https://kubernetes.default.svc"),
		"unbound":          saStatus("agents", "runner", ""),
		"no-label":         saStatus("agents", "unlabeled", "sandbox-2", "recall"),
		"not-a-sa":         {"authenticated": true, "user": map[string]any{"username": "alice", "extra": map[string][]string{podNameExtra: {"p"}}}},
		"weird-pod-name":   saStatus("agents", "runner", "bad pod", "recall"),
		"no-audience-list": saStatus("agents", "runner", "sandbox-3"),
		"pooled":           saStatus("agents", "runner", "pool-x7k2q", "recall"),
	}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	dir := t.TempDir()
	own := filepath.Join(dir, "token")
	if err := os.WriteFile(own, []byte("own-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := newMultiDB()
	keys, _ := newTestKeyStore(t, db, 0)
	cfg.apiServer, cfg.tokenFile, cfg.keyTTL = apiSrv.URL, own, 5*time.Minute
	kube, err := newKubeAuth(cfg, keys)
	if err != nil {
		t.Fatal(err)
	}
	tenants := newTenantServers(serveConfig{backend: db, registry: recall.DefaultRegistry(), embedder: recall.LexicalEmbedder{}, base: "memories", agents: true})
	door := newFrontDoor(keys, tenants, keys)
	door.kube = kube
	srv := httptest.NewServer(door)
	t.Cleanup(srv.Close)
	return srv, api, keys
}

func TestKubernetesExchangeMintsAWorkloadKey(t *testing.T) {
	srv, api, _ := newKubeTest(t, "")
	status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pod-token"}`)
	if status != http.StatusCreated {
		t.Fatalf("exchange: %d %s", status, body)
	}
	if len(api.gotAudiences) != 1 || api.gotAudiences[0] != "recall" {
		t.Fatalf("review asked for audiences %v", api.gotAudiences)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	if view["namespace"] != "agents:sandbox-1" || view["agent"] != "sandbox-1" || view["session"] != "sandbox-1" || view["ttl"] != "5m0s" {
		t.Fatalf("identity is not server-derived: %v", view)
	}
	key := view["key"].(string)
	// The key works in its namespace, and only as its own agent.
	if status, body := post(t, srv.URL+"/v1/remember", key, `{"subject":"user","predicate":"prefers_editor","value":"vim"}`); status != http.StatusOK {
		t.Fatalf("remember: %d %s", status, body)
	}
	if status, body := post(t, srv.URL+"/v1/recall", key, `{"namespace":"agents:sandbox-9","subject":"user","predicate":"prefers_editor"}`); status != http.StatusForbidden {
		t.Fatalf("other namespace: %d %s", status, body)
	}
	out, isErr := rpc(t, srv, key, "agent_resume", `{"agent_id":"sandbox-9"}`)
	if !isErr || !strings.Contains(out, "namespace_mismatch") {
		t.Fatalf("resumed another agent: %v %s", isErr, out)
	}
	// It cannot renew itself: the pod exchanges its token again instead, so
	// a key copied out of the pod dies at its TTL.
	if !strings.Contains(body, `"renewable":false`) {
		t.Fatalf("workload key should say it is not renewable: %s", body)
	}
	if status, body := post(t, srv.URL+"/v1/keys/renew", key, ``); status != http.StatusConflict || !strings.Contains(body, "renew_with_token") {
		t.Fatalf("renew of a workload key: %d %s", status, body)
	}
}

func TestKubernetesExchangeRefusals(t *testing.T) {
	srv, _, _ := newKubeTest(t, "")
	cases := map[string]string{
		"unknown":          `{"token":"nope"}`,
		"wrong audience":   `{"token":"other-aud"}`,
		"not bound":        `{"token":"unbound"}`,
		"not a sa":         `{"token":"not-a-sa"}`,
		"bad pod name":     `{"token":"weird-pod-name"}`,
		"empty":            `{"token":""}`,
		"unexpected field": `{"token":"pod-token","namespace":"acme"}`,
	}
	for name, body := range cases {
		status, out := post(t, srv.URL+"/v1/auth/kubernetes", "", body)
		if status != http.StatusUnauthorized && status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, status, out)
		}
		if strings.Contains(out, `"key"`) {
			t.Errorf("%s: a key was minted: %s", name, out)
		}
	}
	// A token whose review lists no audiences (an older API server) is
	// accepted on the authenticated flag alone.
	if status, out := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"no-audience-list"}`); status != http.StatusCreated {
		t.Errorf("no audience list: %d %s", status, out)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/auth/kubernetes", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}
}

func TestKubernetesTenantFromServiceAccountLabel(t *testing.T) {
	srv, _, _ := newKubeTest(t, "recall.polign.com/tenant")
	status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pod-token"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"acme:sandbox-1"`) {
		t.Fatalf("labeled: %d %s", status, body)
	}
	status, body = post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"no-label"}`)
	if status != http.StatusUnauthorized || !strings.Contains(body, "label") {
		t.Fatalf("unlabeled service account: %d %s", status, body)
	}
}

func TestKubernetesAuthOffByDefault(t *testing.T) {
	srv := newTestFrontDoor(t, staticAuth{})
	status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pod-token"}`)
	if status != http.StatusNotFound || !strings.Contains(body, "-kubernetes") {
		t.Fatalf("%d %s", status, body)
	}
}

func TestKubernetesAgentFromPodLabel(t *testing.T) {
	srv, _, _ := newKubeTestWith(t, "", "recall.polign.com/agent")
	// A pooled pod with the label is the agent the label names; the session
	// stays the pod, so the record still says which pod did the work.
	status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pooled"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:run-1"`) || !strings.Contains(body, `"agent":"run-1"`) || !strings.Contains(body, `"session":"pool-x7k2q"`) {
		t.Fatalf("pooled: %d %s", status, body)
	}
	// A pod without the label falls back to its own name.
	status, body = post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pod-token"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:sandbox-1"`) {
		t.Fatalf("unlabeled: %d %s", status, body)
	}
	// Without the option the pool's name is the identity.
	srv, _, _ = newKubeTest(t, "")
	status, body = post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pooled"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:pool-x7k2q"`) {
		t.Fatalf("no label option: %d %s", status, body)
	}
}

// sandboxRef and friends build owner references the way the agent-sandbox
// controllers write them.
func sandboxRef(name, uid string) ownerReference {
	return ownerReference{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: uid, Controller: true}
}

func extensionRef(kind, name, uid string) map[string]any {
	return map[string]any{"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": kind, "name": name, "uid": uid, "controller": true}
}

const (
	sandboxesPath = "/apis/agents.x-k8s.io/v1beta1/namespaces/agents/sandboxes/"
	claimsPath    = "/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/agents/sandboxclaims/"
)

func TestKubernetesAgentFromSandboxClaim(t *testing.T) {
	srv, api, _ := newKubeTestConfig(t, kubeAuthConfig{sandboxClaims: true, agentLabel: "recall.polign.com/agent"})
	api.reviews["claimed"] = saStatus("agents", "runner", "pool-a1b2c", "recall")
	api.reviews["warm"] = saStatus("agents", "runner", "pool-d3e4f", "recall")
	api.reviews["bare"] = saStatus("agents", "runner", "bare-sandbox", "recall")
	api.reviews["orphan"] = saStatus("agents", "runner", "pool-g5h6i", "recall")
	api.reviews["stale-claim"] = saStatus("agents", "runner", "pool-j7k8l", "recall")
	api.reviews["gone-sandbox"] = saStatus("agents", "runner", "pool-m9n0p", "recall")
	for _, pod := range []string{"pool-a1b2c", "pool-d3e4f", "bare-sandbox", "pool-g5h6i", "pool-j7k8l", "pool-m9n0p"} {
		api.podLabels["agents/"+pod] = map[string]string{}
	}
	api.podLabels["agents/bare-sandbox"] = map[string]string{"recall.polign.com/agent": "hand-made"}
	api.podOwners = map[string][]ownerReference{
		"agents/pool-a1b2c":   {sandboxRef("pool-a1b2c", "sb-1")},
		"agents/pool-d3e4f":   {sandboxRef("pool-d3e4f", "sb-2")},
		"agents/bare-sandbox": {sandboxRef("bare-sandbox", "sb-3")},
		"agents/pool-g5h6i":   {sandboxRef("pool-g5h6i", "sb-4")},
		"agents/pool-j7k8l":   {sandboxRef("pool-j7k8l", "sb-5")},
		"agents/pool-m9n0p":   {sandboxRef("pool-m9n0p", "sb-6")},
	}
	api.objects = map[string]map[string]any{
		sandboxesPath + "pool-a1b2c":   {"uid": "sb-1", "ownerReferences": []any{extensionRef("SandboxClaim", "run-1", "claim-1")}},
		sandboxesPath + "pool-d3e4f":   {"uid": "sb-2", "ownerReferences": []any{extensionRef("SandboxWarmPool", "recall-agents", "pool-1")}},
		sandboxesPath + "bare-sandbox": {"uid": "sb-3"},
		sandboxesPath + "pool-g5h6i":   {"uid": "sb-4", "ownerReferences": []any{extensionRef("SandboxClaim", "deleted", "claim-2")}},
		sandboxesPath + "pool-j7k8l":   {"uid": "sb-5", "ownerReferences": []any{extensionRef("SandboxClaim", "run-1", "claim-0")}},
		sandboxesPath + "pool-m9n0p":   {"uid": "sb-other"},
		claimsPath + "run-1":           {"uid": "claim-1"},
	}

	// A claimed pod is the agent its claim names; the session stays the pod.
	status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"claimed"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:run-1"`) || !strings.Contains(body, `"session":"pool-a1b2c"`) {
		t.Fatalf("claimed: %d %s", status, body)
	}
	// A Sandbox with no controller falls back to the agent label.
	status, body = post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"bare"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:hand-made"`) {
		t.Fatalf("bare sandbox: %d %s", status, body)
	}
	// A pod with no Sandbox owner falls back to its own name.
	status, body = post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pod-token"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:sandbox-1"`) {
		t.Fatalf("no owner: %d %s", status, body)
	}
	// Still in the warm pool, or tied to a claim or Sandbox that is gone or
	// was replaced: refused, no key.
	for _, token := range []string{"warm", "orphan", "stale-claim", "gone-sandbox"} {
		status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"`+token+`"}`)
		if status != http.StatusUnauthorized || strings.Contains(body, `"key"`) {
			t.Errorf("%s: %d %s", token, status, body)
		}
	}
	if _, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"warm"}`); !strings.Contains(body, "warm pool") {
		t.Errorf("warm pool refusal should say why: %s", body)
	}
}

func TestKubernetesSandboxClaimsOffIgnoresOwners(t *testing.T) {
	srv, api, _ := newKubeTest(t, "")
	api.podOwners = map[string][]ownerReference{"agents/sandbox-1": {sandboxRef("sandbox-1", "sb-1")}}
	// Without -kubernetes-sandbox-claims the server reads nothing past the
	// TokenReview, so no Sandbox lookup can fail it.
	status, body := post(t, srv.URL+"/v1/auth/kubernetes", "", `{"token":"pod-token"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"namespace":"agents:sandbox-1"`) {
		t.Fatalf("%d %s", status, body)
	}
}
