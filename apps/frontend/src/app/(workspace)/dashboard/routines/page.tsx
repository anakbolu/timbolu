import type { Metadata } from "next";
import { Suspense } from "react";

import { RoutinesView } from "@/components/dashboard/routines-view";

export const metadata: Metadata = {
  title: "Rutinitas",
  description: "Pekerjaan yang dijalankan tim Bolu secara otomatis, tanpa perlu kamu suruh.",
  robots: { index: false },
};

/** The run list is dated against the visitor's clock, so it is read at request time. */
export default function RoutinesPage() {
  return (
    <Suspense fallback={null}>
      <RoutinesView />
    </Suspense>
  );
}
