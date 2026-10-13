import { BotSvg } from "@/components/bolu/bot-svg";
import { FLOW } from "@/lib/crew";

export function Flow() {
  return (
    <section
      id="cara-kerja"
      aria-labelledby="cara-kerja-title"
      className="mx-auto flex w-full max-w-[1248px] flex-col gap-9 px-6 pt-28"
    >
      <div className="flex max-w-[700px] flex-col gap-3">
        <h2
          id="cara-kerja-title"
          className="font-display text-[clamp(34px,4.6vw,52px)] font-bold leading-[1.08]"
        >
          Satu pesanan, dioper dari tangan ke tangan
        </h2>
        <p className="text-[19px] text-bolu-muted">
          Contoh alurnya: pelanggan pesan lewat WhatsApp, dan sampai bukti bayarnya
          tersimpan rapi, kamu cuma muncul sekali, yaitu saat menyetujui invoice.
        </p>
      </div>

      <div className="flex flex-col gap-7 rounded border border-bolu-border bg-white px-7 py-8">
        <div aria-hidden="true" className="relative mx-[6%] h-3.5">
          <div className="absolute inset-x-0 top-1.5 border-t-2 border-dashed border-[#C9CDE0]" />
          <div className="courier absolute top-0 -ml-[13px] h-3.5 w-[26px] rounded bg-bolu-accent" />
        </div>

        <ol className="grid list-none grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-5 p-0">
          {FLOW.map((step) => (
            <li key={step.n} className="flex flex-col gap-2.5">
              <div className="flex items-center gap-2.5">
                <div className="flex size-[30px] flex-none items-center justify-center rounded-full bg-bolu-ink text-[15px] font-bold text-white">
                  {step.n}
                </div>
                <div className="size-[58px]">
                  {step.bot ? (
                    <BotSvg
                      bot={step.bot}
                      className="bob size-[58px]"
                      style={{ animationDelay: step.bot.delay }}
                    />
                  ) : (
                    <div className="box-border flex size-[58px] items-center justify-center rounded-full border-[3px] border-dashed border-bolu-ink font-display text-[16px] font-bold">
                      Kamu
                    </div>
                  )}
                </div>
              </div>
              <div className="font-display text-[19px] font-semibold leading-[1.2]">
                {step.title}
              </div>
              <div className="text-[15px] text-bolu-muted">{step.text}</div>
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}
