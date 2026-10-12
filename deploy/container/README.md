# Recall container

`ghcr.io/polign/recall` holds the `recall` binary on a pinned distroless base,
running as UID 65532. The default command is `recall serve`; the same image
runs `recall connect` inside a workload. Build it from the repository root:

```sh
docker build -f deploy/container/Dockerfile --build-arg VERSION=$(git describe --tags --always) -t ghcr.io/polign/recall:dev .
```

Kubernetes installation is in `charts/recall` of
[Polign/polign](https://github.com/Polign/polign); the agent-sandbox demo is in
`deploy/agent-sandbox`.
