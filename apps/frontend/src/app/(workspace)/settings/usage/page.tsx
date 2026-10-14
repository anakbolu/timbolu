import type { Metadata } from "next";
import { Suspense } from "react";

import { UsageView } from "@/components/dashboard/usage-view";

export const metadata: Metadata = {
  title: "Pemakaian",
  description:
    "Pemakaian token dan biaya workspace ini: per periode, per Bolu, dan per tugas — plus tugas yang perlu perhatian.",
  robots: { index: false },
};

/**
 * The window and the "sejak …" on this screen are read from the API's own clock,
 * which is the deployment's and not the visitor's. The Suspense boundary keeps the
 * page static while the read happens at request time, the same way the dashboard
 * covers its clock read.
 */
export default function UsagePage() {
  return (
    <Suspense fallback={null}>
      <UsageView />
    </Suspense>
  );
}
