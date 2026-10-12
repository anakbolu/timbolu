import type { Metadata } from "next";

import { OfficeView } from "@/components/chat/office-view";

export const metadata: Metadata = {
  title: "Kantor",
  description: "Lihat siapa sedang mengerjakan apa: posisi Bolu dihitung dari statusnya.",
};

export default function OfficePage() {
  return (
    <main className="min-h-dvh bg-bolu-bg">
      <OfficeView />
    </main>
  );
}
