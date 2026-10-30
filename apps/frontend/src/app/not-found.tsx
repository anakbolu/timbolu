import type { Metadata } from "next";
import Link from "next/link";
import { BotSvg } from "@/components/bolu/bot-svg";
import { SiteFooter } from "@/components/bolu/site-footer";
import { SiteNav } from "@/components/bolu/site-nav";
import { bot } from "@/lib/crew";

export const metadata: Metadata = {
  title: "Halaman tidak ditemukan",
  robots: { index: false },
};

export default function NotFound() {
  return (
    <>
      <SiteNav />
      <main className="flex-1">
        <div className="mx-auto flex w-full max-w-[720px] flex-col items-center px-6 py-24 text-center">
          <BotSvg bot={bot("Pinky")} className="bob size-24" />

          <p className="mt-8 text-xs uppercase tracking-[0.2em] text-bolu-muted">404</p>

          <h1 className="mt-4 font-display text-[clamp(30px,4.2vw,42px)] font-bold leading-[1.08]">
            Halaman tidak ditemukan
          </h1>

          <p className="mt-5 max-w-[560px] text-bolu-muted">
            Pinky sudah memeriksa semua folder arsip, tapi halaman ini tidak ada di sana.
          </p>

          <Link
            href="/"
            className="mt-9 rounded-full bg-bolu-solid px-6 py-3 font-semibold text-bolu-solid-ink no-underline hover:text-bolu-solid-ink"
          >
            Kembali ke beranda
          </Link>

          {/* Pengunjung yang salah ketik URL notifikasi/draf biasanya butuh dasbor, bukan halaman pemasaran. */}
          <Link
            href="/dashboard"
            className="mt-4 text-[14px] text-bolu-muted no-underline hover:underline"
          >
            Ke dasbor
          </Link>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
