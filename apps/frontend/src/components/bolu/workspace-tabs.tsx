"use client";

import { MoveRight } from "lucide-react";
import type { CSSProperties } from "react";
import { useState } from "react";
import { BotSvg } from "@/components/bolu/bot-svg";
import {
  ARSIP_FILES,
  INVOICE_LINES,
  REKAP_ROWS,
  TABS,
  bot,
  type TabId,
} from "@/lib/crew";

function InvoiceMock() {
  return (
    <div className="flex flex-col gap-[18px] rounded-xl bg-bolu-surface p-7">
      <div className="flex flex-wrap justify-between gap-3">
        <div>
          <div className="font-display text-[28px] font-bold">Invoice</div>
          <div className="text-[15px] text-bolu-muted">INV-0042 · jatuh tempo 14 hari</div>
        </div>
        <div className="text-right text-[15px]">
          <div className="font-semibold">Kepada</div>
          <div className="text-bolu-muted">[NAMA PELANGGAN]</div>
        </div>
      </div>

      <div className="overflow-x-auto">
        <div className="flex min-w-[380px] flex-col text-[15px]">
          <div className="grid grid-cols-[3fr_1fr_2fr] gap-3 border-b-2 border-bolu-ink py-2.5 font-semibold">
            <span>Barang</span>
            <span>Qty</span>
            <span className="text-right">Jumlah</span>
          </div>
          {INVOICE_LINES.map((line) => (
            <div
              key={line.barang}
              className="grid grid-cols-[3fr_1fr_2fr] gap-3 border-b border-bolu-border py-2.5"
            >
              <span>{line.barang}</span>
              <span>{line.qty}</span>
              <span className="text-right">{line.jumlah}</span>
            </div>
          ))}
          <div className="grid grid-cols-[3fr_1fr_2fr] gap-3 py-3 font-bold">
            <span>Total</span>
            <span />
            <span className="text-right">Rp 1.124.000</span>
          </div>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2.5">
        <button
          type="button"
          disabled
          className="min-h-11 rounded-full border-0 bg-bolu-solid px-[22px] py-3 font-semibold text-bolu-solid-ink"
        >
          Setujui &amp; kirim
        </button>
        <button
          type="button"
          disabled
          className="min-h-11 rounded-full border-2 border-bolu-ink bg-transparent px-5 py-2.5 font-semibold text-bolu-ink"
        >
          Ubah dulu
        </button>
        <span className="text-[14px] text-bolu-muted">Draf disiapkan Oren</span>
      </div>
    </div>
  );
}

function WhatsAppMock() {
  return (
    <div className="flex max-w-[520px] flex-col overflow-hidden rounded-xl bg-bolu-surface">
      <div className="flex items-center gap-3 bg-bolu-solid px-5 py-3.5 text-bolu-solid-ink">
        <div className="flex size-9 items-center justify-center rounded-full bg-[#F59ABF] font-bold text-bolu-on-tint">
          R
        </div>
        <div>
          <div className="font-semibold">Rina</div>
          <div className="text-[13px] opacity-75">pelanggan</div>
        </div>
      </div>
      <div className="flex flex-col gap-3 bg-bolu-panel p-5">
        <div className="max-w-[80%] self-start rounded-[16px_16px_16px_4px] bg-bolu-surface px-3.5 py-2.5 text-[15px]">
          Kak, pesanan atas nama Rina udah dikirim belum?
        </div>
        <div className="max-w-[80%] self-end rounded-[16px_16px_4px_16px] bg-bolu-accent/15 px-3.5 py-2.5 text-[15px]">
          Halo Kak Rina! Pesananmu sudah dikirim hari ini lewat [KURIR], nomor resinya [NO
          RESI]. Terima kasih sudah belanja ya.
        </div>
        <div className="max-w-[80%] self-start rounded-[16px_16px_16px_4px] bg-bolu-surface px-3.5 py-2.5 text-[15px]">
          Siap kak, makasih. Kalau mau order lagi masih ready?
        </div>
        <div className="flex items-center gap-2 self-end text-[14px] text-bolu-muted">
          <span>Biru sedang mengetik</span>
          <span className="dots" aria-hidden="true">
            <span />
            <span />
            <span />
          </span>
        </div>
      </div>
    </div>
  );
}

function RekapMock() {
  return (
    <div className="flex flex-col gap-3.5 rounded-xl bg-bolu-surface p-6">
      <div className="flex flex-wrap justify-between gap-2">
        <div className="font-display text-[24px] font-bold">Rekap pesanan minggu ini</div>
        <div className="text-[14px] text-bolu-muted">diperbarui Ijo barusan</div>
      </div>
      <div className="overflow-x-auto">
        <div className="flex min-w-[520px] flex-col text-[15px]">
          <div className="grid grid-cols-[1fr_1.4fr_1.6fr_1.2fr_1fr] gap-2.5 rounded-xl bg-bolu-panel px-3 py-2.5 font-semibold">
            <span>Tanggal</span>
            <span>Pelanggan</span>
            <span>Barang</span>
            <span>Total</span>
            <span>Status</span>
          </div>
          {REKAP_ROWS.map((row) => (
            <div
              key={`${row.tgl}-${row.nama}`}
              className="grid grid-cols-[1fr_1.4fr_1.6fr_1.2fr_1fr] gap-2.5 border-b border-bolu-border px-3 py-2.5"
            >
              <span>{row.tgl}</span>
              <span>{row.nama}</span>
              <span>{row.barang}</span>
              <span>{row.total}</span>
              <span
                className={`font-semibold ${
                  row.status === "Lunas" ? "text-bolu-lunas" : "text-bolu-belum"
                }`}
              >
                {row.status}
              </span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function ArsipMock() {
  return (
    <div className="flex flex-col gap-3 rounded-xl bg-bolu-surface p-6">
      <div className="font-display text-[24px] font-bold">
        Folder Unduhan → Arsip/2026/Oktober
      </div>
      {ARSIP_FILES.map((file) => (
        <div
          key={file.from}
          className="flex flex-wrap items-center gap-x-3.5 gap-y-2 rounded-xl border border-bolu-border px-3.5 py-3 text-[15px]"
        >
          <span className="text-bolu-muted line-through">{file.from}</span>
          <MoveRight size={20} strokeWidth={2.5} aria-hidden="true" />
          <span className="font-semibold">{file.to}</span>
        </div>
      ))}
    </div>
  );
}

export function WorkspaceTabs() {
  const [tab, setTab] = useState<TabId>("inv");
  const current = TABS.find((entry) => entry.id === tab) ?? TABS[0];

  return (
    <section
      id="intip"
      aria-labelledby="intip-title"
      className="mx-auto flex w-full max-w-[1248px] flex-col gap-7 px-6 pt-28"
    >
      <div className="flex max-w-[680px] flex-col gap-3">
        <h2
          id="intip-title"
          className="font-display text-[clamp(34px,4.6vw,52px)] font-bold leading-[1.08]"
        >
          Intip meja kerja mereka
        </h2>
        <p className="text-[19px] text-bolu-muted">
          Pilih satu pekerjaan dan lihat siapa yang mengerjakan apa.
        </p>
      </div>

      <div role="tablist" aria-label="Pilih pekerjaan" className="flex flex-wrap gap-2.5">
        {TABS.map((entry) => {
          const selected = entry.id === tab;
          return (
            <button
              key={entry.id}
              type="button"
              role="tab"
              id={`tab-${entry.id}`}
              aria-selected={selected}
              aria-controls="intip-panel"
              onClick={() => setTab(entry.id)}
              className={`min-h-11 rounded-full border-2 border-bolu-ink px-5 py-2.5 font-semibold ${
                selected ? "bg-bolu-solid text-bolu-solid-ink" : "bg-bolu-surface text-bolu-ink"
              }`}
            >
              {entry.label}
            </button>
          );
        })}
      </div>

      <div
        id="intip-panel"
        role="tabpanel"
        aria-labelledby={`tab-${tab}`}
        tabIndex={0}
        className="intip-panel flex flex-wrap items-start gap-7 rounded-xl p-7"
        // The tab's own pastel, handed to the stylesheet rather than painted
        // here: the light theme uses it as the backdrop of the mock workspace,
        // and the dark theme has to blend it into the panel instead of putting a
        // light block on a dark page.
        style={{ "--tint": current.tint } as CSSProperties}
      >
        <div className="min-w-0 flex-[999_1_460px]">
          {tab === "inv" ? <InvoiceMock /> : null}
          {tab === "wa" ? <WhatsAppMock /> : null}
          {tab === "rekap" ? <RekapMock /> : null}
          {tab === "arsip" ? <ArsipMock /> : null}
        </div>

        <div className="min-w-0 flex-[1_1_300px]">
          <ul className="flex list-none flex-col gap-3.5 p-0">
            {current.steps.map((step) => {
              const member = bot(step.who);
              return (
                <li
                  key={`${current.id}-${step.who}-${step.text}`}
                  className="flex items-center gap-3.5 rounded-xl border border-bolu-border bg-bolu-surface py-3 pl-3 pr-4"
                >
                  <BotSvg bot={member} className="size-12 flex-none" />
                  <div className="flex min-w-0 flex-col gap-0.5">
                    <div className="font-display text-[17px] font-semibold">{step.who}</div>
                    <div className="text-[15px] leading-[1.45] text-bolu-muted">
                      {step.text}
                    </div>
                  </div>
                </li>
              );
            })}
          </ul>
        </div>
      </div>
    </section>
  );
}
