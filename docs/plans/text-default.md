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

### 4. Abstention (disclose)

The 267 run moved abstention from 93.3 to 80.0 on 15 questions. Read on
2026-10-13 from the two runs' judge.jsonl and ingest logs:

- One is judge noise: 2133c1b5_abs got the same answer in both runs
  ("you said Harajuku, not Shinjuku, 3 months") and gpt-4o judged it
  right in the fixed run and wrong in the open run.
- One is a filing loss: f4f1d8a4_abs ("what did my dad give me?"). The
  fixed registry filed `detail: received a new stand mixer as a birthday
  gift from sister`; the open vocabulary filed `owns: stand mixer`, and
  the reader guessed dad. `owns` is a seed predicate, so a decline keyed
  on coined predicates would not have fired.

Decided: disclose only. The 0.18.0 notes carry the numbers and the cause.
A prompt rule ("values keep the participants") plus a full re-extraction
would cost about $12 and is not approved.

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

### 5b. Integration packages (found 2026-10-13)

recall-livekit, recall-vapi and recall-pydantic-ai build or take a
`polign_recall.Client` and use the typed calls, so the Python default flip
breaks them. Decided: flip anyway. livekit and vapi pass `typed=True`
themselves; pydantic-ai takes the client from the caller, so its README,
docstring and site page say `Client(..., typed=True)`. All three pin
`polign-recall>=0.18.0` and get patch releases (livekit 0.3.1, vapi 0.2.1,
pydantic-ai 0.1.1) after recall 0.18.0. recall-langgraph only uses the
resume tools and is unaffected. Branch at/recall-text-default in
Polign/polign also carries the recall chart 0.3.0 and the plugin README;
polign_web at/recall-text-default carries the site pages.

The agent-sandbox example (kubernetes-sigs/agent-sandbox
examples/recall-memory, PR 1913) posts typed bodies to `/v1/remember` and
`/v1/recall`. It pins recall 0.17.0, so it is unaffected until the image is
bumped; the bump must add `- -typed` to the server args.

### 6. Release order

1. recall `v0.18.0` + Python 0.18.0 on one tag; `pyproject` and
   `__init__` are already 0.18.0 on the branch, the CHANGELOG heading is
   set at tag time (release checklist).
2. Polign/polign at/recall-text-default merge (chart 0.3.0, plugin README,
   three integration packages), then tags livekit/v0.3.1, vapi/v0.2.1,
   pydanticai/v0.1.1 and the chart tag.
3. polign_web at/recall-text-default merge (main autodeploys).
4. Homebrew cask follows the recall tag.

## Decisions (Anup, 2026-10-13)

1. Setup with `none`: the Claude Code plugin keeps typed open tools
   (Claude extracts, replacement still works). Text tools come with a
   model.
2. Abstention: disclose only (revised 2026-10-13 after reading the two
   runs; see phase 4).
3. Setup asks the model question on a TTY; non-interactive runs get
   `none` plus the hint.

## Out of scope

- Auto-picking a model in setup (cost, privacy, 24 to 82 s per write
  measured with local qwen3:8b).
- Renaming `POLIGN_EXTRACT_MODEL` to `RECALL_*` (separate plan, with
  the other env aliases).
- Any change to how beliefs are retired.
