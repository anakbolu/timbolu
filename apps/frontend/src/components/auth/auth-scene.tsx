"use client";

/**
 * The room beside the login and register cards: the Keluarga Bolu crew at work.
 *
 * The room, the desks, the sign and the status colours are the "Kantor Tim Bolu"
 * view of `design/Dashboard.dc.html`, scaled to the auth column. Each bot rotates
 * through its own act on the shared clock, so the scene reads as work in progress
 * rather than decoration.
 */

import { useEffect, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import type { BoluExpressionName } from "@/components/bolu/characters";
import { ACT_INTERVAL_MS, FEED_AGES, bot } from "@/lib/crew";

/** Status ring colours, from the office legend in the design. */
const STATUS = {
  kerja: "#2FA65A",
  antre: "#E2602B",
  siap: "#4A86E8",
} as const;

type StatusName = keyof typeof STATUS;

type Actor = {
  /** Crew member name, resolved through `bot()` so a typo fails loudly. */
  name: string;
  /** Place the bot works at, shown on its plate. */
  where: string;
  /** Horizontal position in the room, in percent. */
  x: number;
  status: StatusName;
  /** Mood the bot wears while it works. */
  expression: BoluExpressionName;
  /** Rotating "what it is doing right now" lines. */
  acts: string[];
};

type Scene = {
  /** Lettering on the wall sign. */
  sign: string;
  /** Kicker above the room. */
  kicker: string;
  actors: Actor[];
  /** Recently finished work, shown under the room. */
  done: string[];
};

const SCENES: Record<"login" | "signup", Scene> = {
  login: {
    sign: "Pintu masuk",
    kicker: "Bolu sedang bekerja di pintu masuk",
    actors: [
      {
        name: "Biru",
        where: "Pintu depan",
        x: 20,
        status: "kerja",
        expression: "senang",
        acts: ["Menjaga pintu masuk", "Menyapa yang datang", "Membuka pintu untukmu"],
      },
      {
        name: "Ijo",
        where: "Meja daftar",
        x: 50,
        status: "kerja",
        expression: "mikir",
        acts: ["Mengecek daftar anggota", "Mencatat yang baru masuk", "Menyiapkan mejamu"],
      },
      {
        name: "Oren",
        where: "Meja tagihan",
        x: 80,
        status: "antre",
        expression: "iseng",
        acts: [
          "Merapikan meja kerjamu",
          "Menyiapkan pekerjaan tertunda",
          "Menunggu persetujuanmu",
        ],
      },
    ],
    done: ["Ijo menyalakan lampu ruang", "Biru membuka pintu masuk"],
  },
  signup: {
    sign: "Meja pendaftaran",
    kicker: "Bolu sedang menyiapkan tempat kerjamu",
    actors: [
      {
        name: "Pinky",
        where: "Lemari arsip",
        x: 20,
        status: "kerja",
        expression: "gemas",
        acts: ["Menata lemari arsip", "Memberi label nama", "Menyiapkan foldermu"],
      },
      {
        name: "Ijo",
        where: "Buku rekap",
        x: 50,
        status: "kerja",
        expression: "mikir",
        acts: ["Menulis papan nama", "Menyusun buku rekap", "Menata meja kerja"],
      },
      {
        name: "Kunyit",
        where: "Kotak surat",
        x: 80,
        status: "siap",
        expression: "senang",
        acts: ["Menyiapkan kotak surat", "Menyambungkan emailmu", "Menyiapkan draf sambutan"],
      },
    ],
    done: ["Pinky memasang rak baru", "Ijo menulis papan nama"],
  },
};

function CheckMark() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 24 24"
      fill="none"
      stroke="#2FA65A"
      strokeWidth="3"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M5 12.5l4.5 4.5L19 7.5" />
    </svg>
  );
}

/** The room itself. The furniture is drawn per actor, in front of its desk. */
const ROOM_STYLE = {
  aspectRatio: "16 / 10",
  backgroundColor: "#EFE4D6",
  backgroundImage:
    "linear-gradient(#E4D6C4 2px, transparent 2px), linear-gradient(90deg, #E4D6C4 2px, transparent 2px)",
  backgroundSize: "48px 48px",
} as const;

export function AuthScene({ mode }: { mode: "login" | "signup" }) {
  const [tick, setTick] = useState(0);

  useEffect(() => {
    const id = setInterval(() => setTick((value) => value + 1), ACT_INTERVAL_MS);
    return () => clearInterval(id);
  }, []);

  const scene = SCENES[mode];

  return (
    <div className="flex w-full flex-col gap-4">
      <p className="flex items-center gap-2.5 font-display text-[15px] font-semibold text-bolu-ink">
        <span aria-hidden="true" className="pulse size-2.5 rounded-full bg-[#2FA65A]" />
        {scene.kicker}
      </p>

      <div
        aria-hidden="true"
        className="relative w-full overflow-hidden rounded-[26px] border-[10px] border-[#2B2840]"
        style={ROOM_STYLE}
      >
        {/* Back wall, windows, and the sign over the door. */}
        <div className="absolute inset-x-0 top-0 h-[9%] bg-[#DCD3EA]" />
        <div className="absolute left-[6%] top-[1.5%] h-[6%] w-[12%] rounded-[4px] border-[3px] border-white bg-[#BFE3F7]" />
        <div className="absolute left-[34%] top-[1.5%] h-[6%] w-[12%] rounded-[4px] border-[3px] border-white bg-[#BFE3F7]" />
        <div className="absolute left-[62%] top-[1.6%] flex h-[6%] w-[30%] items-center justify-center rounded-[8px] bg-[#2B2840] font-display text-[10px] font-semibold text-white sm:text-[13px]">
          {scene.sign}
        </div>

        {/* Mode props: the door being opened, or the boxes being unpacked. */}
        {mode === "login" ? (
          <div className="absolute bottom-[14%] left-[1%] h-[38%] w-[9%] rounded-t-[10px] border-[3px] border-[#8A6440] bg-[#C99A6B]">
            <span className="absolute right-[12%] top-[46%] size-[10%] rounded-full bg-[#2B2840]" />
          </div>
        ) : (
          <>
            <div className="absolute bottom-[14%] left-[1%] h-[14%] w-[12%] rounded-[4px] border-[3px] border-[#B7895E] bg-[#D9B98C]" />
            <div className="absolute bottom-[26%] left-[2%] h-[11%] w-[10%] rounded-[4px] border-[3px] border-[#B7895E] bg-[#E4CBA6]" />
          </>
        )}

        {scene.actors.map((actor, i) => {
          const member = bot(actor.name);
          const act = actor.acts[(tick + i) % actor.acts.length];
          const color = STATUS[actor.status];

          return (
            <div
              key={actor.name}
              className="absolute w-[26%] -translate-x-1/2"
              style={{ left: `${actor.x}%`, bottom: "12%" }}
            >
              {/* Alternating rows: three bubbles side by side would otherwise overlap. */}
              <div
                className="absolute left-1/2 w-[120%] -translate-x-1/2"
                style={{ bottom: i % 2 ? "82%" : "100%" }}
              >
                <div
                  key={act}
                  className="pop rounded-[12px] border-2 bg-white px-1.5 py-1 text-center text-[10px] font-semibold leading-[1.25] sm:text-[12px]"
                  style={{ borderColor: color }}
                >
                  {act}
                </div>
              </div>

              <Bolu
                member={member}
                expression={actor.expression}
                size="fill"
                className="w-full"
                title={`${member.name}, ${member.role}`}
              />

              {/* Desk front edge, over the feet: the bot works behind its desk. */}
              <div className="absolute -left-[16%] -right-[16%] bottom-[-4%] h-[10%] rounded-[3px] bg-[#C99A6B]" />
              <span className="absolute bottom-[2%] left-[12%] h-[6%] w-[6%] rounded-[2px] bg-white" />
              <span className="absolute bottom-[2%] right-[14%] h-[7%] w-[6%] rounded-[2px] bg-[#2B2840]" />

              <div className="absolute left-1/2 top-[84%] -translate-x-1/2 whitespace-nowrap rounded-[6px] bg-white/85 px-1.5 pb-0.5 text-center">
                <div className="font-display text-[11px] font-semibold leading-[1.2] sm:text-[12px]">
                  {member.name}
                </div>
                <div className="flex items-center justify-center gap-1 text-[9px] leading-[1.2] text-bolu-muted sm:text-[10px]">
                  <span className="size-1.5 flex-none rounded-full" style={{ background: color }} />
                  {actor.where}
                </div>
              </div>
            </div>
          );
        })}
      </div>

      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-[20px] border border-bolu-border bg-white px-4 py-3">
        <span className="font-display text-[14px] font-semibold">Barusan selesai</span>
        {scene.done.map((text, i) => (
          <span key={text} className="flex items-center gap-2 text-[14px] text-bolu-body">
            <CheckMark />
            {text}
            <span className="text-bolu-muted">{FEED_AGES[i]}</span>
          </span>
        ))}
      </div>
    </div>
  );
}
