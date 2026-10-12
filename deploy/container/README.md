# Recall container

`ghcr.io/polign/recall` holds the `recall` binary on a pinned distroless base,
running as UID 65532. The default command is `recall serve`; the same image
runs `recall connect` inside a workload. Build it from the repository root:

```sh
docker build -f deploy/container/Dockerfile --build-arg VERSION=$(git describe --tags --always) -t ghcr.io/polign/recall:dev .
```

`.github/workflows/container-release.yml` publishes the image when a release is
published, or by hand for a version that already exists (Actions, container-release,
Run workflow, `vX.Y.Z`). It builds from the tagged source, runs `smoke.sh` against
a real polign-server first, pushes both architectures with provenance and an
SBOM, and signs the digest. Verify a published image with Cosign:

```sh
cosign verify \
  --certificate-identity=https://github.com/Polign/recall/.github/workflows/container-release.yml@refs/tags/v0.16.0 \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/polign/recall:0.16.0
```

Run the smoke test locally against any image and a `polign-server` binary (the
`polign_db` pip package has one):

```sh
deploy/container/smoke.sh ghcr.io/polign/recall:dev "$(python -c 'import polign_db; print(polign_db.find_bin("polign-server"))')"
```

Kubernetes installation is in `charts/recall` of
[Polign/polign](https://github.com/Polign/polign); the agent-sandbox demo is in
`deploy/agent-sandbox`.
