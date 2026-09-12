# Memory across sessions

Run these commands from the Recall repository against a running Polign server.
The example uses constant three-dimensional vectors for exact memory operations,
so no embedding service is needed. Use your application's embedder for semantic
search. Choose a new collection or subject when you want a fresh demonstration.

```sh
# Session 1: remember vim, then correct the preference to neovim.
go run ./examples/sessions -server http://localhost:8080 -action seed

# Session 2: a new process reads the same durable history.
go run ./examples/sessions -server http://localhost:8080 -action inspect

# Append a blanket retraction, then inspect from another process.
go run ./examples/sessions -server http://localhost:8080 -action forget
go run ./examples/sessions -server http://localhost:8080 -action inspect
```

On a fresh collection, the first two commands show current `neovim` and `vim`
at the first event. After forgetting, current beliefs are empty, the historical
answer remains `vim`, and the log still contains both assertions and a retraction.
`inspect` constructs a client without an embedder.

Set `POLIGN_API_KEY` for an authenticated server. A namespaced key selects its
namespace through the server's normal authentication; the adapter preserves
that credential on every request. `-collection` and `-subject` select the data.

For a server restart demonstration, run Polign with durable storage, for example
`go run ./cmd/server -store fs:/tmp/recall-server` from the polign_db repository.
After `seed`, stop and restart that server using the same store, then run
`inspect`. Cold persistence and restart require a server containing
[polign_db #99](https://github.com/Polign/polign_db/pull/99); v0.6.3 predates that
support. This example uses the new Client API from this source checkout, pending
its feature release.
