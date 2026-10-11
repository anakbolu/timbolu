import { BotSvg } from "@/components/bolu/bot-svg";
import { ApprovalsView } from "./approvals-view";
import type { Dashboard } from "./use-dashboard-state";

/** The Dasbor view: greeting, "Suruh Bolu", stats, pending drafts, feed, week, bills. */
export function DashView({ d }: { d: Dashboard }) {
  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
            Selamat siang
          </h1>
          <div className="text-bolu-muted">Rabu, 7 Oktober · ini ringkasan kerja tim kecilmu</div>
        </div>
        <div className="flex items-center gap-2.5">
          <button
            type="button"
            aria-label="Notifikasi"
            className="flex size-11 cursor-pointer items-center justify-center rounded-full border border-bolu-border bg-white"
          >
            <svg
              width="20"
              height="20"
              viewBox="0 0 24 24"
              fill="none"
              stroke="#1E1B2E"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M6 16V11a6 6 0 0 1 12 0v5l1.5 2h-15zM10 20.5a2.2 2.2 0 0 0 4 0" />
            </svg>
          </button>
          <div className="flex size-11 items-center justify-center rounded-full bg-[#FDE7DA] font-display font-bold">
            [A]
          </div>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3.5 rounded-[26px] bg-bolu-ink p-[18px_20px] text-white">
        <label
          htmlFor="suruh"
          className="flex-none font-display text-[19px] font-semibold max-[760px]:basis-full"
        >
          Suruh Bolu
        </label>
        <input
          id="suruh"
          type="text"
          value={d.dashMsg}
          onChange={(event) => d.onDashMsg(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") d.sendDash();
          }}
          placeholder="misalnya: tagih semua invoice yang telat"
          className="min-h-[46px] min-w-0 flex-[1_1_260px] rounded-full border-0 bg-white px-[18px] text-bolu-ink"
        />
        <button
          type="button"
          onClick={d.sendDash}
          className="min-h-[46px] cursor-pointer rounded-full border-0 bg-bolu-accent px-[22px] font-bold text-white"
        >
          Kirim tugas
        </button>
        <div className="basis-full text-[14px] opacity-75">
          Tugas otomatis diteruskan ke anggota yang paling cocok, dan obrolannya muncul di daftar
          kiri.
        </div>
      </div>

      <div className="grid grid-cols-[repeat(auto-fit,minmax(min(200px,100%),1fr))] gap-3.5">
        {d.stats.map((stat) => (
          <div
            key={stat.label}
            className="flex flex-col gap-1 rounded-[26px] border border-bolu-border bg-white p-[18px_20px]"
          >
            <div className="text-[15px] font-medium text-bolu-muted">{stat.label}</div>
            <div className="font-display text-[34px] font-bold leading-[1.1]">{stat.value}</div>
            <div className="text-[14px] text-bolu-muted">{stat.note}</div>
          </div>
        ))}
      </div>

      <div className="flex flex-wrap items-start gap-[18px]">
        {/* The approval pane is the real one: it reads the workspace's drafts from
            the API and decides through the same endpoint the chat card uses, so
            the two surfaces cannot disagree. The mock that stood here was a
            design placeholder. */}
        <div className="flex min-w-0 flex-[999_1_420px] flex-col gap-3 rounded-[26px] border border-bolu-border bg-white p-5">
          <ApprovalsView />
        </div>

        <div className="flex min-w-0 flex-[1_1_320px] flex-col gap-2.5 rounded-[26px] border border-bolu-border bg-white p-5">
          <div className="flex items-center justify-between">
            <h2 className="font-display text-[22px] font-semibold">Aktivitas</h2>
            <span className="pulse text-[13px] font-bold text-bolu-lunas">LIVE</span>
          </div>
          <div className="flex flex-col">
            {d.feed.map((row) => (
              <div key={row.key} className={`${row.cls} flex gap-3 border-b border-bolu-line p-[9px_0]`}>
                <div className="size-8 flex-none">
                  {row.isYou ? (
                    <div className="flex size-8 items-center justify-center rounded-full bg-bolu-ink">
                      <svg
                        width="16"
                        height="16"
                        viewBox="0 0 24 24"
                        fill="none"
                        stroke="#FFFFFF"
                        strokeWidth="3"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        aria-hidden="true"
                      >
                        <path d="M5 12.5l4.5 4.5L19 7.5" />
                      </svg>
                    </div>
                  ) : (
                    <BotSvg bot={row.bot} className="size-8" />
                  )}
                </div>
                <div className="min-w-0 flex-1 text-[15px] leading-[1.4]">
                  <span className="font-semibold">{row.who}</span> {row.text}
                </div>
                <div className="flex-none text-[13px] text-bolu-muted">{row.time}</div>
              </div>
            ))}
          </div>
        </div>
      </div>

      <div className="flex flex-wrap items-stretch gap-[18px]">
        <div className="flex min-w-0 flex-[999_1_460px] flex-col gap-4 rounded-[26px] border border-bolu-border bg-white p-5">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <h2 className="font-display text-[22px] font-semibold">Tugas selesai minggu ini</h2>
            <span className="text-[14px] text-bolu-muted">per hari, semua anggota</span>
          </div>
          <div className="grid h-[190px] grid-cols-7 items-end gap-3">
            {d.week.map((bar) => (
              <div key={bar.day} className="flex h-full flex-col items-center justify-end gap-1.5">
                <div className="text-[13px] font-semibold">{bar.label}</div>
                <div
                  className="w-full max-w-[46px] rounded-xl"
                  style={{ height: bar.h, backgroundColor: bar.bg }}
                />
                <div className="text-[14px] text-bolu-muted" style={{ fontWeight: bar.weight }}>
                  {bar.day}
                </div>
              </div>
            ))}
          </div>
        </div>

        <div className="flex min-w-0 flex-[1_1_320px] flex-col gap-2.5 rounded-[26px] border border-bolu-border bg-white p-5">
          <h2 className="font-display text-[22px] font-semibold">Tagihan berjalan</h2>
          {d.bills.map((bill) => (
            <div
              key={bill.no}
              className="flex items-center justify-between gap-2.5 border-b border-bolu-line p-[8px_0] text-[15px]"
            >
              <div className="min-w-0">
                <div className="font-semibold">{bill.name}</div>
                <div className="text-[13px] text-bolu-muted">{bill.no}</div>
              </div>
              <div className="text-right">
                <div className="font-semibold">{bill.amt}</div>
                <div className="text-[13px] font-bold" style={{ color: bill.color }}>
                  {bill.status}
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
