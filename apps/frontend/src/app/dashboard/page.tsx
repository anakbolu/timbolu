import type { Metadata } from "next";

import { DashboardOverview } from "@/components/dashboard/dashboard-overview";

export const metadata: Metadata = {
  title: "Dasbor",
  description:
    "Dasbor Keluarga Bolu: tim Bolu-mu, obrolan yang berjalan, dan keputusan yang menunggu.",
  robots: { index: false },
};

export default function DashboardPage() {
  return <DashboardOverview />;
}
