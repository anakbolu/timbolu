import type { Metadata } from "next";
import { Suspense } from "react";

import { RoutinesView } from "@/components/dashboard/routines-view";

export const metadata: Metadata = {
  title: "Rutinitas",
  description: "Pekerjaan yang dijalankan tim Bolu secara otomatis, tanpa perlu kamu suruh.",
  robots: { index: false },
};

/**
 * The routine id exists only at request time, so this route is rendered on demand
 * rather than prerendered as a shell.
 *
 * The layout above it reads the workspace from client hooks, which an unbounded
 * segment cannot do at build time: there is no id to render it against. Cache
 * Components calls that a blocking route, and saying so is what keeps the read
 * at request time instead of failing the build.
 */
export const instant = false;

/**
 * One routine's page.
 *
 * It exists so `bolu://routine/<routine_id>` has a web address to land on: the app's
 * deep link opens the routine list focused on that routine, and a universal link needs
 * a matching path to forward to. `RoutinesView` treats the segment as a highlight, not
 * as a separate read — the list is the same read `/routines` does.
 *
 * `params` is a promise in this version of Next; awaiting it is what makes the id
 * available.
 */
export default async function RoutinePage({ params }: { params: Promise<{ routineID: string }> }) {
  const { routineID } = await params;

  return (
    <Suspense fallback={null}>
      <RoutinesView focusID={routineID} />
    </Suspense>
  );
}
