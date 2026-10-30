import type { Metadata } from "next";
import { Faq } from "@/components/bolu/faq";
import { Features } from "@/components/bolu/features";
import { FinalCta } from "@/components/bolu/final-cta";
import { Flow } from "@/components/bolu/flow";
import { Hero } from "@/components/bolu/hero";
import { SiteFooter } from "@/components/bolu/site-footer";
import { SiteNav } from "@/components/bolu/site-nav";
import { Team } from "@/components/bolu/team";
import { WorkspaceTabs } from "@/components/bolu/workspace-tabs";
import { openGraphCard } from "@/lib/metadata";

// The landing's card copy is the one the layout already declares; it is repeated
// here because a page-level `openGraph` replaces the inherited object wholesale.
// What this page adds is the canonical URL and the Twitter card. It exports no
// `title`/`description`: the layout's default already titles this page, and a
// page-level title string would be decorated by the layout's template again.
export const metadata: Metadata = openGraphCard({
  title: "Timbolu — pekerjaan berulang, beres sampai tuntas",
  description:
    "Bot Timbolu menyelesaikan pekerjaan lintas langkah di komputer virtual, dengan persetujuanmu sebelum aksi eksternal.",
  path: "/",
});

export default function Home() {
  return (
    <>
      <SiteNav />
      <main className="flex-1">
        <Hero />
        <Flow />
        <WorkspaceTabs />
        <Features />
        <Team />
        <Faq />
        <FinalCta />
      </main>
      <SiteFooter />
    </>
  );
}
