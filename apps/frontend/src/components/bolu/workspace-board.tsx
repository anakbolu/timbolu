"use client";

import { Check } from "lucide-react";
import { useEffect, useState } from "react";
import { BotSvg } from "@/components/bolu/bot-svg";
import { ACT_INTERVAL_MS, CREW, FEED, FEED_AGES } from "@/lib/crew";

function CheckMark() {
  return <Check size={18} strokeWidth={3} color="#2FA65A" aria-hidden="true" />;
}

/**
 * The live board: each bot rotates through its acts on a shared tick,
 * and the "Barusan selesai" feed shifts with the same tick.
 */
export function WorkspaceBoard() {
  const [tick, setTick] = useState(0);

  useEffect(() => {
    const id = setInterval(() => setTick((value) => value + 1), ACT_INTERVAL_MS);
    return () => clearInterval(id);
  }, []);

  const feed = FEED_AGES.map((when, k) => ({
    text: FEED[(tick + FEED.length - k) % FEED.length],
    when,
  }));

  return (
    <div className="bolu-board flex flex-col gap-5 rounded-md p-7">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2.5 font-semibold">
          <span
            aria-hidden="true"
            className="inline-block size-2.5 rounded-full bg-[#2FA65A]"
          />
          Ruang kerja · semua sedang bekerja
        </div>
        <div className="text-[15px] text-bolu-muted">Hari ini, Rabu</div>
      </div>

      <ul className="grid list-none grid-cols-[repeat(auto-fit,minmax(170px,1fr))] gap-4 p-0">
        {CREW.map((bot, i) => {
          const act = bot.acts[(tick + i) % bot.acts.length];
          return (
            <li
              key={bot.name}
              className="flex flex-col items-center gap-3 rounded-md bg-white p-3.5 pb-4"
            >
              <div
                className="box-border flex min-h-[76px] w-full flex-col justify-between gap-2 rounded-md px-3 py-2.5"
                style={{ background: bot.tint }}
              >
                <div key={act} className="pop text-[14px] font-semibold leading-[1.3]">
                  {act}
                </div>
                <div className="h-1.5 overflow-hidden rounded-md bg-[rgb(30_27_46_/_0.1)]">
                  <div
                    className="bar h-full rounded-md"
                    style={{ width: "40%", background: bot.color, animationDelay: bot.delay }}
                  />
                </div>
              </div>

              <BotSvg bot={bot} className="bob size-[104px]" style={{ animationDelay: bot.delay }} />

              <div className="text-center leading-[1.25]">
                <div className="font-display text-[19px] font-semibold">{bot.name}</div>
                <div className="text-[14px] text-bolu-muted">{bot.role}</div>
              </div>
            </li>
          );
        })}
      </ul>

      <div className="flex flex-wrap items-center gap-x-7 gap-y-2.5 rounded-md bg-white px-[18px] py-3.5">
        <div className="font-display font-semibold">Barusan selesai</div>
        {feed.map((item) => (
          <div
            key={item.text}
            className="pop flex items-center gap-2 text-[15px]"
          >
            <CheckMark />
            <span>{item.text}</span>
            <span className="text-bolu-muted">{item.when}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
