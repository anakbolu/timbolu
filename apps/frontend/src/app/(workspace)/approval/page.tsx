import type { Metadata } from "next";
import { Suspense } from "react";

import { ApprovalInbox } from "@/components/dashboard/approval-inbox";

export const metadata: Metadata = {
  title: "Persetujuan",
  description: "Draf yang menunggu keputusanmu, terbaru dulu.",
  robots: { index: false },
};

/**
 * The inbox reads the workspace from client hooks, so it is rendered inside a
 * Suspense boundary rather than prerendered: the list belongs to whoever is
 * signed in, and a shell built at build time would have no workspace to ask.
 */
export default function ApprovalInboxPage() {
  return (
    <Suspense fallback={null}>
      <ApprovalInbox />
    </Suspense>
  );
}
