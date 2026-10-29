// Talks to `recall serve` the way the chat route does, with no model in the
// loop: connect over MCP, list the tools, remember two statements, recall.
//
//   RECALL_URL=http://127.0.0.1:8080 RECALL_API_KEY=... npm run smoke
import { createMCPClient } from '@ai-sdk/mcp';

const base = process.env.RECALL_URL ?? 'http://127.0.0.1:8080';
const key = process.env.RECALL_API_KEY;
if (!key) {
  console.error('set RECALL_API_KEY');
  process.exit(2);
}

const client = await createMCPClient({
  transport: {
    type: 'http',
    url: `${base.replace(/\/+$/, '')}/mcp`,
    headers: { Authorization: `Bearer ${key}` },
  },
});

const text = (result) =>
  (result?.content ?? [])
    .filter((c) => c.type === 'text')
    .map((c) => c.text)
    .join('\n');

try {
  const tools = await client.tools();
  console.log('tools:', Object.keys(tools).join(', '));
  const opts = { toolCallId: 'smoke', messages: [] };
  const stamp = new Date().toISOString();
  console.log('\nremember:', text(await tools.remember.execute({ text: `As of ${stamp} the smoke test's editor is vim.` }, opts)));
  console.log('\nremember:', text(await tools.remember.execute({ text: `As of ${stamp} the smoke test's editor is neovim, not vim.` }, opts)));
  console.log('\nrecall:', text(await tools.recall.execute({ question: "what is the smoke test's editor" }, opts)));
} finally {
  await client.close();
}
