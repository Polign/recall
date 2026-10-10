Use Recall for durable user preferences and project facts with corrections,
withdrawals, and history. The `recall` command serves it over MCP and keeps the
memory in a Polign database (`polign-server`), or in another backend chosen
with `-backend`.

## Connect

Use an existing Recall connection when available. To connect an MCP host directly
to a Polign database, configure it to launch:

```sh
recall mcp -write
```

Set `POLIGN_URL` to the database address, `POLIGN_API_KEY` to its key when
required, and `POLIGN_COLLECTION` to a dedicated memory collection (default
`recall_lexical_v1`). Without `POLIGN_URL`, `recall mcp` uses the connection
`recall setup` saved. Keep credentials private. Omit
`-write` for read-only tools. The process speaks MCP on stdin/stdout; use the
host's MCP integration to call tools.

To create a managed local database and connection, `recall setup
-no-plugin -local` writes local configuration and starts the database. Then
configure the host to launch `recall mcp`. `recall doctor` checks
the saved connection and says which tools it serves. Omitting `-no-plugin` from
setup also installs the Claude Code plugin. These setup actions are separate
from reading this guide.

A server can offer one of two tool surfaces:

- **Text tools** (`recall mcp -text -extract-model <provider:model>`, or a setup
  saved with `recall setup -extract-model`): `remember`, `recall`, and `forget`
  take plain text, and the server works out the facts with its model.
- **Typed tools** (the default): `remember` takes a subject, predicate, and
  value, and `list_predicates`, `memory_history`, and `explain` are offered too.

Check the tool list: if `remember` takes only `text` and there is no
`list_predicates`, use the text tools section below; otherwise use the typed
tools section.

## Text tools

Pass what is worth remembering as text, in the user's words where you can:

```json
{"text": "I switched from helix to zed last week."}
```

The server works out the statements, files them, and says what it remembered
and what each one replaced. Restating something already known changes nothing.
Pass `observed_at`, an RFC3339 time, only for something said earlier, such as an
imported conversation.

Ask in words before relying on anything about the user:

```json
{"question": "Which editor do I use?"}
```

Answers are current memories as sentences, each with `since`, `days_ago`, and
under `before` what it replaced. Mention a correction when it matters to the
answer. Pass `as_of` to ask what was believed at an earlier time.

Withdraw what a description names:

```json
{"text": "my editor"}
```

It returns what it withdrew. The texts the facts came from stay as the record,
and history keeps the withdrawn values. Without an extraction model, `remember`
keeps text as written (searchable, but it replaces nothing) and `forget` reports
that it needs a model.

## Typed tools

Call `list_predicates` first. It returns the predicates in use, with value types
and cardinality. A single-valued predicate replaces its current value; a
multi-valued one accumulates values until explicitly withdrawn. Use `user` for
personal preferences and stable project names for project facts.

What happens with a predicate that is not listed depends on the server. Read
its instructions:

- **Closed** (they say predicates are a closed set): do not invent predicates.
  When a statement is worth keeping and no predicate fits, use predicate `note`
  with the statement in the user's words as the value. Notes accumulate, never
  replace each other, and come back after typed memories.
- **Open** (`recall mcp -open`, or new setups; they say new predicates are
  defined by first use): reuse a listed predicate whenever one fits. Otherwise
  name a short relation that many facts could share, such as `allergic_to`,
  never with the value or subject in its name. On that first write, pass
  `cardinality` (`multi` when values add up, `single` when a new value makes the
  old one untrue; single is the default) and a one-line `description`.

Call `remember` with:

```json
{"subject":"user","predicate":"prefers_response_style","value":"detailed","kind":"preference","source":"user_stated"}
```

Correct it with another `remember` call:

```json
{"subject":"user","predicate":"prefers_response_style","value":"concise","kind":"preference","source":"user_stated"}
```

Call `recall` with the exact pair:

```json
{"subject":"user","predicate":"prefers_response_style"}
```

The current value is `concise`. Call `memory_history` with the same pair to see
both statements. `recall` also accepts `as_of`, an RFC3339 time, for a historical
interpretation. Search with `query` uses lexical word overlap by default; prefer
an exact pair when known. Empty search results do not establish absence.

Call `explain` with a question to see why each matching memory is held: the
text it came from, the predicate it was filed under, the definitions that
decide how it folds, and its history.

Call `forget` to withdraw the current value:

```json
{"subject":"user","predicate":"prefers_response_style","value":"concise"}
```

In the MCP tool, omitting `value` withdraws every value for that pair. False and
zero are real typed values. Forgetting appends a retraction and preserves history;
it does not erase data or revive a superseded value.

### Extracting text with typed tools

For free text, the calling agent proposes facts. Call `remember` with `text` and
a `statements` array; each proposal contains `subject`, `predicate`, typed `value`,
and `evidence` quoting the input exactly. Omit ambiguous claims. On a closed
server, use registered predicates; if none fits a statement, propose the one you
would want: the whole text is kept as a note and that proposal is listed under
`unfiled`. On an open server, a proposal may name a new predicate with
`cardinality` and `description`, and it is defined and filed. Text with no
proposals is also kept as a note, about `user`. Extracted proposals are marked
`agent_inferred`; validation does not establish truth, and the evidence quote is
not stored in event metadata.

## Interpreting results

Read relevant current memories before relying on them. Corrections are events;
do not patch or delete raw records in the memory collection. If a write fails
partway or its acknowledgement is lost, inspect returned partial results and
history before retrying. Incomplete history is an error, not evidence that no
memory exists. Reads follow backend visibility; Recall does not add transactions
across agents.

Retain memory within the user's authorized scope. A remembered preference,
source label, or confidence score does not authorize an action. Changing memory
does not automatically revoke queued work or update dependent actions; the
execution layer must enforce the task's current authorization.
