import type { Metadata } from "next";
import { Suspense } from "react";

import { AdminUsersView } from "@/components/admin/users-view";

export const metadata: Metadata = {
  title: "Pengguna",
  description: "Setiap akun di deployment ini, perannya, dan workspace yang diikutinya.",
  robots: { index: false },
};

/**
 * The list is read from the API at request time, so the read sits inside a
 * Suspense boundary and the page itself stays static.
 */
export default function AdminUsersPage() {
  return (
    <Suspense fallback={null}>
      <AdminUsersView />
    </Suspense>
  );
}
