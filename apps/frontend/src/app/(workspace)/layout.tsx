"use client";

/**
 * The workspace layout: one store, one live connection, one sidebar.
 *
 * Every screen under this group reads the same workspace from the store below,
 * which is what keeps the sidebar, the dashboard and the office from disagreeing
 * about the same workspace. Navigation is by route, so a screen keeps its own
 * URL and the browser's back button works; the route group itself adds nothing
 * to the URLs.
 */

import { WorkspaceProvider } from "@/components/dashboard/workspace-data";
import { WorkspaceShell } from "@/components/dashboard/shell";

export default function WorkspaceLayout({ children }: LayoutProps<"/">) {
  return (
    <WorkspaceProvider>
      <WorkspaceShell>{children}</WorkspaceShell>
    </WorkspaceProvider>
  );
}
