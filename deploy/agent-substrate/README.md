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
repository, deploys them, and checks five things:

1. An actor remembers a fact and recalls it, holding no key.
2. A second actor asks the same question and gets nothing.
3. A call that reaches Recall without an actor token is refused with 401.
4. After a suspend, the actor's in-process counter continues from its snapshot,
   and Recall answers on the first call after the resume.
5. After the actor is deleted and created again, the counter starts over and
   the memory is still there.

`./run-kind.sh clean` removes what the script created.

## What to know

- **Use the full Service name.** An actor's DNS only answers names its egress
  policy lists, and it does not apply the cluster's search domains. The policy
  and `RECALL_URL` both name `recall.recall-system.svc.cluster.local`.
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
- **The State API works as it does with an ordinary key.** The token is not
  bound to a session, so writes carry the lease epoch.

Tested on kind with Substrate v1.0.0 and gVisor workers.
