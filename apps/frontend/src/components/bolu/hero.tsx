import { StartLink } from "@/components/bolu/start-link";
import { WorkspaceBoard } from "@/components/bolu/workspace-board";

export function Hero() {
  return (
    <section
      id="atas"
      aria-labelledby="hero-title"
      className="mx-auto flex w-full max-w-[1248px] flex-col gap-10 px-6 pb-6 pt-12"
    >
      <div className="flex max-w-[780px] flex-col gap-5">
        <h1
          id="hero-title"
          className="font-display text-[clamp(42px,6.4vw,76px)] font-bold leading-[1.02] tracking-[-0.02em]"
        >
          Delegasikan satu proses kerja. Timbolu yang menuntaskannya.
        </h1>
        <p className="max-w-[620px] text-[20px] text-bolu-muted">
          Timbolu menjalankan pekerjaan berulang dari awal sampai selesai di komputer
          virtualnya—dari membalas pesanan WhatsApp, mencatatnya, sampai menyiapkan
          invoice. Kamu meninjau dan menyetujui sebelum apa pun dikirim.
        </p>
        <div className="mt-1 flex flex-wrap gap-3">
          <StartLink className="inline-flex min-h-12 items-center rounded-full bg-bolu-accent px-[26px] py-3.5 text-[18px] font-bold text-bolu-solid-ink no-underline hover:text-bolu-solid-ink">
            Coba gratis
          </StartLink>
          <a
            href="#cara-kerja"
            className="inline-flex min-h-12 items-center rounded-full border-2 border-bolu-ink px-6 py-3 text-[18px] font-semibold text-bolu-ink no-underline hover:text-bolu-ink"
          >
            Lihat satu alur kerja
          </a>
        </div>
      </div>

      <WorkspaceBoard />
    </section>
  );
}
