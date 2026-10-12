import type { Metadata } from "next";
import { Suspense } from "react";

import { DashView } from "@/components/dashboard/dash-view";

export const metadata: Metadata = {
  title: "Dasbor",
  description:
    "Dasbor Keluarga Bolu: tim Bolu-mu, obrolan yang berjalan, dan keputusan yang menunggu.",
  robots: { index: false },
};

/**
 * The greeting, the date and every "3 menit lalu" on this screen are read from the
 * clock, which is the visitor's own and not the build's. The Suspense boundary is
 * what keeps the page static while the read happens at request time, the same way
 * the auth pages cover their query-string reads.
 */
export default function DashboardPage() {
  return (
    <Suspense fallback={null}>
      <DashView />
    </Suspense>
  );
}
