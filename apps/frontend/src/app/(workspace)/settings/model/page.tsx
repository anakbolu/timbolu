import type { Metadata } from "next";

import { ModelView } from "@/components/settings/model-view";

export const metadata: Metadata = {
  title: "Model",
  description: "Penyedia model, rantai fallback, dan pemakaian token workspace.",
};

export default function ModelSettingsPage() {
  return <ModelView />;
}