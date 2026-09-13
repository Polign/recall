# Changelog

## [0.3.0] - 2026-09-12

- Default registry with 15 predicates for preferences, identity, and project facts.
- `RememberText` proposal pipeline: the calling agent supplies extracted facts
  and exact evidence quotes; the registry validates the entire batch before the
  fold resolves each write. Partial storage failures report completed results.
- Built-in, versioned lexical feature hashing for memory without an embedding
  service or a second model runtime. Exact typed memory requires no model.
- Optional bounded materialized belief view keyed by a backend-visible log
  watermark, with late-arrival, overwrite, and future-event invalidation.
- Polign HTTP watermark adapter; older servers fall back to uncached reads.
- Python client `polign-recall` v0.1.0 over the existing MCP stdio transport,
  with typed results, timeout handling, and no automatic write retries.
- Claude Code Recall plugin v0.1.0 in `Polign/polign`, using the memory-only
  MCP surface in Polign v0.6.4. That server release also contains #99's complete
  cold-history support.

The Go library's prior v0.1.0, v0.2.0, and v0.2.1 tags are preserved. Python and
the plugin have independent version lines. This release also includes the
context-aware client, public HTTP adapter, and audit bundles previously on main.
