# Developing Recall

The repository root exposes the Go API through `recall.go`. Memory behavior and
its unit tests live together in `internal/engine/`. Start there when changing
Recall's semantics.

## Find the implementation

Paths below are relative to `internal/engine/`.

| Area | Files |
| --- | --- |
| Context-aware client and backend interfaces | `client.go` |
| Memory writes, retractions, reads, and history | `store.go` |
| Events, beliefs, and deterministic folding | `event.go` |
| Predicate registry and typed values | `predicate.go`, `defaults.go` |
| Stored metadata and validation | `codec.go`, `validation.go` |
| Broad exact-query discovery | `discovery.go` |
| Optional materialized reads | `materialized.go` |
| Model proposal validation | `extract.go` |
| Event exports, checksums, and audit replay | `export.go`, `audit.go` |

Tests sit beside the implementation. History, retraction, and concurrency tests
exercise behavior across these areas. The shared JSON fixtures stay in
`testdata/` at the repository root because both the engine and audit CLI use them.

The Go Polign adapter is in `polign/`, the audit CLI in `cmd/recall-audit/`,
and the Python client in `python/`. Runnable integrations are in `examples/`.

## Public API boundary

Applications continue to import `github.com/Polign/recall` and
`github.com/Polign/recall/polign`. The root package uses type aliases and forwarding
functions; behavior belongs in the engine. When adding a public engine symbol,
expose it in `recall.go` and document it in the [reference](reference.md).

Aliases retain fields, interfaces, and method sets. Concrete types are defined
in `internal/engine`, so reflection reports that package as their defining path.
Use the public API rather than depending on reflected package names. Detailed
implementation documentation is available with `go doc -all ./internal/engine`.

The root package's external tests exercise the public API, while the adapter,
CLI, and Go example also compile against it. Keep new tests next to the package
they exercise; Go tests that need engine internals belong in `internal/engine/`.

## Run checks

From the repository root, with Go 1.25+:

```sh
go build ./...
go vet ./...
gofmt -l .
go test -race -timeout 5m ./...
```

For the Python client, with Python 3.10+:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install ./python
.venv/bin/python -m unittest discover -s python/tests -v
```

The default suites need no running database. The Go adapter tests start local
HTTP servers. Python's optional live integration test requires
`RECALL_TEST_POLIGN` (the CLI executable) and `POLIGN_URL`.

To run only the memory implementation or verify an audit fixture:

```sh
go test ./internal/engine
go run ./cmd/recall-audit < testdata/audit-v1.json
```
