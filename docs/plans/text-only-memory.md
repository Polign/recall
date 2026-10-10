# Text-only memory: predicates become internal

Status: phases 1 to 6 built on branch `at/text-only-memory` (2026-10-09).

## Goal

Users and agents pass text. The engine decides what is a fact, what it is
about, and whether it replaces something. No predicates file, no predicate
names in any tool, prompt, or answer.

```
remember("I switched from helix to zed last week")
recall("what editor do I use?")
  -> "You use zed (since Oct 2). Before that: helix."
forget("my editor")
```

Predicates still exist inside the engine, because supersession needs a key.
They are created, matched and corrected by the engine and recorded in the
log, like any other event.

## What already exists

| Piece | Where | State |
| --- | --- | --- |
| Free-text writes with a model | `Client.RememberText`, `internal/engine/extract.go` | works, but only files registered predicates; the rest become notes |
| Extractors | `model/` (anthropic, openai, ollama), `-extract-model` | works; prompt and JSON schema restrict predicates to the registry |
| Registry as log events | `internal/engine/schema.go` (`registry_change`) | works; `SyncRegistry` records changes |
| Aliases and renames | `Predicate.Aliases`, `schema.canonical` | works |
| Read-time fold | `Fold`, `Store` | works; a definition change re-derives every answer |
| Episode + evidence links | `RememberText` | works; every statement points at its episode |
| Time phrases in queries | `internal/engine/when.go` | works |
| `recall explain` | none | not built |

## Decisions

1. **`remember` needs a model.** Without one, text is kept as an episode and
   is searchable, but nothing is filed or replaced, and the tool result says
   so. `recall setup` picks a model (API key found in the environment, or a
   local Ollama model).
2. **Unknown predicates default to single-valued.** Stated facts are mostly
   updates. A wrong guess is fixed by one retroactive definition
   (`Client.Redefine`), because answers are folded at read time and nothing
   is deleted. An ordinary definition change only applies from when it is
   recorded, so the retroactive kind was added for this (built in phase 1).
3. **The typed API stays.** The library gets `Config.Open`, off by default,
   so existing typed callers keep a closed registry. The `recall` binary
   turns it on in phase 4, when its surface becomes text-only; a predicates
   file there turns on `-strict` (closed vocabulary, today's behavior). The
   MCP typed tools move behind `-typed` for one release, then that flag
   stays as the strict surface.
4. **Never merge a single-valued predicate with a multi-valued one**, however
   close the names are.
5. **Keep the raw relation** the model wrote on each event (new metadata field
   `relation`), so a merge is a view and can be undone.
6. **Pitch change.** "Corrections follow a rule, not a model's judgment"
   becomes "corrections follow a recorded rule; how a statement is filed is
   logged and correctable". recall.html and claude-code.html change with the
   release.

## Phases

### 1. Open registry (engine)

- A write with an unknown predicate defines it instead of failing
  (`store.go:193`). The definition is a `registry_change` event: normalized
  name, cardinality (from the writer, else single), value type from the first
  value (JSON number, bool, else string), description if given.
- Before defining, re-read the registry log, skipping the 30 s cache
  (`registryTTL`), so two clients rarely define the same name twice. If they
  do, the earliest recorded definition wins (already how the log folds).
- Name normalization: lowercase, snake_case, trim, collapse repeats.
- `-strict` (or a configured registry with `Strict: true`) keeps today's
  refusal.
- `RememberText` stops sending unregistered proposals to `Unfiled`; they are
  defined and filed.

Done when: engine tests cover define-on-write, concurrent definitions,
strict mode, and a cardinality flip that re-derives past answers.

Built: `Config.Open`, define-on-write as `auto` registry events (first
definition wins), name normalization, type inference,
`RememberRequest.Cardinality`/`Description`, `Client.Vocabulary`,
retroactive `Client.Redefine`, `RememberText` filing coined predicates and
offering the learned vocabulary, and audit fold `recall-fold-v3` (written
only when a bundle needs it). Not yet: re-reading the registry log on a miss
happens only inside `define`, so a closed client can take up to 30 s to see
a new predicate.

### 2. Matching new names to existing ones

- When a name is new, embed `name + description` and search the registry
  events in the same collection (filter `subject = recall:registry`). This
  needs no extra index and nothing to rebuild on restart.
- Reuse the closest predicate when it is above a threshold and has the same
  cardinality and type; record the new name as an alias event. Otherwise
  define a new predicate.
- The threshold is per collection, with a default tuned offline.
- Check what registry events embed today; they may need `name +
  description` text instead of the JSON definition.
- Measure with the lexical embedder and a model embedder. If lexical cannot
  separate synonyms from distinct relations, matching needs a model embedder
  or falls back to asking the extraction model to pick from the vocabulary
  (phase 3 already sends it).

Done when: a labeled synonym set (built from the cached LongMemEval
extraction outputs, no new model calls) shows the chosen threshold's
over-merge and under-merge rates, and the over-merge rate is under 1%.

Built: matching in the process with the client's embedder and a cache,
instead of a vector search over registry events (those embed JSON-heavy
text, and search scores differ by backend). A match records a separate
merge record (`alias_of`), so concurrent merges cannot overwrite each other;
events written through a merged name keep it, which is also how decision 5
(keep the raw relation) is met without an event format change.
`Client.Merge` and `Client.Split` merge and undo by hand.
`Config.MatchThreshold` defaults to 0.9.

Measured (`eval/predicate-matching`, 25 same and 25 different pairs, built
by hand instead of from LongMemEval outputs): at 0.9, nomic-embed-text
merges 24% of synonyms with no wrong merges; at 0.85 it merges 40% but
also `works_at` with `worked_at`. The lexical embedder merges nothing but
reordered words. So name matching is a safety net and the extraction
model's reuse of the shown vocabulary does most of the work. Open: measure
how often the model reuses names, from phase 3 output.

### 3. Extraction coins predicates

- `model/` prompt: show the current vocabulary with descriptions; allow a new
  predicate with a one-line description and `single` or `multi`.
- JSON schema: predicate becomes a free string, plus optional `cardinality`
  and `description` on new ones (today it is an enum of registered names).
- Retry safety: the episode is the unit of retry. Derive the episode key from
  the text hash plus an optional caller key (agent id and turn). If
  statements with that `EvidenceID` exist, skip extraction and return them,
  so a crash and retry cannot file the same text twice in different ways.
- `forget(text)`: the model maps the text to a subject and predicate (and a
  value when one is named) using the vocabulary and a search over current
  beliefs. It withdraws and reports what it withdrew. A wrong match is
  undone from history.

Done when: replaying a crashed write files nothing new, and forget on a
paraphrase withdraws the right belief in the test suite.

Built: open prompt and schema in `model/` (with `value_type` so coined
numbers stay numbers), `recall.OpenVocabulary(ctx)`, `Client.ForgetText`
with a `Selector` (`model.Extractor` implements it). Retry safety changed
from the caller key above: the engine records each model extraction beside
its episode (reserved subject `recall:extraction`) and replays it when the
same text comes in again. That needs no caller key, files a retry
identically, and still lets restated text become current again, because
replay goes through the fold. Not yet: a live-model test that forget on a
paraphrase picks the right belief (the suite uses a stub selector).

### 4. Text-only surface

- MCP tools: `remember(text, observed_at?)`, `recall(question, as_of?)`,
  `forget(text)`. `list_predicates` and `memory_history` move behind
  `-typed`; `recall` answers already carry replaced values and dates.
- Instructions: no predicate list. Short guidance on what is worth
  remembering.
- Answers render as sentences (`{subject} {predicate as text} {value}`, with
  since-when and the replaced value). The predicate-to-text template is part
  of the definition, generated by the model when it coins the predicate.
- Go: `Client.Ask(ctx, question)` returning `[]Memory{Text, Since,
  Replaced, EventID}`, `Client.ForgetText`. `RememberText` stays.
- Python client mirrors the Go calls.

Built (phase 4): `Client.Ask`/`AskAt` returning `Memory` sentences,
`recall.Sentence`, `recall mcp -text` (three tools, no predicates anywhere,
tested) and `recall mcp -open` (typed tools, open vocabulary, `cardinality`
and `description` on remember). Changed from the plan: both are opt-in
flags, not the new default. The Python client starts `recall mcp` (sometimes
with `-extract-model`) and calls the typed tools, and its integration test
expects unknown predicates to become notes, so switching surfaces by
default would break it. The default flips once the Python client passes
`-typed` (or the new flags) for a release. Without a model, `-text` keeps
text as written instead of refusing. Not yet: the Python client mirror, and
model-written sentence templates (answers use the predicate's words).

Live check (local Qdrant, `ollama:qwen3:8b`): extraction reused starter
predicates, coined `lives_in`, superseded helix with zed, and replayed
restated text. It found two bugs, both fixed and tested: forget also
withdrew the source texts (one held where the user lives), and recall told
every fact twice (fact and source text). Writes took 24 to 82 s each with
that thinking model.

### 5. `recall explain`

Ships in the same release, not later. Given a question or an event id, it
shows the episode text, the statements filed from it, the predicate each one
landed on, which names were merged into that predicate, and the definition
events (with cardinality changes) that decided the answer. CLI and an MCP
tool behind `-typed`.

Built: `Client.Explain`/`ExplainBelief`, `recall explain <question>` (text
or `-json`), and an `explain` tool on the typed surface only.

### 6. Setup and defaults

- `recall setup` finds `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`, or a running
  Ollama, and saves `-extract-model`. With none, it says writes will be kept
  as text only.
- Check whether Claude Code supports MCP sampling. If it does, the host's
  model can do extraction and setup needs no key.

Built, changed from the plan: setup saves `-extract-model` when given
(`none` clears it) but never picks one by itself. A hosted model receives
and bills every remembered passage, and the live check showed a local
thinking model adding 24 to 82 s per write, so the choice stays with the
user; setup lists local Ollama models as a hint. New setups get an open
vocabulary. Sampling: Claude Code does not support MCP sampling, and the
MCP spec deprecated sampling (SEP 2577, 2026-07-28), per a docs check on
2026-10-09, so a server-side model is required for the text surface.

### 7. Eval gate (paid, needs a cost estimate and a yes first)

The current lead (89.9 vs 83.9 with gpt6 astra) uses the hand-tuned eval
registry in `eval/longmemeval/registry.json`, including prompt rules about
which predicates keep only the newest value. Open vocabulary gives that
tuning up, so this is the main accuracy risk, mostly in knowledge-update.

- Reuse cached judged outputs for the fixed-vocabulary baseline.
- Pilot: 30 knowledge-update questions, open vocabulary, same reader and
  judge. Estimate cost from the cached run's token counts before running.
- Full run only if the pilot is within 3 points.
- Ship bar: within 2 points of the fixed-vocabulary headline. Over 5 points
  means the matching threshold or the extraction prompt is wrong.

Result (2026-10-10, 77 knowledge-update questions of the user-facts
subset, gpt-4o-mini extraction, gpt-4o reader and judge; the fixed version
is the knowledge-update rows of uf-rel012-ages, which this branch retrieves
identically): fixed 66/77 (85.7), open 64/77 (83.1), a 2.6-point gap. Four
questions flipped against the open version and two in its favor (exact
McNemar p = 0.69), so the gap is within noise at this size; abstention went
from 5/6 to 4/6. Session retrieval improved (nDCG@5 82.7 to 88.6). The
losses are filing choices, not vocabulary sprawl: a record update filed as
an event (`did`) instead of a `status`, and an update the extractor did not
pick up. A first attempt without seeds coined 90 predicates for 167
statements and was stopped; with the personal starter and the firmer
rules, 98% of statements used a seed. Cost about $3.70.

Full user-facts subset (267 questions, same setup; the fixed version is
uf-rel012-ages, retrieved identically on this branch): fixed 217/267
(81.3), open 212/267 (79.4), a 1.9-point gap with 7 questions gained and
12 lost (exact McNemar p = 0.36; bootstrap 95% interval for the
difference -5.2 to +1.1 points). By type: single-session-user 95.7 to
94.3, preference 43.3 to 43.3, knowledge-update 85.7 to 83.1, temporal
78.9 to 76.7, abstention 93.3 to 80.0 (2 of 15). nDCG@5 75.7 to 80.2.
88,264 statements, 323 distinct predicates, 97% on a seed. This meets the
ship bar (within 2 points); abstention is the one place to watch. Total
phase 7 cost about $12.

### 8. Docs and site

README, `docs/mcp.md`, `docs/reference.md`, the agent guide (`recall skill`),
the Claude plugin skill, polign.com recall.html ("Not an extractor" line),
claude-code.html (five tools become three), and the LiveKit, Vapi and
LangGraph adapters, which may pass predicates today.

### 9. Release

Minor version (0.14.0). The MCP tool surface changes, so the changelog says
how to keep the old one (`-typed`).

## Order

1, then 3 (it needs open definitions), then 2 (matching is tuned on what 3
produces), then 4, 5 and 6 together, then the eval gate, then 8 and 9.
Phase 1 alone removes the predicates file for typed callers.

## Open questions

- Is the lexical embedder good enough for phase 2, or does matching need a
  model embedder or the extraction model?
- Does Claude Code support MCP sampling?
- What does the per-collection threshold look like for an operator: a flag,
  or a registry event?
