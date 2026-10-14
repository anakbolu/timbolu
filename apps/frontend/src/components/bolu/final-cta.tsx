import { BotSvg } from "@/components/bolu/bot-svg";
import { StartLink } from "@/components/bolu/start-link";
import { CREW } from "@/lib/crew";

export function FinalCta() {
  return (
    <section
      id="mulai"
      aria-labelledby="mulai-title"
      className="mx-auto w-full max-w-[1248px] px-6 pt-28"
    >
      <div className="flex flex-wrap items-center justify-between gap-8 rounded-md bg-bolu-ink px-9 py-12 text-white">
        <div className="flex max-w-[560px] flex-col gap-3.5">
          <h2
            id="mulai-title"
            className="font-display text-[clamp(32px,4.2vw,48px)] font-bold leading-[1.08]"
          >
            Pulang lebih cepat, kerjaan tetap beres.
          </h2>
          <p className="text-[18px] opacity-80">
            Mulai dari satu pekerjaan dulu, misalnya invoice atau balas chat. Tambah
            anggota tim kapan saja.
          </p>
          <div className="mt-1.5 flex flex-wrap gap-3">
            <StartLink className="inline-flex min-h-12 items-center rounded-full bg-bolu-accent px-[26px] py-3.5 text-[18px] font-bold text-white no-underline hover:text-white">
              Coba gratis
            </StartLink>
            <a
              href="#mulai"
              className="inline-flex min-h-12 items-center rounded-full border-2 border-white px-6 py-3 text-[18px] font-semibold text-white no-underline hover:text-white"
            >
              Jadwalkan demo
            </a>
          </div>
        </div>

        <ul className="flex list-none flex-wrap items-end gap-1 p-0">
          {CREW.map((bot) => (
            <li key={bot.name}>
              <BotSvg
                bot={bot}
                className="bob size-[72px]"
                style={{ animationDelay: bot.delay }}
              />
            </li>
          ))}
        </ul>
      </div>
    </section>
  );
}
