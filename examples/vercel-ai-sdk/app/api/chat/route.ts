import { anthropic } from '@ai-sdk/anthropic';
import { convertToModelMessages, stepCountIs, streamText, type UIMessage } from 'ai';
import { connectRecall } from '@/lib/recall';

export const maxDuration = 60;

const memoryTools = ['remember', 'recall', 'forget', 'history', 'diff'];

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
      model: anthropic('claude-opus-5-5'),
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
