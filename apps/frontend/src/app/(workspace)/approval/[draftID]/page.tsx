import type { Metadata } from "next";
import { Suspense } from "react";

import { ApprovalView } from "@/components/dashboard/approval-view";

export const metadata: Metadata = {
  title: "Draf · persetujuan",
  robots: { index: false },
};

/**
 * The draft id exists only at request time, so this route is rendered on demand
 * rather than prerendered as a shell.
 *
 * The layout above it reads the workspace from client hooks, which an unbounded
 * segment cannot do at build time: there is no id to render it against. Cache
 * Components calls that a blocking route, and saying so is what keeps the read
 * at request time instead of failing the build.
 */
export const instant = false;

/**
 * One draft's approval page.
 *
 * `params` is a promise in this version of Next, and awaiting it is what makes
 * the id available: a component that read `params.draftID` directly would read
 * a promise and pass an object where a uuid is expected.
 */
export default async function ApprovalPage({ params }: { params: Promise<{ draftID: string }> }) {
  const { draftID } = await params;

  return (
    <Suspense fallback={null}>
      <ApprovalView draftID={draftID} />
    </Suspense>
  );
}
