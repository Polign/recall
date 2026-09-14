// Package recall is the typed agent-memory layer: a closed registry of
// predicates, an append-only event log per subject, and the fold that turns
// that log into what the agent believes, now or at any past instant.
//
// The central decision is that a belief is derived, never mutated. An earlier
// design flipped a record's status to "superseded" in a second write after
// storing its replacement, which left two live beliefs for a single-valued
// predicate whenever a process died between the two. Here the log is the
// truth and Fold deterministically interprets the events visible to a reader.
// This does not serialize concurrent writers or establish a read snapshot.
// Current and historical beliefs use the same fold with a different ceiling.
//
// Retraction is likewise an event, not a deletion. Forgetting a fact on
// Wednesday must not change what the agent believed on Tuesday, and a log
// that erases its own history cannot answer that.
package recall
