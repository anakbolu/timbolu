import type { Metadata } from "next";

import { ErasureView } from "@/components/dashboard/erasure-view";

export const metadata: Metadata = {
  title: "Data",
  description:
    "Apa yang Bolu simpan, di mana, berapa lama, dan cara meminta penghapusan seluruh data workspace.",
  robots: { index: false },
};

/** The data policy and the erasure request. It reads nothing on the server, so it is static. */
export default function DataPage() {
  return <ErasureView />;
}
