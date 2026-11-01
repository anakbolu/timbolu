"use client";

import Link from "next/link";
import { useEffect } from "react";
import { BotSvg } from "@/components/bolu/bot-svg";
import { bot } from "@/lib/crew";

export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // Frontend belum memiliki layanan telemetri/log eksternal (Sentry/Datadog),
    // sehingga error dicatat ke console untuk diagnosa runtime browser.
    console.error("Global application error:", error);
  }, [error]);

  return (
    <html lang="id" className="h-full antialiased">
      <body className="flex min-h-full flex-col items-center justify-center bg-bolu-bg p-6 font-sans text-bolu-ink">
        <main className="mx-auto flex w-full max-w-[560px] flex-col items-center text-center">
          <BotSvg bot={bot("Pinky")} className="bob size-24" />

          <p className="mt-8 text-xs font-semibold uppercase tracking-[0.2em] text-bolu-muted">
            Galat Sistem
          </p>

          <h1 className="mt-4 font-display text-[clamp(28px,3.8vw,38px)] font-bold leading-[1.1]">
            Ada yang salah di aplikasi
          </h1>

          <p className="mt-4 max-w-[480px] text-sm text-bolu-muted">
            Terjadi masalah yang tidak terduga saat memproses halaman ini. Kamu bisa mencoba memuat ulang komponen atau kembali ke dasbor.
          </p>

          <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
            <button
              type="button"
              onClick={() => reset()}
              className="cursor-pointer rounded-full bg-bolu-solid px-6 py-2.5 text-sm font-semibold text-bolu-solid-ink transition hover:opacity-90"
            >
              Coba lagi
            </button>

            <Link
              href="/dashboard"
              className="rounded-full border border-bolu-border bg-bolu-surface px-6 py-2.5 text-sm font-semibold text-bolu-ink transition hover:bg-bolu-panel no-underline"
            >
              Kembali ke dasbor
            </Link>
          </div>
        </main>
      </body>
    </html>
  );
}
