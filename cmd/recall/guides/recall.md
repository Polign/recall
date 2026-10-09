Use Recall for durable user preferences and project facts with corrections,
withdrawals, and history. The `recall` command serves it over MCP and keeps the
memory in a Polign database (`polign-server`).

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
the saved connection. Omitting `-no-plugin` from setup also installs the Claude
Code plugin. These setup actions are separate from reading this guide.

## Remember, correct, and withdraw

Call `list_predicates` first. It returns the allowed predicates, value types, and
cardinality. Do not invent predicates. A single-valued predicate replaces its
current value; a multi-valued one accumulates values until explicitly withdrawn.
Use `user` for personal preferences and stable project names for project facts.

When a statement is worth keeping and no predicate fits, do not drop it. Use
predicate `note` with the statement in the user's words as the value. Notes
accumulate, never replace each other, and come back after typed memories.

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

Call `forget` to withdraw the current value:

```json
{"subject":"user","predicate":"prefers_response_style","value":"concise"}
```

In the MCP tool, omitting `value` withdraws every value for that pair. False and
zero are real typed values. Forgetting appends a retraction and preserves history;
it does not erase data or revive a superseded value.

## Extracting text and interpreting results

For free text, the calling agent proposes facts. Call `remember` with `text` and
a `statements` array; each proposal contains `subject`, `predicate`, typed `value`,
and `evidence` quoting the input exactly. Use registered predicates and omit
ambiguous claims. If no predicate fits a statement, propose the one you would
want: the whole text is kept as a note and that proposal is listed under
`unfiled`. Text with no proposals is also kept as a note, about `user`.
Extracted proposals are marked `agent_inferred`; validation does not establish
truth, and the evidence quote is not stored in event metadata.

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
