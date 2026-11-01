import type { Metadata } from "next";
import { Suspense } from "react";
import { CredentialsForm } from "@/components/auth/credentials-form";

export const metadata: Metadata = {
  title: "Daftar",
  description: "Buat akun Bolu dan dapatkan tim asisten untuk usahamu.",
  robots: { index: false },
};

// The form reads a query parameter on the client, so it sits behind a Suspense
// boundary and the page itself stays static.
export default function SignupPage() {
  return (
    <Suspense fallback={null}>
      <CredentialsForm mode="signup" />
    </Suspense>
  );
}
