import type { Metadata } from "next";
import { Suspense } from "react";
import { VerifyEmailForm } from "@/components/auth/verify-email-form";

export const metadata: Metadata = {
  title: "Verifikasi email",
  description: "Verifikasi alamat email akun Bolu-mu.",
  robots: { index: false },
};

// The component reads its query parameter on the client, so the page itself
// stays static and the Suspense boundary covers only the client hook.
export default function VerifyEmailPage() {
  return (
    <Suspense fallback={null}>
      <VerifyEmailForm />
    </Suspense>
  );
}
