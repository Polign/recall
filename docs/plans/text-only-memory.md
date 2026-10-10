# Text-only memory: predicates become internal

Status: draft, 2026-10-09. Nothing built yet.

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
   updates. A wrong guess is fixed by one definition event, because answers
   are folded at read time and nothing is deleted.
3. **The typed API stays.** The Go and Python typed calls keep working. A
   predicates file becomes optional and turns on `-strict` (closed
   vocabulary, today's behavior). The MCP typed tools move behind `-typed`
   for one release, then that flag stays as the strict surface.
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

### 5. `recall explain`

Ships in the same release, not later. Given a question or an event id, it
shows the episode text, the statements filed from it, the predicate each one
landed on, which names were merged into that predicate, and the definition
events (with cardinality changes) that decided the answer. CLI and an MCP
tool behind `-typed`.

### 6. Setup and defaults

- `recall setup` finds `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`, or a running
  Ollama, and saves `-extract-model`. With none, it says writes will be kept
  as text only.
- Check whether Claude Code supports MCP sampling. If it does, the host's
  model can do extraction and setup needs no key.

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
