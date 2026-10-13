import type { Metadata } from "next";
import { Suspense } from "react";

import { TasksView } from "@/components/dashboard/tasks-view";

export const metadata: Metadata = {
  title: "Tugas",
  description:
    "Riwayat tugas workspace ini: keadaan tiap tugas, alasan yang berhenti, langkah-langkahnya, dan rantai serah-terimanya.",
  robots: { index: false },
};

/**
 * The list is read from the API at request time, and the Suspense boundary keeps the
 * route static while that happens — the same shape the usage screen uses.
 */
export default function TasksPage() {
  return (
    <Suspense fallback={null}>
      <TasksView />
    </Suspense>
  );
}
