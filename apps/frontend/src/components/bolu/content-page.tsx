import type { ReactNode } from "react";
import Link from "next/link";
import { SiteFooter } from "@/components/bolu/site-footer";
import { SiteNav } from "@/components/bolu/site-nav";

export function ContentPage({ title, intro, children }: { title: string; intro: string; children: ReactNode }) {
  return (
    <>
      <SiteNav />
      <main className="mx-auto w-full max-w-[900px] flex-1 px-6 pb-16 pt-10">
        <Link href="/" className="text-sm no-underline">← Kembali ke beranda</Link>
        <header className="mb-9 mt-7">
          <h1 className="font-display text-4xl font-bold leading-tight sm:text-5xl">{title}</h1>
          <p className="mt-4 max-w-3xl text-lg text-bolu-muted">{intro}</p>
        </header>
        <div className="space-y-8 text-bolu-body [&_h2]:mb-2 [&_h2]:font-display [&_h2]:text-2xl [&_h2]:font-semibold [&_p]:mt-2 [&_ul]:mt-3 [&_ul]:list-disc [&_ul]:space-y-2 [&_ul]:pl-6">
          {children}
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
