"use client";

import Link from "next/link";
import { useEffect } from "react";
import { BotSvg } from "@/components/bolu/bot-svg";
import { bot } from "@/lib/crew";

export default function WorkspaceError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // Frontend belum memiliki utilitas telemetri atau log tersentralisasi (mis. Sentry/Datadog),
    // sehingga error dicatat langsung ke console agar mudah ditelusuri dari inspect tool peramban.
    console.error("Workspace render error:", error);
  }, [error]);

  return (
    <div className="mx-auto flex w-full max-w-[640px] flex-1 flex-col items-center justify-center px-6 py-20 text-center">
      <BotSvg bot={bot("Pinky")} className="bob size-20" />

      <p className="mt-6 text-xs font-semibold uppercase tracking-[0.2em] text-bolu-muted">
        Galat Halaman
      </p>

      <h1 className="mt-3 font-display text-[clamp(24px,3.5vw,34px)] font-bold leading-[1.12]">
        Ada yang salah di halaman ini
      </h1>

      <p className="mt-3 max-w-[480px] text-sm text-bolu-muted">
        Halaman tidak dapat dimuat dengan benar. Kamu bisa mencoba memuat ulang tampilan atau kembali ke dasbor utama.
      </p>

      <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
        <button
          type="button"
          onClick={() => reset()}
          className="cursor-pointer rounded-full bg-bolu-solid px-5 py-2.5 text-sm font-semibold text-bolu-solid-ink transition hover:opacity-90"
        >
          Coba lagi
        </button>

        <Link
          href="/dashboard"
          className="rounded-full border border-bolu-border bg-bolu-surface px-5 py-2.5 text-sm font-semibold text-bolu-ink transition hover:bg-bolu-panel no-underline"
        >
          Kembali ke dasbor
        </Link>
      </div>
    </div>
  );
}
