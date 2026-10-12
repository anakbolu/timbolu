import type { Metadata } from "next";

import { SubscriptionView } from "@/components/settings/subscription-view";

export const metadata: Metadata = {
  title: "Langganan",
  description: "Paket workspace, pemakaian token, paket tambahan, dan riwayat tagihan.",
  robots: { index: false },
};

export default function SubscriptionPage() {
  return <SubscriptionView />;
}
