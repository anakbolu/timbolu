import { BotSvg } from "@/components/bolu/bot-svg";
import { CREW } from "@/lib/crew";

export function Team() {
  return (
    <section
      id="tim"
      aria-labelledby="tim-title"
      className="mx-auto flex w-full max-w-[1248px] flex-col gap-9 px-6 pt-28"
    >
      <div className="flex max-w-[680px] flex-col gap-3">
        <h2
          id="tim-title"
          className="font-display text-[clamp(34px,4.6vw,52px)] font-bold leading-[1.08]"
        >
          Kenalan dulu dengan timnya
        </h2>
        <p className="text-[19px] text-bolu-muted">
          Masing-masing punya satu keahlian. Kalau satu tugas butuh banyak langkah, mereka
          saling oper tanpa perlu kamu suruh satu-satu.
        </p>
      </div>

      <ul className="grid list-none grid-cols-[repeat(auto-fit,minmax(300px,1fr))] gap-[18px] p-0">
        {CREW.map((bot) => (
          <li
            key={bot.name}
            className="flex items-start gap-[18px] rounded-xl border border-bolu-border bg-white p-[22px]"
          >
            <div
              className="box-border size-[92px] flex-none rounded-xl p-2"
              style={{ background: bot.tint }}
            >
              <BotSvg bot={bot} className="size-full" />
            </div>
            <div className="flex min-w-0 flex-col gap-1.5">
              <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1">
                <span className="font-display text-[24px] font-bold">{bot.name}</span>
                <span className="text-[15px] font-semibold text-bolu-muted">
                  {bot.role}
                </span>
              </div>
              <p className="text-[16px] text-bolu-body">{bot.bio}</p>
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
