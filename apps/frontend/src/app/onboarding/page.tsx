import type { Metadata } from "next";
import { OnboardingForm } from "@/components/auth/onboarding-form";

export const metadata: Metadata = {
  title: "Kenalan dulu",
  description: "Kenalan singkat sebelum tim Bolu-mu mulai bekerja.",
  robots: { index: false },
};

export default function OnboardingPage() {
  return <OnboardingForm />;
}
