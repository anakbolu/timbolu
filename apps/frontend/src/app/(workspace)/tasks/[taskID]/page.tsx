import type { Metadata } from "next";
import { Suspense } from "react";

import { TaskDetailView } from "@/components/dashboard/task-detail-view";

export const metadata: Metadata = {
  title: "Tugas · detail",
  robots: { index: false },
};

/**
 * The task id exists only at request time, so this route is rendered on demand rather
 * than prerendered as a shell.
 *
 * It is the workspace's first dynamic route, and the layout above it reads the
 * workspace from client hooks — a read an unbounded segment cannot do at build time,
 * because there is no id to render it against. Cache Components (this repo builds with
 * `cacheComponents`) calls that a blocking route, and saying so is what keeps the read
 * at request time instead of failing the build.
 */
export const instant = false;

/**
 * One task's page.
 *
 * `params` is a promise in this version of Next, and awaiting it is what makes the
 * id available: a component that read `params.taskID` directly would read a promise
 * and pass an object where a uuid is expected.
 */
export default async function TaskPage({ params }: { params: Promise<{ taskID: string }> }) {
  const { taskID } = await params;

  return (
    <Suspense fallback={null}>
      <TaskDetailView taskID={taskID} />
    </Suspense>
  );
}
