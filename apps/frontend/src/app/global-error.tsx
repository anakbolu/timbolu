"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { BotSvg } from "@/components/bolu/bot-svg";
import { bot } from "@/lib/crew";

export default function GlobalError({
  error,
  retry,
}: {
  error: Error & { digest?: string };
  /** Diteruskan App Router; tidak dipanggil sendiri karena `retry` sudah memanggilnya. */
  reset: () => void;
  retry: () => void;
}) {
  // `reset()` hanya membersihkan state boundary, jadi galat yang deterministik langsung
  // terlempar lagi dan tombolnya tampak mati. Percobaan pertama memakai `retry()`
  // (refresh + reset); kalau boundary terlempar lagi, payload-nya sendiri yang rusak dan
  // hanya muat ulang penuh yang bisa menggantinya.
  const [retried, setRetried] = useState(false);

  const recover = () => {
    if (!retried) {
      setRetried(true);
      retry();
      return;
    }

    window.location.reload();
  };

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

          {error.digest ? (
            <p className="mt-2 text-xs text-bolu-muted">
              Kode galat: <span className="font-mono">{error.digest}</span>
            </p>
          ) : null}

          <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
            <button
              type="button"
              onClick={recover}
              className="cursor-pointer rounded-full bg-bolu-solid px-6 py-2.5 text-sm font-semibold text-bolu-solid-ink transition hover:opacity-90"
            >
              {retried ? "Muat ulang halaman" : "Coba lagi"}
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
