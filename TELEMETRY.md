# Usage statistics

Release builds of `recall` send a small anonymous heartbeat so the project can
see how many installs are running, on which platforms and versions, and which
parts of Recall get used. It is on by default and one command turns it off.

Nothing about what is remembered ever leaves the machine. No memory text, no
subjects, predicates or values, no collection names, no database addresses, no
keys, no model names, no file paths, no hostnames.

## Turning it off

Any one of these is enough:

```sh
recall telemetry off        # for this Recall home, until `recall telemetry on`
export RECALL_TELEMETRY=0   # for this environment
export DO_NOT_TRACK=1       # the cross-tool convention
```

It is also off, with nothing to set, when `CI` is set, when
`POLIGN_CONFIDENTIAL` is set, in a build made from source without a release
version, and when Recall has no home directory to keep an install ID in.
While it is off nothing is sent and no install ID is created.

`recall telemetry` says whether it is on, why not when it is off, and prints
the exact JSON a heartbeat holds.

## What is sent

One JSON object with exactly these fields, to
`https://telemetry.polign.com/v1/recall/ping`:

| Field | Example | What it is |
| --- | --- | --- |
| `version` | `v0.26.0` | The Recall release. |
| `os`, `arch` | `darwin`, `arm64` | The platform the binary was built for. |
| `install_id` | `a19d3099...` | 128 random bits, created on first use and kept in `telemetry-id` in the Recall home. Not derived from the machine, the user or the network. Delete the file to get a new one. |
| `process_id` | `0123456789abcdef` | 64 random bits, new for every run, so one run's counts are not confused with the next one's. |
| `mode` | `mcp` | The command: `mcp`, `serve` or `setup`. |
| `backend` | `local` | The backend's registered name: `local`, `polign`, `qdrant`, `pgvector`, `mongodb`. Never its address. |
| `embedder` | `openai` | The embedding provider, or `lexical` for word search. The model name is not sent. |
| `extractor` | `anthropic` | The extraction model's provider, or `none`. The model name is not sent. |
| `surface` | `text` | Which tools are served: `text` or `typed`. |
| `client` | `claude-code` | For `recall mcp`, the name the MCP client gave in its `initialize` request, reduced to lowercase letters, digits, `.`, `_` and `-`. Empty for `recall serve`. |
| `uptime_seconds` | `420` | How long this run has been up. |
| `remembers`, `recalls`, `forgets` | `3`, `7`, `1` | Successful calls of each tool in this run. |
| `replacements` | `1` | `remember` calls that replaced something already believed. |
| `inspections` | `2` | Successful `history`, `diff`, `explain` and `timeline` calls. |
| `state_calls` | `0` | Successful State API calls (the agent resume tools). |
| `rollbacks` | `0` | State API writes refused with `position_behind`, because the process was restored from an earlier point than its records. |
| `repeats` | `0` | State API writes answered with what was already recorded under the same message id, call id or outcome, as after a retry or a restore. |

The counts are numbers of calls. The arguments and results of a call are never
read for telemetry, except that a `remember` result is checked for whether it
lists a replacement.

## When

`recall mcp` and `recall serve` send one heartbeat a few seconds after they
start, one every hour, and one as they exit. `recall setup` sends one when it
finishes. A send that fails is dropped. Sending never delays a memory
operation, and the last one waits at most two seconds before the process
exits.

The first time an install ID is created, Recall says so once: `recall setup`
prints the notice, and `recall mcp` and `recall serve` write it to stderr.

## What the collector keeps

The collector stores the fields above and the time it received them. It does
not store the IP address, the location, the `User-Agent` or any other header.
It rejects a request that carries any field not in the table. Its source is
the `polign_telemetry` Worker.

The local database that Recall runs for itself is started with its own
telemetry off, so a Recall install reports once, through Recall.
