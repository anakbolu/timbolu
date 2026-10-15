import type { Metadata } from "next";
import { Suspense } from "react";

import { AdminOverviewView } from "@/components/admin/overview-view";

export const metadata: Metadata = {
  title: "Ringkasan Admin",
  description: "Jumlah akun di deployment ini, dan berapa yang boleh membuka halaman admin.",
  robots: { index: false },
};

/**
 * The numbers are read from the API at request time, so the read sits inside a
 * Suspense boundary and the page itself stays static — the same shape every
 * screen in this app has.
 */
export default function AdminDashboardPage() {
  return (
    <Suspense fallback={null}>
      <AdminOverviewView />
    </Suspense>
  );
}
