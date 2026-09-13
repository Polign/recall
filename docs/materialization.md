# Materialized beliefs

`Config.Materialize: true` enables a bounded, process-local belief view for
backends implementing `WatermarkBackend`. `polign mcp` enables it by default.

Each entry contains the subject/predicate, folded beliefs, an opaque visible-log
watermark, the fold's as-of time, and the next future event time if any. A client
owns its view and immutable registry; clients never share entries across
credentials, collections, registries, or backends. At most 1,024 pairs are retained.

An unchanged watermark lets a repeated exact pair read return its materialized
beliefs without listing or replaying history. A changed watermark triggers a
complete validated history read, then a second watermark check. A change during
that read returns `ErrIncompleteHistory` rather than publishing a mixed view.
Backdated events and same-ID overwrites invalidate the view. A future event's
observation time expires an otherwise unchanged view. Earlier historical queries
rebuild from history; later queries may reuse a view when no event boundary lies
between the two times.

Polign v0.6.4 adds `GET /v1/collections/{collection}/watermark`, under the same
authentication and namespace boundary as the collection data API. Its opaque
revision covers the in-memory collection incarnation and mutation count, the
segment manifest, and every applied tail source key. It does not scan persisted
event records. Tail bookkeeping and manifest metadata still cost work to read.
Revisions are equality tokens, not sortable offsets or cross-replica commit IDs.
Any collection mutation can invalidate all cached pairs, including changes in
another namespace; the endpoint exposes no underlying record IDs or log keys.

The cache preserves the serving node's visibility contract. It does not add
linearizable replication, cross-writer serialization, or a database-wide snapshot.
Broad queries still discover candidates, and writes still validate complete
history. The durable log remains authoritative; restart rebuilds the process-local
view. Older Polign servers and backends without watermarks use uncached reads.

Regenerating a materialization can fail when history exceeds the existing
10,000-event per-pair limit. Caching does not remove that correctness bound.
