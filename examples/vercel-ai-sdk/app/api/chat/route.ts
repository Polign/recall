import { convertToModelMessages, stepCountIs, streamText, type UIMessage } from 'ai';
import { connectRecall } from '@/lib/recall';

export const maxDuration = 60;

const memoryTools = ['remember', 'recall', 'forget', 'history', 'diff'];

// A public deployment pays for every turn. These caps are per warm function
// instance, so they bound a burst rather than a determined visitor; a shared
// limiter (Vercel KV, Upstash) is the step up if the demo draws real traffic.
const WINDOW_MS = 10 * 60_000;
const MAX_REQUESTS_PER_WINDOW = 20;
const MAX_MESSAGES = 40;
const MAX_MESSAGE_CHARS = 4_000;
const windows = new Map<string, { count: number; resetAt: number }>();

function overLimit(ip: string) {
  const now = Date.now();
  const w = windows.get(ip);
  if (!w || w.resetAt <= now) {
    windows.set(ip, { count: 1, resetAt: now + WINDOW_MS });
    return false;
  }
  w.count += 1;
  return w.count > MAX_REQUESTS_PER_WINDOW;
}

function textLength(message: UIMessage | undefined) {
  return (message?.parts ?? []).reduce(
    (n, part) => n + (part.type === 'text' ? part.text.length : 0),
    0,
  );
}

const system = `You are an assistant with long-term memory kept in Recall.

Before you answer anything about the user, their preferences, or their past
decisions, call recall with the question. Only current beliefs come back; when
a belief lists something under "before", say what changed and when.

When the user states a fact, a preference, a decision, or a correction, call
remember with that sentence as written. When they say to forget something,
call forget. Do not announce these calls; mention them in a short clause only
when they change your answer.`;

export async function POST(req: Request) {
  const { messages }: { messages: UIMessage[] } = await req.json();

  const ip = req.headers.get('x-forwarded-for')?.split(',')[0]?.trim() || 'unknown';
  if (overLimit(ip)) {
    return new Response('Too many requests; try again in a few minutes.', { status: 429 });
  }
  if (messages.length > MAX_MESSAGES) {
    return new Response('This conversation is long enough; start a new one.', { status: 413 });
  }
  if (textLength(messages.at(-1)) > MAX_MESSAGE_CHARS) {
    return new Response('Message too long.', { status: 413 });
  }

  // One MCP client per request: Vercel functions do not keep connections
  // between invocations, and the connect costs one initialize round trip.
  const recall = await connectRecall();
  let closed = false;
  const close = async () => {
    if (closed) return;
    closed = true;
    await recall.close();
  };

  try {
    // The server also serves the agent-resume tools, which this chat has no use
    // for; hand Claude the five memory tools only.
    const served = await recall.tools();
    const tools = Object.fromEntries(
      memoryTools.filter((name) => name in served).map((name) => [name, served[name]]),
    );
    const result = streamText({
      // A string model id goes through the Vercel AI Gateway: on Vercel the
      // deployment authenticates as itself, locally AI_GATEWAY_API_KEY does.
      // No provider key is needed anywhere.
      model: 'anthropic/claude-opus-5.5',
      system,
      tools,
      stopWhen: stepCountIs(8),
      messages: await convertToModelMessages(messages),
      onFinish: close,
      onAbort: close,
      onError: async ({ error }) => {
        console.error('chat:', error);
        await close();
      },
    });
    return result.toUIMessageStreamResponse();
  } catch (error) {
    await close();
    throw error;
  }
}
