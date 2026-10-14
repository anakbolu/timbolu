import type { Metadata } from "next";

import { NewBoluView } from "@/components/team/new-bolu-view";

export const metadata: Metadata = {
  title: "Tambah Bolu",
  description: "Buat Bolu baru: pilih wujudnya, beri nama, dan sebutkan tugasnya.",
  robots: { index: false },
};

export default function NewBoluPage() {
  return <NewBoluView />;
}
