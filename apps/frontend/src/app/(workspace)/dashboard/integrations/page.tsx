import type { Metadata } from "next";

import { IntegrationsPanel } from "@/components/dashboard/integrations-view";

export const metadata: Metadata = {
  title: "Integrasi",
  description:
    "Sambungkan akun yang dipakai tim Bolu, atur siapa yang boleh memakainya, dan kelola server MCP.",
  robots: { index: false },
};

export default function IntegrationsPage() {
  return <IntegrationsPanel />;
}
