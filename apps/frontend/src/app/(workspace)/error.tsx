"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { BotSvg } from "@/components/bolu/bot-svg";
import { bot } from "@/lib/crew";

export default function WorkspaceError({
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

      {error.digest ? (
        <p className="mt-2 text-xs text-bolu-muted">
          Kode galat: <span className="font-mono">{error.digest}</span>
        </p>
      ) : null}

      <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
        <button
          type="button"
          onClick={recover}
          className="cursor-pointer rounded-full bg-bolu-solid px-5 py-2.5 text-sm font-semibold text-bolu-solid-ink transition hover:opacity-90"
        >
          {retried ? "Muat ulang halaman" : "Coba lagi"}
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
