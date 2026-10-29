import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Recall memory chat',
  description: 'A Vercel AI SDK chat whose memory is a self-hosted Recall.',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
