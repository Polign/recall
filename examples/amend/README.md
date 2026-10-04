# Amend: correct once, prove what it knew

![The Amend demo: seed, correct, withdraw, timetravel, and audit beats](amend.gif)

A support team runs a chat agent and a voice agent that share one memory. A rep
promises customer 4812 a refund exception on September 12, and the customer
calls on September 20 to use it. Today a support lead revokes the exception.
The customer disputes the refund they asked for on the 20th.

The demo needs no model. Each transcript turn and the statements drawn from it
are fixed, so every run gives the same answers. It uses the `support` starter
registry plus two predicates: `refund_exception` (approved or revoked) and
`policy`.

Start a Polign server, then run every beat in order:

```sh
polign-server -http :8080 -store fs:/tmp/amend-store
go run ./examples/amend -server http://localhost:8080 -collection amend-$(date +%s) -pause
```

`-pause` waits for Enter between beats, for presenting live. Use a new
`-collection` for each fresh run. Running the seed beat again is harmless: its
turns are dated in the past, so they never undo a later correction.

## The beats

| Beat | What happens | What to say |
| --- | --- | --- |
| `seed` | Replays the history: a policy memo (Aug 1), the chat promise (Sep 12), the voice call asking to use it (Sep 20). The voice agent reads the exception the chat agent recorded. | Both agents learn from one memory. |
| `correct` | The chat agent records the revocation, timestamped now. The voice agent is asked and answers from the new belief. The history shows both events. | "The old fact wasn't deleted. It was retired, with a timestamp." Correct once, fixed everywhere. |
| `withdraw` | The goodwill-credit policy is withdrawn. The voice agent can no longer cite it; the history keeps it and the withdrawal. | Gone from beliefs, kept in history. |
| `timetravel` | Reads the exception as of Sep 20: approved, with the quote and the whole chat turn it came from. Today's answer is revoked. | The request was made while the exception stood, so the company honors it. This is the screenshot. |
| `audit` | Exports what was believed about customer 4812 on Sep 20 as a JSON bundle, then replays it from the file alone and verifies the digest. | Anyone holding the file can check it, without our server. |

Run one beat with `-beat correct` and so on; each beat is a separate process,
so the later beats also show the memory surviving across processes. Never cut
`correct` or `timetravel`.

Check the bundle independently:

```sh
go run ./cmd/recall-audit < amend-audit.json
```

## Recording

`record.sh` plays every beat with narration and nobody typing, against its own
server on port 24400 with a fresh store. `amend.cast` and `amend.gif` were made
with it:

```sh
asciinema rec --headless --window-size 124x34 -c ./examples/amend/record.sh amend.cast
agg --idle-time-limit 6 amend.cast amend.gif
```

## What it does not show

The demo proves what the agents believed on any date and which turn each belief
came from. It does not link an agent's answer to the beliefs that produced it;
that log is not built yet.

Set `POLIGN_API_KEY` for an authenticated server.
