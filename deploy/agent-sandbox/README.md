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

Needs docker, kind, kubectl and helm, and a checkout of
[Polign/polign](https://github.com/Polign/polign) beside this repository for
the `recall` chart.

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

## On a real cluster

Point `recall-values.yaml` at your store (the polign chart with a bucket, or
Qdrant), set `runtimeClassName` in the pod spec to the sandbox runtime your
cluster offers, and replace the curl image with your agent's image running
`recall connect` (MCP over stdio) or using `polign_recall.Client.from_env()`.
Both read `RECALL_URL` and `RECALL_TOKEN_FILE` and need nothing else.

## Limitations

- A deleted sandbox's key stays valid for its TTL (15 minutes by default) unless revoked through `/v1/keys`; a revoked key is still accepted for the server's auth cache TTL (30 seconds).
- Keys are not rotated in flight; a workload exchanges a new one.
- The resume tools need a backend with leases: polign_db today.
- Tenants map to Kubernetes namespaces unless the cluster admin controls service account labels and sets `kubernetes.tenantLabel`.
