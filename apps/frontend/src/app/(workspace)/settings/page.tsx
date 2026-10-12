import type { Metadata } from "next";

import { SettingsView } from "@/components/dashboard/settings-view";

export const metadata: Metadata = {
  title: "Pengaturan",
  description:
    "Atur cara tim Bolu bekerja: ruang kerja, jam kerja, persetujuan, dan aplikasi terhubung.",
  robots: { index: false },
};

export default function SettingsPage() {
  return <SettingsView />;
}
