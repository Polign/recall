import { createMCPClient } from '@ai-sdk/mcp';

/**
 * Connects to `recall serve` as an MCP client over streamable HTTP.
 *
 * The key names the namespace: every tool call this client makes reads and
 * writes that namespace's memory and nothing else. For memory per user, mint
 * one key per user (`recall key create -namespace <user>`) and look it up
 * here instead of reading one key from the environment.
 */
export async function connectRecall(key = process.env.RECALL_API_KEY) {
  const base = process.env.RECALL_URL;
  if (!base) throw new Error('RECALL_URL is not set; it is where recall serve listens');
  if (!key) throw new Error('RECALL_API_KEY is not set; create one with `recall key create`');
  return createMCPClient({
    transport: {
      type: 'http',
      url: `${base.replace(/\/+$/, '')}/mcp`,
      headers: { Authorization: `Bearer ${key}` },
    },
  });
}
