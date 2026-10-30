import type { Metadata } from "next";
import { Suspense } from "react";
import { InviteAccept } from "@/components/auth/onboarding-form";

export const metadata: Metadata = {
  title: "Undangan",
  description: "Accept an invitation to a Bolu workspace.",
};

// The component reads its query parameter on the client, so the page itself
// stays static and the Suspense boundary covers only the client hook.
export default function JoinPage() {
  return (
    <Suspense fallback={null}>
      <InviteAccept />
    </Suspense>
  );
}
