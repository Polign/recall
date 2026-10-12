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
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"labels": labels}})
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
	kube, err := newKubeAuth(kubeAuthConfig{apiServer: apiSrv.URL, tokenFile: own, tenantLabel: tenantLabel, agentLabel: agentLabel, keyTTL: 5 * time.Minute}, keys)
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
	// And it renews itself.
	if status, body := post(t, srv.URL+"/v1/keys/renew", key, ``); status != http.StatusOK {
		t.Fatalf("renew: %d %s", status, body)
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
