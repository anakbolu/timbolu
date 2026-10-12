import type { Metadata } from "next";

import { GroupView } from "@/components/chat/group-view";

export const metadata: Metadata = {
  title: "Grup",
  description: "Grup Bolu: beberapa Bolu dalam satu obrolan, dengan router yang memilih penjawab.",
  robots: { index: false },
};

export default function GroupsPage() {
  return (
    <main className="flex min-h-0 flex-1 flex-col">
      <GroupView />
    </main>
  );
}
