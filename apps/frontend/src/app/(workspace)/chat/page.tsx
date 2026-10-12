import type { Metadata } from "next";
import { Suspense } from "react";

import { ChatScreen } from "@/components/chat/chat-view";

export const metadata: Metadata = {
  title: "Obrolan",
  description: "Ngobrol dengan Bolu-mu: balasan streaming, grafik, diagram, dan draf.",
  robots: { index: false },
};

/**
 * Which Bolu is open travels in the query, so a link from the sidebar or the office
 * opens that thread. Reading a query string is a client-side read, which is what the
 * Suspense boundary keeps out of the prerender — the same shape the auth pages use.
 */
export default function ChatPage() {
  return (
    <main className="flex h-dvh min-h-0 flex-1 flex-col overflow-hidden">
      <Suspense fallback={null}>
        <ChatScreen />
      </Suspense>
    </main>
  );
}
