"use client";

/**
 * The operator layout: the gate, then the shell.
 *
 * It is a client component because both answers it needs — is there a session,
 * and is its account an admin — come from the API rather than from the build.
 * Nothing here reads the request, so the pages below stay static and are shipped
 * as they are.
 */

import { AdminGate } from "@/components/admin/admin-gate";

export default function AdminLayout({ children }: LayoutProps<"/admin">) {
  return <AdminGate>{children}</AdminGate>;
}
