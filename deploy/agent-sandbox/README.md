# Recall inside a Kubernetes Agent Sandbox

[agent-sandbox](https://agent-sandbox.sigs.k8s.io/) runs an AI agent as a
`Sandbox`: one isolated, stateful pod with a stable name, on gVisor or Kata
where the cluster offers it. Sandboxes are disposable. Everything the agent
learned goes with the pod unless memory lives outside it.

This directory puts Recall outside it. The sandbox pod carries one projected
service account token with audience `recall` and nothing else. It posts that
token to `recall serve`, which verifies it with the API server and mints a
short-lived key for `<tenant>:<sandbox name>`, fixed to that agent. Memory
written with the key outlives the sandbox; a sandbox recreated with the same
name resumes where it stopped. No Secret is written per sandbox, no webhook
injects anything, and nothing a tenant controls decides who it is: the tenant
is the pod's namespace (or a label the cluster admin put on the service
account), and the sandbox name comes from the token itself.

## What is here

| File | What it is |
| --- | --- |
| `namespace.yaml` | The tenant namespace `agents` and the `runner` service account. |
| `sandbox.yaml` | One `Sandbox` created directly, with the projected token. |
| `template.yaml` | The platform shape: `SandboxTemplate`, `SandboxWarmPool`, `SandboxClaim`. |
| `recall-values.yaml` | Chart values: polign_db backend, token exchange on. |
| `demo.sh` | Runs inside the sandbox with curl: exchange, remember, recall, refused. |
| `Makefile` | The whole demo on kind. |

## Run it on kind

Needs docker, kind, kubectl and helm. The recall chart comes from
`oci://ghcr.io/polign/charts/recall`; set `RECALL_CHART=../../../polign/charts/recall`
to test a local checkout of it instead.

```sh
make demo
```

That creates a cluster, installs agent-sandbox v1.0.6, builds the recall
image from this checkout, installs polign_db on a local volume and `recall
serve` on top of it with Kubernetes token exchange on, creates the sandbox,
and runs `demo.sh` inside it. The last lines show the current belief with the
value it replaced, and an HTTP 403 for a namespace the sandbox does not own.

Then delete the sandbox and create it again:

```sh
kubectl -n agents delete sandbox demo-1
kubectl -n agents apply -f sandbox.yaml
kubectl -n agents wait --for=condition=Ready pod/demo-1 --timeout=180s
kubectl -n agents exec demo-1 -- sh -c 'curl -sS -H "Content-Type: application/json" -d "{\"token\":\"$(cat $RECALL_TOKEN_FILE)\"}" $RECALL_URL/v1/auth/kubernetes'
```

The new pod gets a new key for the same namespace, `agents:demo-1`, and the
memory is still there. `make clean` removes the cluster.

## What the first run showed

Measured on kind (Kubernetes v1.35 node, runc, Apple Silicon), 2026-10-11,
with the recall image already on the node:

| What | Result |
| --- | --- |
| Fresh `Sandbox` applied to pod Ready | 0.5 s |
| Token exchange (TokenReview plus key mint), 20 calls from the sandbox | p50 2.4 ms, max 3.3 ms |
| recall over REST, 40 calls from the sandbox | p50 1.0 ms, p90 1.3 ms, p99 2.7 ms |
| Revoked key refused | at once with one replica (the server drops its own cache entry); within the 30 s auth cache TTL across replicas |
| recall serve resident memory, polign-server resident memory | 17 MB, 14 MB on the demo's handful of records |

Deleting `demo-1` and applying it again gave a new pod and a new key for the
same namespace; the editor preference and the agent's working state were
there, and `agent_resume` picked up where the first pod left off. A key fixed
to `demo-1` could not resume `run-1`.

Two things about agent-sandbox v1.0.6 shaped the manifests:

- **Templated sandboxes are isolated from the cluster by default.** With
  `networkPolicyManagement: Managed` and no `networkPolicy`, the controller
  writes a policy that allows egress to public addresses only and points the
  pod at public DNS resolvers, so an in-cluster Recall cannot even be resolved.
  `template.yaml` carries an explicit policy that opens cluster DNS and the
  `recall` Service and nothing else; the controller then gives the pod cluster
  DNS. A directly created `Sandbox` has no policy and needs nothing.
- **A claim's pod carries the pool's generated name.** The token says which
  pod is calling, so the identity would change with every claim. The claim
  labels its pod with the agent's name (`sandbox.users.io/agent: run-1`; the
  controller allowlists the label domains a claim may set, default
  `sandbox.users.io`) and the recall chart's `kubernetes.agentLabel` tells the
  server to use it. The key is then for `agents:run-1`, the session stays the
  pod, and a tenant can only name its own agents because the label lives in
  its namespace.

## On a real cluster

Point `recall-values.yaml` at your store (the polign chart with a bucket, or
Qdrant), set `runtimeClassName` in the pod spec to the sandbox runtime your
cluster offers, and replace the curl image with your agent's image running
`recall connect` (MCP over stdio) or using `polign_recall.Client.from_env()`.
Both read `RECALL_URL` and `RECALL_TOKEN_FILE` and need nothing else.

## Limitations

- A deleted sandbox's key stays valid for its TTL (15 minutes by default) unless revoked through `/v1/keys`; it cannot be renewed, since workload keys are only ever re-exchanged with the pod's token. A revoked key is still accepted for the server's auth cache TTL (30 seconds) on replicas other than the one that revoked it.
- Keys are not rotated in flight; a workload exchanges a new one.
- The resume tools need a backend with leases: polign_db today.
- Tenants map to Kubernetes namespaces unless the cluster admin controls service account labels and sets `kubernetes.tenantLabel`.
- Every token exchange mints a key. `recall connect` and the Python client exchange once and renew; a loop that exchanges on every call leaves keys behind until they expire.
