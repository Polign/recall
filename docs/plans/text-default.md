# Text tools by default, predicates as the option

Status: decided 2026-10-13. Nothing built.

## Decision

Recall's default surface becomes the text tools (`remember(text)`,
`recall(question)`, `forget(text)`) with the open vocabulary, driven by an
extraction model. The typed tools and a predicates file stay available
behind explicit flags. Setup asks for the model; it never picks one.

Why: the 267-question user-facts run put the open vocabulary at 79.4
against 81.3 for the hand-tuned registry (p=0.36, within the 2-point
bar) and improved retrieval (nDCG@5 75.7 to 80.2). The predicates file is
the first thing a new user trips on. 97% of coined predicates landed on a
starter seed, so sprawl is handled.

What does not change: the model extracts and names facts; the engine
decides what replaces what. No model-judged corrections (see
recall-corrections-measurement).

## Where things stand (0.17.0)

| Surface | Today | After |
|---|---|---|
| `recall setup` (Claude Code plugin) | typed tools, open vocabulary; text tools only when `-extract-model` is saved | asks for a model; with one, text tools; `none` keeps typed open tools (Claude is the extractor there) |
| `recall mcp` | typed, closed starter registry; `-text`, `-open` opt in | text + open; `-typed` opts in; `-predicates FILE` closes the registry |
| `recall serve` | same as `recall mcp` | same as `recall mcp` |
| Python `Client` | `text=False`, calls typed tools | `text=None` follows the server (text); `typed=True` passes `-typed` |
| Helm chart | serve defaults | `extractModel` value, text default |
| Plugin skill | both surfaces, typed described first | text first, typed as the option |

Known blocker: the Python client spawns `recall mcp` and calls the typed
tools without passing a surface flag. Flipping the binary first breaks it.

## Phases

One release: recall `v0.18.0` and polign-recall 0.18.0 on the same tag.
Nobody outside us runs the Python client against an older binary, so the
client guards the mismatch instead of a two-step rollout.

### 1. Flags in the binary (recall 0.18.0)

- `recall mcp`, `recall serve`: add `-typed` (serve the typed tools).
  Passing `-typed` and `-text` together is an error.
- `recall setup`: add `-typed` (save `Typed: true` in config.json; the
  launcher serves typed tools even when a model is saved).
- `RECALL_TYPED=1` env mirrors the flag; `RECALL_OPEN` stays.
- `describeSurface` and `recall doctor` report the saved surface.
- The MCP `initialize` reply carries the server version (check it does).

### 2. Python client passes the surface explicitly (polign-recall 0.18.0, same tag)

- `Client(text: bool | None = None, typed: bool = False)`. `text=None`
  passes nothing (follows the binary); `text=True` passes `-text`;
  `typed=True` passes `-typed`. `extract_model` implies text unless
  `typed=True`.
- Deprecation: `text=False` explicit means typed, with a warning that says
  to use `typed=True`.
- Always passes `-text` or `-typed`, never neither.
- Reads the server version from the `initialize` reply; older than 0.18.0
  raises "recall 0.18.0 or newer is required" before any tool call.
- Typed-tool methods (`remember(subject, predicate, value)`,
  `list_predicates`, `memory_history`) raise a clear error when the server
  answered the handshake with text tools, naming `typed=True`. This is
  what an older polign-recall sees against the new binary.
- Fake-server tests for each combination (27 tests exist; extend).
- README: text example first, typed under "Fixed vocabulary".

### 3. Flip the default (same release)

- `recall mcp`, `recall serve`: text tools and open vocabulary by default.
  `-text` becomes a no-op kept for compatibility. `-predicates FILE`
  keeps the registry closed without changing the surface (model files
  into the fixed registry; the LongMemEval harness keeps working).
- No model: `textRemember` already keeps the text as a `note` with a
  message. Keep it; make the server's instructions and the tool
  description say "without an extraction model, text is kept as written
  and replaces nothing; pass -extract-model". Never refuse, never block.
- `recall setup`:
  - interactive (stdin is a TTY, no `-extract-model`): ask one question,
    "Extraction model?" with the options found: `anthropic:<default>`
    (if ANTHROPIC_API_KEY is set), `openai:<default>` (if OPENAI_API_KEY),
    each local Ollama model, `none`. Default highlighted is `none`; a
    choice is written to config.json. Print the cost line for hosted and
    the measured per-write latency line for local.
  - non-interactive or `none`: typed open tools (today's behavior) and
    the existing `modelHint()`.
  - `-typed` with a model: text extraction stays off, typed tools served.
- Plugin launcher: unchanged (reads config.json).
- Helm chart (charts/polign): `recall.extractModel` and
  `recall.typed` values; document that no model means notes only.
- Changelog 0.18.0: breaking default, Python and binary must match,
  migration lines (`-typed` for the old surface; `-predicates` for the
  old registry).

### 4. Abstention (build, then measure)

The 267 run moved abstention from 93.3 to 80.0 on 15 questions
(2 of 15 answered when they should have declined). Decided: build and
rerun.

1. In `recall(question)`, when every supporting belief sits on a coined
   predicate (not a seed) and the match score is below a threshold, answer
   with the evidence and "nothing stored says this directly" rather than a
   value. Tune the threshold offline against the 15 cached abstention
   rows first; no new extraction.
2. Rerun the 267 user-facts subset with the change (gpt-4o-mini
   extraction cached, gpt-4o read/judge). Estimated ~$11; confirm the
   figure from the cache state on the day and get a go before running.
   Pass bar: abstention back above 90 with accuracy within 2 points of
   79.4. Report both numbers in the 0.18.0 notes either way.

### 5. Docs, skill, site (with 0.18.0)

- recall: README, docs/reference.md, docs/mcp.md, docs/memory-api-v1.md,
  cmd/recall/guides/recall.md. Text tools first; "Fixed vocabulary"
  section covers `-typed` and `-predicates`.
- Polign/polign plugins/recall: SKILL.md leads with the text tools; README
  setup section shows the model question. Branch, then merge to main
  before the site.
- polign_web: recall.html (tiles, feature list), claude-code.html (setup
  transcript with the model question, tools list). Keep the line "the
  model extracts and names; the engine decides what replaces what".
  No hero taglines. Merge after the plugin, main autodeploys.
- Check polign_db's `polign mcp` (memory part superseded 2026-10-08): if
  it still serves memory tools, point it at `recall mcp` in its help text
  rather than flipping it too.

### 6. Release order

1. recall `v0.18.0` + Python 0.18.0 on one tag; CHANGELOG heading,
   `pyproject` and `__init__` bumped before the tag (release checklist).
2. Plugin skill merge, then site branch.
3. Homebrew cask follows the tag; chart `polign-chart-v0.3.0` after.

## Decisions (Anup, 2026-10-13)

1. Setup with `none`: the Claude Code plugin keeps typed open tools
   (Claude extracts, replacement still works). Text tools come with a
   model.
2. Abstention: build the weak-evidence answer and rerun the 267 subset
   (~$11, go confirmed at run time).
3. Setup asks the model question on a TTY; non-interactive runs get
   `none` plus the hint.

## Out of scope

- Auto-picking a model in setup (cost, privacy, 24 to 82 s per write
  measured with local qwen3:8b).
- Renaming `POLIGN_EXTRACT_MODEL` to `RECALL_*` (separate plan, with
  the other env aliases).
- Any change to how beliefs are retired.
