import { Suspense } from "react";
import { InviteAccept } from "@/components/auth/onboarding-form";
import { pageMetadata } from "@/lib/metadata";

export const metadata = pageMetadata({
  title: "Undangan",
  description: "Terima undangan untuk bergabung ke workspace Bolu-mu.",
  path: "/join",
  robots: { index: false },
});

// The component reads its query parameter on the client, so the page itself
// stays static and the Suspense boundary covers only the client hook.
export default function JoinPage() {
  return (
    <Suspense fallback={null}>
      <InviteAccept />
    </Suspense>
  );
}
