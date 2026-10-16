# Predicate name matching

Measures how well an embedder separates predicate names that mean the same
relation from names that do not. An open Recall client makes this comparison
before it defines a new predicate (`Config.MatchThreshold`).

```sh
go run ./eval/predicate-matching -model nomic-embed-text   # local Ollama
go run ./eval/predicate-matching -lexical                  # built-in word search
go run ./eval/predicate-matching -local                    # built-in model, all-MiniLM-L6-v2
```

`pairs.json` holds 25 same-relation pairs and 25 different-relation pairs.
Results on 2026-10-09, names only (no descriptions):

| Threshold | nomic-embed-text, same merged | nomic-embed-text, wrong merges | lexical, same merged | lexical, wrong merges |
| --- | --- | --- | --- | --- |
| 0.80 | 14/25 (56%) | 1/25 (4%) | 1/25 (4%) | 0/25 |
| 0.85 | 10/25 (40%) | 1/25 (4%) | 0/25 | 0/25 |
| 0.90 | 6/25 (24%) | 0/25 | 0/25 | 0/25 |
| 0.95 | 3/25 (12%) | 0/25 | 0/25 | 0/25 |

The wrong merge below 0.9 is `works_at` and `worked_at` (0.881), which would
let a past job replace the current one. The default stays 0.9. Name matching
is a safety net: with a model embedder it catches about a quarter of
synonyms, and with the lexical embedder only reordered words. Reuse mostly
comes from the extraction model, which is shown the predicates in use.

Rerun on 2026-10-16 with the model built into recall (`-local`,
all-MiniLM-L6-v2), names only:

| Threshold | same merged | wrong merges |
| --- | --- | --- |
| 0.80 | 12/25 (48%) | 1/25 (4%) |
| 0.85 | 9/25 (36%) | 1/25 (4%) |
| 0.90 | 5/25 (20%) | 0/25 |
| 0.95 | 1/25 (4%) | 0/25 |

The highest different-relation score is 0.879, under 0.9, so the default
holds for the built-in model too.

