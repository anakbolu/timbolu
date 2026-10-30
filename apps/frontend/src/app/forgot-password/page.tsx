import type { Metadata } from "next";
import { Suspense } from "react";
import { PasswordResetForm } from "@/components/auth/password-reset-form";

export const metadata: Metadata = {
  title: "Atur ulang kata sandi",
  description: "Reset the password of your Bolu account.",
};

// The component reads its query parameter on the client, so the page itself
// stays static and the Suspense boundary covers only the client hook.
export default function ForgotPasswordPage() {
  return (
    <Suspense fallback={null}>
      <PasswordResetForm />
    </Suspense>
  );
}
