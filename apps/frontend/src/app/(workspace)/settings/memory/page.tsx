import type { Metadata } from "next";

import { MemoryView } from "@/components/settings/memory-view";

export const metadata: Metadata = {
  title: "Memori",
  description: "Apa yang diingat setiap Bolu, apa yang berlaku untuk seluruh workspace, dan basis pengetahuan.",
};

export default function MemorySettingsPage() {
  return <MemoryView />;
}
