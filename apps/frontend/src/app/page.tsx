import { Faq } from "@/components/bolu/faq";
import { Features } from "@/components/bolu/features";
import { FinalCta } from "@/components/bolu/final-cta";
import { Flow } from "@/components/bolu/flow";
import { Hero } from "@/components/bolu/hero";
import { SiteFooter } from "@/components/bolu/site-footer";
import { SiteNav } from "@/components/bolu/site-nav";
import { Team } from "@/components/bolu/team";
import { WorkspaceTabs } from "@/components/bolu/workspace-tabs";

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
