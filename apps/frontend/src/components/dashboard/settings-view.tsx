"use client";

/**
 * Pengaturan: how the team works for you.
 *
 * Everything on this screen is saved through the API as it changes — the profile
 * in one request, the approval policies one kind at a time — and every value is
 * one the gate actually reads: the window decides when an approved action may
 * leave, the language decides which language a Bolu replies in, and the policy
 * decides which kinds may go out without asking.
 *
 * The notification switches have nowhere to go: the backend has no notification
 * preferences, so they are absent rather than drawn as switches that forget.
 */

import { useCallback, useEffect, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import {
  currentWorkspace,
  setApprovalPolicy,
  updateWorkspace,
  type ApprovalPolicy,
  type Workspace,
} from "@/lib/api";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-[26px] border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";

/** The zones the app offers, with the label the design uses. */
const TIMEZONES = [
  { value: "Asia/Jakarta", label: "WIB (GMT+7)" },
  { value: "Asia/Makassar", label: "WITA (GMT+8)" },
  { value: "Asia/Jayapura", label: "WIT (GMT+9)" },
];

const LANGUAGES = [
  { value: "id", label: "Bahasa Indonesia" },
  { value: "en", label: "English" },
  { value: "both", label: "Ikuti bahasa pelanggan" },
];

/** The week, in the design's order. ISO weekday numbers. */
const WORK_DAYS = [
  { day: 1, label: "Sen" },
  { day: 2, label: "Sel" },
  { day: 3, label: "Rab" },
  { day: 4, label: "Kam" },
  { day: 5, label: "Jum" },
  { day: 6, label: "Sab" },
  { day: 7, label: "Min" },
];

/**
 * The words for each action kind.
 *
 * A kind is the name of the tool that reaches outside the workspace, so the label
 * is written for the ones this deployment ships and anything else is shown by its
 * own name rather than hidden: an unknown kind still has to be decidable.
 */
const ACTION_LABEL: Record<string, { title: string; desc: string }> = {
  "whatsapp.send_message": { title: "Balasan WhatsApp", desc: "Mengirim pesan ke pelanggan" },
  "gmail.send": { title: "Email keluar", desc: "Mengirim email ke pelanggan atau rekanan" },
  "jira.create_issue": { title: "Tiket Jira", desc: "Membuat tiket di proyek Jira" },
  "github.create_comment": { title: "Komentar GitHub", desc: "Menulis komentar di pull request atau issue" },
  "tokopedia.update_order": { title: "Pesanan Tokopedia", desc: "Mengubah status pesanan di Tokopedia" },
};

/** describeKind falls back to the tool's own name in a readable shape. */
function describeKind(kind: string): { title: string; desc: string } {
  const known = ACTION_LABEL[kind];
  if (known) return known;
  const [app, ...rest] = kind.split(".");
  const verb = rest.join(" ").replace(/_/g, " ");
  return {
    title: `${app} · ${verb || kind}`,
    desc: "Aksi ini keluar dari workspace",
  };
}

/** The tone the Bolu answers in, which the design offers as three chips. */
const TONES = [
  { value: "santai", label: "Santai", sample: "Halo Kak! Pesananmu sudah dikirim ya, resinya [NO RESI]." },
  { value: "ramah", label: "Ramah", sample: "Halo Kak, pesananmu sudah dikirim hari ini dengan nomor resi [NO RESI]. Terima kasih sudah berbelanja!" },
  { value: "formal", label: "Formal", sample: "Yth. Pelanggan, pesanan Anda telah kami kirimkan. Nomor resi: [NO RESI]." },
];

/** SettingsView is the settings screen. */
export function SettingsView() {
  const store = useWorkspace();
  const { workspace: loaded, agents, policies: storePolicies, reloadPolicies, workspaceID } = store;

  // The field a person is editing is held here and thrown away on a refusal; every
  // other field keeps reading the store. Mirroring the whole profile into local
  // state would mean a save in another tab left this screen showing the old value.
  const [edit, setEdit] = useState<Partial<Workspace> | null>(null);
  const [fetched, setFetched] = useState<Workspace | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tone, setTone] = useState(TONES[1].value);

  const profile = loaded ?? fetched;

  // The screen is reachable straight from a URL and the store may still be reading,
  // so the profile is fetched once when nothing has arrived yet.
  useEffect(() => {
    if (loaded || fetched || !workspaceID) return;
    void (async () => {
      const result = await currentWorkspace(workspaceID);
      if (result.ok) setFetched(result.data);
    })();
  }, [loaded, fetched, workspaceID]);

  /** shown is what the form displays: the field being edited, over the stored value. */
  const shown: Partial<Workspace> = { ...profile, ...edit };
  const policies: ApprovalPolicy[] = storePolicies;

  const save = useCallback(
    async (next: Partial<Workspace>) => {
      if (!workspaceID || !profile) return;
      // The field is shown immediately, then written whole: the endpoint replaces
      // the profile, so every other field travels with it.
      const merged: Workspace = { ...profile, ...edit, ...next };
      setEdit(merged);
      setBusy(true);
      const result = await updateWorkspace(workspaceID, {
        name: merged.name,
        business_field: merged.business_field,
        timezone: merged.timezone,
        language: merged.language,
        work_hours_start: merged.work_hours_start,
        work_hours_end: merged.work_hours_end,
        work_days: merged.work_days,
      });
      setBusy(false);
      if (!result.ok) {
        // The edit is dropped rather than kept: a screen that shows a value the
        // server refused is lying about what the approval gate will read.
        setEdit(null);
        setError(result.message);
        return;
      }
      setError(null);
      setEdit(null);
      setNotice("Tersimpan.");
      await store.reload();
    },
    [workspaceID, profile, edit, store],
  );

  const togglePolicy = useCallback(
    async (policy: ApprovalPolicy) => {
      if (!workspaceID) return;
      const mode = policy.mode === "auto" ? "always_draft" : "auto";
      const result = await setApprovalPolicy(workspaceID, {
        action_kind: policy.action_kind,
        mode,
        max_amount: policy.max_amount,
      });
      if (!result.ok) {
        setError(result.message);
        return;
      }
      setError(null);
      await reloadPolicies();
      setNotice("Kebijakan tersimpan.");
    },
    [workspaceID, reloadPolicies],
  );

  const days = shown.work_days ?? [];

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div>
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Pengaturan
        </h1>
        <div className="text-bolu-muted">
          Atur cara tim Bolu bekerja untukmu.{" "}
          {busy ? "Menyimpan…" : notice ? notice : "Perubahan langsung tersimpan."}
        </div>
      </div>

      {error ? (
        <p role="alert" className="rounded-2xl border border-bolu-belum/30 bg-bolu-belum/5 px-4 py-3 text-[15px] text-bolu-belum">
          {error}
        </p>
      ) : null}

      <section className={`${CARD} flex flex-col gap-4 p-[22px]`}>
        <h2 className={HEADING}>Ruang kerja</h2>
        <div className="flex flex-wrap gap-3.5">
          <label className="flex min-w-[220px] flex-1 flex-col gap-1.5 text-[14px] font-semibold">
            Nama ruang kerja
            <input
              type="text"
              value={shown.name ?? ""}
              onChange={(event) => {
                const value = event.target.value;
                setEdit((previous) => ({ ...previous, name: value }));
              }}
              onBlur={() => void save({})}
              className="min-h-11 rounded-xl border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent"
            />
          </label>
          <label className="flex min-w-[180px] flex-[0_1_240px] flex-col gap-1.5 text-[14px] font-semibold">
            Zona waktu
            <select
              value={shown.timezone ?? TIMEZONES[0].value}
              onChange={(event) => void save({ timezone: event.target.value })}
              className="min-h-11 cursor-pointer rounded-xl border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent"
            >
              {TIMEZONES.map((zone) => (
                <option key={zone.value} value={zone.value}>
                  {zone.label}
                </option>
              ))}
            </select>
          </label>
          <label className="flex min-w-[180px] flex-[0_1_240px] flex-col gap-1.5 text-[14px] font-semibold">
            Bahasa balasan
            <select
              value={shown.language ?? "id"}
              onChange={(event) => void save({ language: event.target.value })}
              className="min-h-11 cursor-pointer rounded-xl border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent"
            >
              {LANGUAGES.map((language) => (
                <option key={language.value} value={language.value}>
                  {language.label}
                </option>
              ))}
            </select>
          </label>
        </div>
      </section>

      <section className={`${CARD} flex flex-col gap-3.5 p-[22px]`}>
        <h2 className={HEADING}>Jam kerja tim</h2>
        <p className="text-[15px] text-bolu-muted">
          Di luar jam ini Bolu tidak menghubungi pelanggan. Pekerjaan dalam seperti rekap dan arsip
          tetap jalan.
        </p>
        <div className="flex flex-wrap items-end gap-3.5">
          <label className="flex flex-col gap-1.5 text-[14px] font-semibold">
            Mulai
            <input
              type="time"
              value={shown.work_hours_start || "08:00"}
              onChange={(event) => void save({ work_hours_start: event.target.value })}
              className="min-h-11 rounded-xl border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent"
            />
          </label>
          <label className="flex flex-col gap-1.5 text-[14px] font-semibold">
            Selesai
            <input
              type="time"
              value={shown.work_hours_end || "21:00"}
              onChange={(event) => void save({ work_hours_end: event.target.value })}
              className="min-h-11 rounded-xl border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent"
            />
          </label>
          <div className="flex flex-wrap gap-1.5">
            {WORK_DAYS.map((item) => {
              const on = days.includes(item.day);
              return (
                <button
                  key={item.day}
                  type="button"
                  aria-pressed={on}
                  onClick={() => {
                    const next = on
                      ? days.filter((day) => day !== item.day)
                      : [...days, item.day].sort((a, b) => a - b);
                    void save({ work_days: next });
                  }}
                  className={`min-h-11 min-w-11 cursor-pointer rounded-full border text-[14px] font-bold ${
                    on
                      ? "border-bolu-ink bg-bolu-ink text-white"
                      : "border-bolu-border bg-white text-bolu-body"
                  }`}
                >
                  {item.label}
                </button>
              );
            })}
          </div>
        </div>
        <p className="text-[13px] text-bolu-muted">
          Draf yang disetujui di luar jam ini ditahan sampai jendela berikutnya dibuka.
        </p>
      </section>

      <section className={`${CARD} flex flex-col gap-1 p-[22px_22px_10px]`}>
        <h2 className={HEADING}>Persetujuan</h2>
        <p className="mb-2 text-[15px] text-bolu-muted">
          Pilih mana yang boleh langsung dikirim Bolu tanpa menunggu kamu. Yang dimatikan selalu
          masuk sebagai draf.
        </p>
        {policies.length === 0 ? (
          <p className="py-3 text-[14px] text-bolu-muted">Belum ada kebijakan yang terbaca.</p>
        ) : (
          policies.map((policy) => {
            const copy = describeKind(policy.action_kind);
            const on = policy.mode === "auto";
            return (
              <div
                key={policy.action_kind}
                className="flex items-center gap-3.5 border-t border-bolu-line py-3"
              >
                <div className="flex-1 min-w-0">
                  <div className="font-semibold">{copy.title}</div>
                  <div className="text-[14px] text-bolu-muted">{copy.desc}</div>
                </div>
                <button
                  type="button"
                  role="switch"
                  aria-checked={on}
                  aria-label={copy.title}
                  onClick={() => void togglePolicy(policy)}
                  className={`h-[30px] w-[52px] flex-none cursor-pointer rounded-full border-0 p-[3px] transition-colors ${
                    on ? "bg-bolu-ink" : "bg-bolu-chip"
                  }`}
                >
                  <span
                    className={`block size-6 rounded-full bg-white transition-transform ${
                      on ? "translate-x-[22px]" : ""
                    }`}
                  />
                </button>
              </div>
            );
          })
        )}
      </section>

      <section className={`${CARD} flex flex-col gap-3.5 p-[22px]`}>
        <h2 className={HEADING}>Gaya bahasa</h2>
        <div className="flex flex-wrap gap-2">
          {TONES.map((item) => (
            <button
              key={item.value}
              type="button"
              aria-pressed={item.value === tone}
              onClick={() => setTone(item.value)}
              className={`min-h-11 cursor-pointer rounded-full border px-4 text-[15px] font-semibold ${
                item.value === tone
                  ? "border-bolu-ink bg-bolu-ink text-white"
                  : "border-bolu-border bg-white text-bolu-ink"
              }`}
            >
              {item.label}
            </button>
          ))}
        </div>
        <div className="flex items-start gap-2.5">
          <div className="flex-none">
            {agents[0] ? (
              <Bolu
                member={crewFor(agents[0])}
                expression={expressionOf(stateOf(agents[0]))}
                color={agents[0].color || undefined}
                size={38}
              />
            ) : null}
          </div>
          <p className="rounded-[6px_20px_20px_20px] bg-bolu-panel px-3.5 py-2.5 text-[15px] text-bolu-ink">
            {TONES.find((item) => item.value === tone)?.sample}
          </p>
        </div>
        <p className="text-[13px] text-bolu-muted">
          Gaya ini disimpan pada tiap Bolu, di halaman Tim: tiap anggota bisa punya gayanya sendiri.
        </p>
      </section>

      <section className={`${CARD} flex flex-wrap items-center justify-between gap-3.5 p-[22px]`}>
        <div>
          <h2 className={HEADING}>Aplikasi terhubung</h2>
          <div className="text-bolu-muted">
            {store.hub.connections.length === 0
              ? "Belum ada aplikasi terhubung"
              : `${store.hub.connections.length} aplikasi terhubung`}
          </div>
        </div>
        <a
          href="/dashboard/integrations"
          className="flex min-h-11 items-center rounded-full bg-bolu-ink px-5 font-bold text-white no-underline hover:text-white"
        >
          Kelola integrasi
        </a>
      </section>

      <section className={`${CARD} flex flex-wrap items-center justify-between gap-3.5 p-[22px]`}>
        <div>
          <h2 className={HEADING}>Anggota ruang kerja</h2>
          <div className="text-bolu-muted">
            {store.members.length} orang · {agents.length} Bolu
          </div>
        </div>
        <a
          href="/settings/team"
          className="flex min-h-11 items-center rounded-full border-2 border-bolu-ink px-5 font-bold text-bolu-ink no-underline hover:text-bolu-ink"
        >
          Kelola tim
        </a>
      </section>

      <section className={`${CARD} flex flex-wrap items-center justify-between gap-3.5 p-[22px]`}>
        <div>
          <h2 className={HEADING}>Paket dan tagihan</h2>
          <div className="text-bolu-muted">
            Belum ada paket berjalan. Penagihan transfer bank dikerjakan setelah billing dibangun.
          </div>
        </div>
        <span className="flex min-h-11 items-center rounded-full border border-bolu-border px-5 font-bold text-bolu-muted">
          Belum tersedia
        </span>
      </section>
    </div>
  );
}
