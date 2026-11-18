# Recall on Agent Substrate

[Agent Substrate](https://github.com/agent-substrate/substrate) runs each agent
as an actor and snapshots its process memory and files when it goes idle. A
snapshot belongs to one actor and ends when the actor is deleted. This example
gives actors memory that lives outside them: it survives deleting the actor, and
no credential for it is ever inside the actor or its snapshots.

## How it works

1. `recall serve -substrate-issuer https://idp.ate-system.svc` trusts one
   issuer, the install's `ate-idp-server`.
2. Each actor's egress policy allows one destination, the Recall Service, and
   tells the egress gateway to replace the `Authorization` header with a JWT
   that Substrate mints for that actor (`egress-policy.yaml`).
3. The actor calls Recall with a placeholder in that header. The gateway
   discards the placeholder and sends the actor's token.
4. Recall checks the token's signature against the issuer's key set, its
   audience (`recall`), its expiry, and that its subject is
   `actor/<atespace>/<name>`. The actor's memory is the namespace
   `<atespace>:<name>`.

Nothing in that identity comes from the actor. The subject names the actor and
not one incarnation of it, so an actor deleted and created again under the same
name reaches the same memory. To share one memory across the actors of an
atespace, start the server with `-substrate-scope atespace`.

## Run it on kind

Install Substrate v1.0.0 on kind as in its
[quickstart](https://github.com/agent-substrate/substrate/blob/main/docs/get-started/quickstart.md),
including the counter demo, and put `kubectl-ate` on your `PATH`. Then:

```sh
./run-kind.sh
```

The script builds the `recall` image and a small demo actor from this
repository, deploys them, and checks six things:

1. An actor remembers a fact and recalls it, holding no key.
2. A second actor asks the same question and gets nothing.
3. A call that reaches Recall without an actor token is refused with 401.
4. After a suspend, the actor's in-process counter continues from its snapshot,
   and Recall answers on the first call after the resume.
5. After the actor is deleted and created again, the counter starts over and
   the memory is still there.
6. Through the State API, an actor is refused another actor's state, and an
   actor created again resumes with the working state the deleted one left.

Checks 7 to 10 test Recall state when the runtime puts a process back to an
earlier point. The demo actor runs a small harness whose progress lives in
process memory, and the script counts its side effects outside the actor:

7. An actor sends an invoice, is reverted to a snapshot taken before, and runs
   its job again.
8. An actor sends `record_turn`, `begin_action` and `end_action` twice each,
   as it would after losing the first answer.
9. An actor created from another actor's tag calls the State API with the id
   it woke up holding.
10. An actor holds an agent in process through the Go client, against a
    polign_db it reaches directly (`polign.yaml`), and is reverted inside the
    lease TTL and after it.

The harness passes its own message and call ids, sends the position of its
last answer as `If-Position`, and calls `since` after `position_behind`; the
checks pass when a reverted or retrying actor records no duplicate turn and
sends no second invoice. Where Recall does not behave as it should, they
print `GAP` and the script exits 1; see `docs/plans/snapshot-compatibility.md`.
`SKIP_BASE=1 ./run-kind.sh` runs only checks 7 to 10.

`./run-kind.sh clean` removes what the script created.

## What to know

- **Use the full Service name in the egress policy.** An actor's DNS only
  answers names its egress policy lists, and it matches the name as it is
  looked up, which for a Service is the fully qualified one. A policy that
  lists `recall.recall-system.svc` gets `no such host`. The policy and
  `RECALL_URL` both name `recall.recall-system.svc.cluster.local`.
- **Every actor needs its own egress policy.** A policy belongs to one actor.
  Without one the actor reaches nothing, Recall included.
- **Connections do not survive a suspend.** Substrate does not keep open
  connections, so the demo actor dials Recall again on every call.
- **A token is valid until it expires.** Substrate does not revoke actor
  tokens. The policy here asks for 15 minute tokens, and a deleted actor's last
  token works until then.
- **Whoever may write an atespace's egress policies chooses where its tokens
  go.** Recall accepts only tokens minted for the audience `recall`, so a token
  minted for another service is refused here.
- **The demo keeps memory in a local database inside the Recall pod**, which is
  lost when that pod is replaced. A real install names a backend with
  `RECALL_BACKEND` and `RECALL_BACKEND_URL`.
- **For the State API, the agent is the actor.** An actor may resume only the
  agent with its own name, and its writes need no lease epoch. It never has
  to manage the lease: a call for an agent the server does not hold resumes
  it, and an actor created again under a deleted actor's name gets the old
  actor's working state back from `agent_resume`. An agent with no call for
  five minutes (`-agent-idle`) is released and taken back on its next call.
- **An actor's writes carry its position.** Substrate can revert an actor to
  an earlier snapshot at any time, including on its own after a lost node,
  so the actor's memory can be behind Recall's records. Every answer carries
  `Recall-Position`; send it back as `If-Position` (or `"at"`) on
  `record_turn`, `begin_action`, `end_action`, `update_working_state` and
  `milestone`, which answer `400` without it. A `409 position_behind` means
  the actor was put back: call `since` with its position, hand the briefing
  to the model (those turns and actions happened), and carry on from the
  position it returns. Pass `message_id` on turns and `call_id` (the model's
  tool call id) on `begin_action`, and a replay of what the actor already did
  answers with what was recorded, so a finished invoice comes back `done`
  instead of being sent again.
- **A clone wakes holding its source's identity.** An actor created from
  another actor's tag, or from the template's golden snapshot, still has the
  source's agent id in memory; its token names the new actor, so it is
  refused with `403 namespace_mismatch`. Read the actor's own name from
  `/run/ate/actor-name` when you need it, rather than keeping it from before
  the snapshot, and do not resume an agent before the golden snapshot is
  taken.

Tested on kind with Substrate v1.0.0 and gVisor workers.
