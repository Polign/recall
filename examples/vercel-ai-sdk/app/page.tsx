'use client';

import { useChat } from '@ai-sdk/react';
import { DefaultChatTransport } from 'ai';
import { useState } from 'react';

export default function Page() {
  const [input, setInput] = useState('');
  const { messages, sendMessage, status, error } = useChat({
    transport: new DefaultChatTransport({ api: '/api/chat' }),
  });
  const busy = status === 'submitted' || status === 'streaming';

  return (
    <main>
      <h1>Recall memory chat</h1>
      <p className="lede">
        Tell it something, correct yourself, then open a new tab and ask what it knows.
        Memory lives in your own Recall, not in this page.
      </p>

      {messages.map((m) => (
        <div key={m.id} className={`msg ${m.role}`}>
          <div className="role">{m.role === 'user' ? 'you' : 'assistant'}</div>
          <div className="bubble">
            {m.parts.map((part, i) => {
              if (part.type === 'text') return <span key={i}>{part.text}</span>;
              if (part.type === 'dynamic-tool') {
                const label =
                  part.state === 'output-available'
                    ? `${part.toolName}: ${summarize(part.output)}`
                    : part.state === 'output-error'
                      ? `${part.toolName} failed: ${part.errorText}`
                      : `${part.toolName}…`;
                return (
                  <div key={i} className="tool" title={JSON.stringify(part.input)}>
                    {label}
                  </div>
                );
              }
              return null;
            })}
          </div>
        </div>
      ))}

      {error && <div className="tool">error: {error.message}</div>}

      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!input.trim() || busy) return;
          sendMessage({ text: input });
          setInput('');
        }}
      >
        <div>
          <input
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="I switched from vim to neovim last week"
            autoFocus
          />
          <button type="submit" disabled={busy}>
            Send
          </button>
        </div>
      </form>
    </main>
  );
}

/** First line of an MCP text result, for the tool trace under a message. */
function summarize(output: unknown): string {
  const content = (output as { content?: { type: string; text?: string }[] })?.content;
  const text = content?.find((c) => c.type === 'text')?.text ?? JSON.stringify(output);
  const line = text.split('\n').find((l) => l.trim()) ?? '';
  return line.length > 160 ? `${line.slice(0, 157)}...` : line;
}
