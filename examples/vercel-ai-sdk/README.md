# Recall with the Vercel AI SDK

A Next.js chat built on the [Vercel AI SDK](https://ai-sdk.dev) whose long-term
memory is your own `recall serve`. The app connects to Recall as an MCP client
over streamable HTTP, so Claude gets `remember`, `recall`, `forget`, `history`
and `diff` as tools and no Recall client library is needed on the TypeScript
side. Tell it something, correct yourself, open a new tab, and ask what it
knows: the answer comes with what the correction replaced.

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2FPolign%2Frecall%2Ftree%2Fmain%2Fexamples%2Fvercel-ai-sdk&project-name=recall-memory-chat&repository-name=recall-memory-chat&env=RECALL_URL,RECALL_API_KEY&envDescription=RECALL_URL%20and%20RECALL_API_KEY%20point%20at%20your%20recall%20serve.%20Claude%20is%20called%20through%20the%20Vercel%20AI%20Gateway%2C%20no%20provider%20key%20needed.&envLink=https%3A%2F%2Fgithub.com%2FPolign%2Frecall%2Ftree%2Fmain%2Fexamples%2Fvercel-ai-sdk%23environment)

Memory stays in the store behind your `recall serve`; nothing about the user is
kept on Vercel. The deployment needs to reach the server over HTTPS, so for a
production deployment put `recall serve` behind a TLS endpoint (the
[container](../../deploy/container), the Helm chart in
[Polign/polign](https://github.com/Polign/polign), or one of the
[marketplace images](../../deploy)).

## Run it locally

You need Node 20+, the `recall` binary and a store. The quickest store is a
`polign-server` on local disk (`pip install polign_db` installs one).

```sh
# 1. A store, on local disk.
polign-server -store fs:/tmp/recall-store -http 127.0.0.1:23000

# 2. recall serve, with one key for the "demo" namespace.
echo "rcl_dev_example_key demo" > keys.txt
recall -backend polign -url http://127.0.0.1:23000 serve -keys-file keys.txt

# 3. The app.
cp .env.example .env.local        # set AI_GATEWAY_API_KEY for local runs
npm install
npm run dev                       # http://localhost:3000
```

`npm run smoke` talks to the server the way the chat route does, with no model
in the loop: it lists the tools, remembers two statements and recalls them.

Give `recall serve` an extraction model (`-extract-model openai:gpt-4.1-mini`
or any provider it supports) and a correction replaces the earlier fact
instead of sitting beside it; without one the server keeps each statement as
written and says so in its tool result.

## Environment

| Variable | What it is |
| --- | --- |
| `RECALL_URL` | Where `recall serve` listens. The app posts to `$RECALL_URL/mcp`. |
| `RECALL_API_KEY` | A Recall key. Its namespace is the memory this deployment reads and writes. Create one with `recall key create -namespace <name>` against a server with a key store, or list it in `-keys-file`. |
| `AI_GATEWAY_API_KEY` | Local runs only. Claude is called through the [Vercel AI Gateway](https://vercel.com/docs/ai-gateway); a deployment on Vercel authenticates as itself, so the project needs no model key at all. Usage is billed to the Vercel account. |

## How it works

- [lib/recall.ts](lib/recall.ts) opens an MCP client with `createMCPClient`
  from `@ai-sdk/mcp`, transport `http`, the key as `Authorization: Bearer`.
- [app/api/chat/route.ts](app/api/chat/route.ts) passes `client.tools()` to
  `streamText`, with a system prompt that says to recall before answering
  about the user and to remember facts, preferences and corrections. One
  client is opened per request and closed when the stream ends.
- [app/page.tsx](app/page.tsx) is `useChat`; tool calls show as a trace line
  under the message so you can see what was remembered and recalled.

## Limits

The chat route caps each visitor at 20 requests per ten minutes, 40 messages
per conversation and 4,000 characters per message. The request cap is held
in memory per function instance, so it bounds a burst, not a determined
visitor; put a shared limiter in front for a deployment that draws traffic.

## Memory per user

The key names the namespace, so this example is one memory for the whole
deployment. For memory per user, mint a key per user and pass it to
`connectRecall(key)` from the request's session instead of the environment:

```sh
recall -url http://polign:23000 key create -namespace user:42 -expires 720h
```

Each namespace is its own collection, so nothing one user's agent sends can
read another's.
