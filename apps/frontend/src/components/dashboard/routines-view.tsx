"use client";

/**
 * Rutinitas: the work the team runs on its own.
 *
 * Everything here reads the routine module's own endpoints, so what a person sees
 * is what the scheduler will do: the cadence in words, the next times in the
 * workspace's own zone, and the run history with the one-line summary each run
 * produced.
 *
 * Two things are deliberate:
 * - a routine fires in the workspace's timezone rather than one of its own, so the
 *   zone is shown beside the times and can be changed here by an owner — changing
 *   it moves every future run of every routine;
 * - the history is the run's own summary rather than the task's raw steps, because
 *   "4 file dirapikan" is the answer to "what did this do" and a step count is not.
 */

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { NoWorkspaceNotice } from "@/components/dashboard/no-workspace";
import { Bolu } from "@/components/bolu/bolu";
import {
  type Routine,
  type RoutineInput,
  type RoutineRun,
  type Workspace,
  createRoutine,
  deleteRoutine,
  ensureActiveWorkspace,
  listRoutineRuns,
  listRoutines,
  pauseRoutine,
  previewRoutine,
  resumeRoutine,
  updateRoutine,
  updateWorkspace,
} from "@/lib/api";
import { ALL_BOTS } from "@/lib/crew";
import { TIMEZONES, crewFor, expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";
const FIELD = "min-h-11 rounded-md border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent";
const LABEL = "flex flex-col gap-1.5 text-[14px] font-semibold";

/** The cadences the form offers, in the words the design uses. */
const KINDS: { value: Routine["kind"]; label: string; note: string }[] = [
  { value: "daily", label: "Setiap hari", note: "sekali sehari pada jam tetap" },
  { value: "hourly_range", label: "Sepanjang hari", note: "berulang tiap jam dalam rentang" },
  { value: "weekly", label: "Setiap minggu", note: "sekali seminggu pada hari dan jam tetap" },
  { value: "trigger", label: "Saat ada kejadian", note: "dijalankan saat event integrasi masuk" },
];

/** ISO weekdays, matching what the API stores and the form shows. */
const WEEKDAYS = [
  { value: 1, label: "Senin" },
  { value: 2, label: "Selasa" },
  { value: 3, label: "Rabu" },
  { value: 4, label: "Kamis" },
  { value: 5, label: "Jumat" },
  { value: 6, label: "Sabtu" },
  { value: 7, label: "Minggu" },
];

/** STATUS_LABEL is what a run's outcome means, in the user's words. */
const STATUS_LABEL: Record<string, string> = {
  running: "Berjalan",
  succeeded: "Selesai",
  failed: "Gagal",
  skipped: "Dilewati",
};

/** STATUS_STYLE is the chip each outcome wears. */
const STATUS_STYLE: Record<string, { bg: string; fg: string }> = {
  running: { bg: "#E2602B1F", fg: "#E2602B" },
  succeeded: { bg: "#1E7A431F", fg: "#1E7A43" },
  failed: { bg: "#C5362B1F", fg: "#C5362B" },
  skipped: { bg: "#8B88A01F", fg: "#5E5B70" },
};

const HOURS = Array.from({ length: 24 }, (_, hour) => hour);
const MINUTES = [0, 15, 30, 45];

/** weekdayLabel names one ISO weekday in the words the form uses. */
function weekdayLabel(day: number): string {
  return WEEKDAYS.find((weekday) => weekday.value === day)?.label ?? "—";
}

/**
 * partsOf renders one instant as the wall clock of a zone.
 *
 * It is not the same thing as reading the browser's clock, and the difference is
 * the whole point: a cadence reads "setiap hari 06:00" because the schedule is
 * registered in the workspace's zone, so the next run must be shown in that zone
 * too. Rendering it in the viewer's zone would print "05.00" beside "06:00" for
 * anyone in a different zone — two numbers that disagree about the same run.
 *
 * The zone is a name the API gave us, so the parts are read with `formatToParts`;
 * a date's own `getHours()` is always the viewer's.
 */
function partsOf(iso: string, timezone: string): { hour: number; minute: number; day: number; month: number; weekday: number } | null {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return null;

  const format = new Intl.DateTimeFormat("en-GB", {
    timeZone: timezone,
    hour: "2-digit",
    minute: "2-digit",
    day: "2-digit",
    month: "2-digit",
    weekday: "short",
    hour12: false,
  });

  const parts: Record<string, string> = {};
  for (const part of format.formatToParts(at)) {
    parts[part.type] = part.value;
  }

  const weekday = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"].indexOf(parts.weekday ?? "");

  return {
    hour: Number(parts.hour),
    minute: Number(parts.minute),
    day: Number(parts.day),
    month: Number(parts.month),
    weekday: weekday < 0 ? 1 : weekday + 1,
  };
}

/** dateKey is a calendar day in a zone, for comparing two instants by their date. */
function dateKey(at: Date, timezone: string): string {
  return new Intl.DateTimeFormat("en-CA", { timeZone: timezone, dateStyle: "short" }).format(at);
}

/** clockIn is the wall-clock time of an instant in one zone. */
function clockIn(iso: string, timezone: string): string {
  const parts = partsOf(iso, timezone);
  if (!parts) return "";
  return `${String(parts.hour).padStart(2, "0")}.${String(parts.minute).padStart(2, "0")}`;
}

/** formatNextRun says when a routine runs next, in the workspace's own clock. */
function formatNextRun(iso: string, now: Date, timezone: string): string {
  const parts = partsOf(iso, timezone);
  if (!parts) return "—";

  // "hari ini" and "besok" are calendar days, so they are compared as calendar
  // days in the same zone: comparing raw millisecond differences would call 23:00
  // tomorrow "today" for a viewer whose clock is another zone away.
  const at = new Date(iso);
  const today = dateKey(now, timezone);
  const tomorrow = dateKey(new Date(now.getTime() + 24 * 60 * 60 * 1000), timezone);
  const day = dateKey(at, timezone);
  const time = clockIn(iso, timezone);

  if (day === today) return `hari ini ${time}`;
  if (day === tomorrow) return `besok ${time}`;
  return `${weekdayLabel(parts.weekday)}, ${parts.day}/${parts.month} ${time}`;
}

/** wallClock reads an "HH:MM" setting as the two numbers the form uses. */
function wallClock(value: string | undefined, fallback: number): { hour: number; minute: number } {
  const [hour, minute] = (value ?? "").split(":");
  const parsed = Number(hour);
  const parsedMinute = Number(minute);
  if (!Number.isFinite(parsed) || hour === undefined || hour === "") {
    return { hour: fallback, minute: 0 };
  }
  return {
    hour: parsed,
    minute: Number.isFinite(parsedMinute) ? parsedMinute : 0,
  };
}

/**
 * emptyForm is the shape the create form starts from.
 *
 * Its times are the workspace's working hours rather than a number invented here:
 * the window is already a setting somebody chose, and a routine that starts at the
 * opening of the business's day is the common case. The fallback is only for a
 * workspace whose window is unreadable, which the settings screen does not allow.
 */
function emptyForm(agentId: string, workspace?: Workspace | null): RoutineInput {
  const start = wallClock(workspace?.work_hours_start, 8);
  const end = wallClock(workspace?.work_hours_end, 21);

  return {
    agent_id: agentId,
    name: "",
    kind: "daily",
    minute: start.minute,
    hour_start: start.hour,
    hour_end: end.hour >= start.hour ? end.hour : 23,
    day_of_week: 1,
    instruction: "",
    enabled: true,
  };
}

/** formFor turns a stored routine back into the form's own shape. */
function formFor(routine: Routine): RoutineInput {
  return {
    agent_id: routine.agent_id,
    name: routine.name,
    kind: routine.kind,
    minute: routine.minute,
    hour_start: routine.hour_start,
    hour_end: routine.hour_end,
    day_of_week: routine.day_of_week ?? 1,
    trigger_source: routine.trigger_source,
    instruction: routine.instruction,
    enabled: routine.enabled,
  };
}

/** RoutinesView is the routines screen. */
export function RoutinesView() {
  const store = useWorkspace();
  const { agents, workspace, members, user, hub } = store;

  const [workspaceID, setWorkspaceID] = useState<string | null>(null);
  const [routines, setRoutines] = useState<Routine[]>([]);
  const [runs, setRuns] = useState<RoutineRun[]>([]);
  const [draft, setDraft] = useState<RoutineInput | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [preview, setPreview] = useState<string[]>([]);
  const [filterAgent, setFilterAgent] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [noWorkspace, setNoWorkspace] = useState(false);

  // One clock per render, so every row in the history is read against the same
  // "now" rather than each one taking its own reading.
  const now = new Date();

  const load = useCallback(async () => {
    const active = await ensureActiveWorkspace();
    setWorkspaceID(active);
    if (!active) {
      // The session is fine; the account just has nothing to work in yet. That is
      // not an error line, it is the screen that offers to make one.
      setNoWorkspace(true);
      setLoading(false);
      return;
    }

    const [listed, history] = await Promise.all([listRoutines(active), listRoutineRuns(active, { limit: 50 })]);
    if (listed.ok) {
      setRoutines(listed.data);
    } else {
      setError(listed.message);
    }
    if (history.ok) {
      setRuns(history.data);
    } else {
      setError(history.message);
    }
    setLoading(false);
  }, []);

  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  // The form opens on the first Bolu, because a routine belongs to one and a form
  // that starts with nothing chosen cannot show what it will save. It is derived
  // rather than seeded into state, so a Bolu that arrives after the first render
  // becomes the default without an effect that writes state.
  const form: RoutineInput | null = agents.length === 0 ? null : (draft ?? emptyForm(agents[0].id, workspace));
  const setForm = (next: RoutineInput) => setDraft(next);

  // A preview of the times the cadence being filled in would fire at. It asks the
  // server rather than computing in the browser, so the times a person reads are
  // the same ones the engine registers.
  const kind = form?.kind;
  const minute = form?.minute;
  const hourStart = form?.hour_start;
  const hourEnd = form?.hour_end;
  const dayOfWeek = form?.day_of_week;

  useEffect(() => {
    if (!workspaceID || !kind || kind === "trigger") return;

    let live = true;
    void (async () => {
      const result = await previewRoutine(workspaceID, {
        kind,
        minute,
        hour_start: hourStart,
        hour_end: hourEnd,
        day_of_week: dayOfWeek,
      });
      if (!live) return;
      setPreview(result.ok ? result.data.next_runs : []);
    })();

    return () => {
      live = false;
    };
  }, [workspaceID, kind, minute, hourStart, hourEnd, dayOfWeek]);

  // A trigger routine has no times to preview, whatever the last cadence fetched.
  const shownPreview = kind === "trigger" ? [] : preview;

  const zone = workspace?.timezone ?? "Asia/Jakarta";
  const isOwner = members.some((member) => member.user_id === user?.id && member.role === "owner");

  /**
   * saveZone changes the workspace's zone, which is what moves every future run:
   * a routine has no zone of its own, so the times it shows are the workspace's.
   */
  const saveZone = useCallback(
    async (timezone: string) => {
      if (!workspaceID || !workspace) return;
      setBusy(true);
      const result = await updateWorkspace(workspaceID, {
        name: workspace.name,
        business_field: workspace.business_field,
        timezone,
        language: workspace.language,
        work_hours_start: workspace.work_hours_start,
        work_hours_end: workspace.work_hours_end,
        work_days: workspace.work_days,
      });
      setBusy(false);
      if (!result.ok) {
        setError(result.message);
        return;
      }

      setError(null);
      setNotice("Zona waktu tersimpan. Jadwal berikutnya dihitung ulang.");
      await Promise.all([store.reload(), load()]);
    },
    [workspaceID, workspace, store, load],
  );

  const save = useCallback(async () => {
    if (!workspaceID || !form) return;
    if (!form.name.trim() || !form.instruction.trim()) {
      setError("Nama dan instruksi harus diisi.");
      return;
    }

    setBusy(true);
    const result = editing
      ? await updateRoutine(workspaceID, editing, form)
      : await createRoutine(workspaceID, form);
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setError(null);
    setNotice(editing ? "Jadwal diperbarui." : "Rutinitas dibuat dan dijadwalkan.");
    setEditing(null);
    setForm(emptyForm(form.agent_id, workspace));
    await load();
  }, [workspaceID, form, editing, load, workspace]);

  const toggle = useCallback(
    async (routine: Routine) => {
      if (!workspaceID) return;
      const result = routine.enabled
        ? await pauseRoutine(workspaceID, routine.id)
        : await resumeRoutine(workspaceID, routine.id);
      if (!result.ok) {
        setError(result.message);
        return;
      }

      setError(null);
      setNotice(routine.enabled ? "Rutinitas dijeda. Riwayatnya tetap ada." : "Rutinitas dilanjutkan.");
      await load();
    },
    [workspaceID, load],
  );

  const remove = useCallback(
    async (routine: Routine) => {
      if (!workspaceID) return;
      setBusy(true);
      const result = await deleteRoutine(workspaceID, routine.id);
      setBusy(false);
      if (!result.ok) {
        setError(result.message);
        return;
      }

      setError(null);
      setNotice("Rutinitas dihapus. Riwayatnya tetap tersimpan.");
      if (editing === routine.id) {
        setEditing(null);
        setForm(emptyForm(routine.agent_id, workspace));
      }
      await load();
    },
    [workspaceID, editing, load, workspace],
  );

  // The event a trigger routine watches, taken from what this deployment's
  // adapters actually deliver rather than from a hard-coded list.
  const triggerEvents = useMemo(() => {
    const events = hub.adapters.flatMap((adapter) =>
      (adapter.triggers ?? []).map((trigger) => ({
        event: trigger.event,
        label: `${adapter.app} · ${trigger.description || trigger.event}`,
      })),
    );
    return events;
  }, [hub.adapters]);

  const shown = filterAgent ? routines.filter((routine) => routine.agent_id === filterAgent) : routines;
  const shownRuns = filterAgent
    ? runs.filter((run) => routineAgent(run, routines) === filterAgent)
    : runs;
  const byAgent = useMemo(() => {
    const map = new Map<string, Routine[]>();
    for (const routine of shown) {
      map.set(routine.agent_id, [...(map.get(routine.agent_id) ?? []), routine]);
    }
    return map;
  }, [shown]);

  const scheduled = routines.filter((routine) => routine.scheduled).length;
  const running = runs.filter((run) => run.status === "running").length;

  if (noWorkspace) {
    return (
      <div className="p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
        <NoWorkspaceNotice />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div>
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Rutinitas
        </h1>
        <div className="text-bolu-muted">
          Pekerjaan yang dijalankan tim secara otomatis.{" "}
          {loading
            ? "Memuat…"
            : routines.length === 0
              ? "Belum ada rutinitas."
              : `${routines.length} rutinitas, ${scheduled} terjadwal, ${running} sedang jalan.`}
        </div>
      </div>

      {error ? <Alert tone="error">{error}</Alert> : null}
      {notice && !error ? <Alert tone="success">{notice}</Alert> : null}

      {/* The form. It is always open rather than behind a button, because a screen
          whose purpose is a list of cadences should be able to make one. */}
      <section className={`${CARD} flex flex-col gap-4 p-[22px]`}>
        <div className="flex flex-wrap items-baseline justify-between gap-2.5">
          <h2 className={HEADING}>{editing ? "Ubah rutinitas" : "Rutinitas baru"}</h2>
          <span className="text-[14px] text-bolu-muted">
            {editing ? "Perubahan langsung menggeser jadwal berikutnya." : "Bolu mengerjakannya sendiri, tanpa disuruh."}
          </span>
        </div>

        {form ? (
          <div className="flex flex-col gap-3.5">
            <div className="flex flex-wrap gap-3.5">
              <label className={`${LABEL} min-w-[220px] flex-1`}>
                Nama rutinitas
                <input
                  type="text"
                  name="routine-name"
                  value={form.name}
                  placeholder="Cek dokumen baru"
                  onChange={(event) => setForm({ ...form, name: event.target.value })}
                  className={FIELD}
                />
              </label>
              <label className={`${LABEL} min-w-[180px] flex-[0_1_240px]`}>
                Dikerjakan oleh
                <select
                  name="routine-agent"
                  value={form.agent_id}
                  onChange={(event) => setForm({ ...form, agent_id: event.target.value })}
                  className={`${FIELD} cursor-pointer`}
                >
                  {agents.map((agent) => (
                    <option key={agent.id} value={agent.id}>
                      {agent.name}
                    </option>
                  ))}
                </select>
              </label>
              <label className={`${LABEL} min-w-[180px] flex-[0_1_240px]`}>
                Jenis jadwal
                <select
                  name="routine-kind"
                  value={form.kind}
                  onChange={(event) => setForm({ ...form, kind: event.target.value as Routine["kind"] })}
                  className={`${FIELD} cursor-pointer`}
                >
                  {KINDS.map((option) => (
                    <option key={option.value} value={option.value}>
                      {option.label}
                    </option>
                  ))}
                </select>
              </label>
              <label className={`${LABEL} min-w-[180px] flex-[0_1_240px]`}>
                Zona waktu
                <select
                  name="routine-timezone"
                  value={zone}
                  disabled={!isOwner || busy}
                  onChange={(event) => void saveZone(event.target.value)}
                  className={`${FIELD} cursor-pointer disabled:cursor-not-allowed disabled:bg-bolu-panel/60`}
                >
                  {TIMEZONES.map((option) => (
                    <option key={option.value} value={option.value}>
                      {option.label}
                    </option>
                  ))}
                </select>
                <span className="text-[13px] font-normal text-bolu-muted">
                  {isOwner
                    ? "Mengubahnya menggeser jadwal semua rutinitas dan jam kerja."
                    : "Hanya pemilik ruang kerja yang bisa mengubahnya."}
                </span>
              </label>
            </div>

            {/* The time fields each cadence actually uses. A trigger has no clock,
                so it asks for the event instead. */}
            {form.kind === "trigger" ? (
              <div className="flex flex-wrap gap-3.5">
                <label className={`${LABEL} min-w-[240px] flex-1`}>
                  Kejadian yang ditunggu
                  <input
                    type="text"
                    name="routine-trigger"
                    list="routine-trigger-events"
                    value={form.trigger_source ?? ""}
                    placeholder="gmail.email_received"
                    onChange={(event) => setForm({ ...form, trigger_source: event.target.value })}
                    className={FIELD}
                  />
                  <datalist id="routine-trigger-events">
                    {triggerEvents.map((option) => (
                      <option key={option.event} value={option.event}>
                        {option.label}
                      </option>
                    ))}
                  </datalist>
                  <span className="text-[13px] font-normal text-bolu-muted">
                    {triggerEvents.length === 0
                      ? "Belum ada integrasi yang mengirim kejadian. Sambungkan aplikasi dulu di halaman Integrasi."
                      : "Nama kejadian dari integrasi yang tersambung."}
                  </span>
                </label>
              </div>
            ) : (
              <div className="flex flex-wrap gap-3.5">
                {form.kind === "weekly" ? (
                  <label className={`${LABEL} min-w-[140px] flex-[0_1_160px]`}>
                    Hari
                    <select
                      name="routine-weekday"
                      value={form.day_of_week ?? 1}
                      onChange={(event) => setForm({ ...form, day_of_week: Number(event.target.value) })}
                      className={`${FIELD} cursor-pointer`}
                    >
                      {WEEKDAYS.map((weekday) => (
                        <option key={weekday.value} value={weekday.value}>
                          {weekday.label}
                        </option>
                      ))}
                    </select>
                  </label>
                ) : null}

                <label className={`${LABEL} min-w-[110px] flex-[0_1_140px]`}>
                  {form.kind === "hourly_range" ? "Mulai jam" : "Jam"}
                  <select
                    name="routine-hour-start"
                    value={form.hour_start ?? 6}
                    onChange={(event) => {
                      const hour = Number(event.target.value);
                      // A window keeps its start below its end: the form must not be
                      // able to ask for a range the server will refuse.
                      setForm({
                        ...form,
                        hour_start: hour,
                        hour_end: form.kind === "hourly_range" ? Math.max(hour, form.hour_end ?? hour) : hour,
                      });
                    }}
                    className={`${FIELD} cursor-pointer`}
                  >
                    {HOURS.map((hour) => (
                      <option key={hour} value={hour}>
                        {String(hour).padStart(2, "0")}.00
                      </option>
                    ))}
                  </select>
                </label>

                {form.kind === "hourly_range" ? (
                  <label className={`${LABEL} min-w-[110px] flex-[0_1_140px]`}>
                    Sampai jam
                    <select
                      name="routine-hour-end"
                      value={form.hour_end ?? 21}
                      onChange={(event) => setForm({ ...form, hour_end: Number(event.target.value) })}
                      className={`${FIELD} cursor-pointer`}
                    >
                      {HOURS.filter((hour) => hour >= (form.hour_start ?? 0)).map((hour) => (
                        <option key={hour} value={hour}>
                          {String(hour).padStart(2, "0")}.00
                        </option>
                      ))}
                    </select>
                  </label>
                ) : null}

                <label className={`${LABEL} min-w-[110px] flex-[0_1_140px]`}>
                  Menit
                  <select
                    name="routine-minute"
                    value={form.minute ?? 0}
                    onChange={(event) => setForm({ ...form, minute: Number(event.target.value) })}
                    className={`${FIELD} cursor-pointer`}
                  >
                    {MINUTES.map((minute) => (
                      <option key={minute} value={minute}>
                        {String(minute).padStart(2, "0")}
                      </option>
                    ))}
                  </select>
                </label>
              </div>
            )}

            <label className={LABEL}>
              Instruksi untuk Bolu
              <textarea
                name="routine-instruction"
                value={form.instruction}
                rows={3}
                placeholder="Buka dokumen baru di folder Pesanan, catat pesanannya, lalu ringkas."
                onChange={(event) => setForm({ ...form, instruction: event.target.value })}
                className="rounded-md border border-bolu-border bg-white p-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent"
              />
            </label>

            {/* What the cadence will actually do, from the server that registers it. */}
            <div className="rounded-md bg-bolu-panel/60 p-[14px_16px]">
              <div className="text-[14px] font-semibold">
                {KINDS.find((option) => option.value === form.kind)?.label} ·{" "}
                {form.kind === "trigger"
                  ? form.trigger_source || "kejadian belum dipilih"
                  : shownPreview.length === 0
                    ? "menghitung waktu berikutnya…"
                    : `berikutnya ${formatNextRun(shownPreview[0], now, zone)}`}
              </div>
              {shownPreview.length > 1 ? (
                <div className="mt-1 text-[13px] text-bolu-muted">
                  {shownPreview.slice(1, 4).map((iso) => formatNextRun(iso, now, zone)).join(" · ")}
                </div>
              ) : null}
              <div className="mt-1 text-[13px] text-bolu-muted">
                Dihitung di zona waktu {zone}. Matikan untuk menyimpan tanpa menjalankannya.
              </div>
            </div>

            <div className="flex flex-wrap items-center gap-2.5">
              <label className="flex items-center gap-2 text-[15px]">
                <input
                  type="checkbox"
                  checked={form.enabled ?? true}
                  onChange={(event) => setForm({ ...form, enabled: event.target.checked })}
                  className="size-4"
                />
                Aktif
              </label>
              <button
                type="button"
                onClick={() => void save()}
                disabled={busy}
                className="min-h-11 rounded-full bg-bolu-ink px-5 text-[14px] font-bold text-white disabled:opacity-60"
              >
                {busy ? "Menyimpan…" : editing ? "Simpan perubahan" : "Simpan rutinitas"}
              </button>
              {editing ? (
                <button
                  type="button"
                  onClick={() => {
                    setEditing(null);
                    setForm(emptyForm(agents[0]?.id ?? "", workspace));
                  }}
                  className="min-h-11 rounded-full border border-bolu-border px-5 text-[14px] font-bold text-bolu-ink"
                >
                  Batal
                </button>
              ) : null}
            </div>
          </div>
        ) : (
          <p className="text-[14px] text-bolu-muted">
            {agents.length === 0
              ? "Buat Bolu dulu di halaman Tim, lalu kembali ke sini."
              : "Memuat…"}
          </p>
        )}
      </section>

      {/* One card per Bolu, which is how the design lists them: a routine belongs
          to a Bolu, and what it does is read together with who does it. */}
      {byAgent.size === 0 ? (
        <section className={`${CARD} p-5`}>
          <p className="text-[14px] text-bolu-muted">
            {loading ? "Memuat rutinitas…" : "Belum ada rutinitas. Buat satu di atas."}
          </p>
        </section>
      ) : (
        [...byAgent.entries()].map(([agentId, list]) => {
          const agent = agents.find((item) => item.id === agentId) ?? null;
          return (
            <section key={agentId} className={`${CARD} flex flex-col gap-3 p-5`}>
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex items-center gap-3">
                  <Bolu
                    member={agent ? crewFor(agent) : ALL_BOTS[0]}
                    expression={expressionOf(agent ? stateOf(agent) : "idle")}
                    color={agent?.color || undefined}
                    size={40}
                  />
                  <div>
                    <div className={HEADING}>{agent?.name ?? "Bolu"}</div>
                    <div className="text-[13px] text-bolu-muted">
                      {list.length} rutinitas · {list.filter((routine) => routine.enabled).length} aktif
                    </div>
                  </div>
                </div>
                <button
                  type="button"
                  onClick={() => setFilterAgent(agentId === filterAgent ? "" : agentId)}
                  className="min-h-9 rounded-full border border-bolu-border px-4 text-[13px] font-semibold text-bolu-ink"
                >
                  {agentId === filterAgent ? "Tampilkan semua" : "Saring riwayat Bolu ini"}
                </button>
              </div>

              <ul className="flex flex-col">
                {list.map((routine) => (
                  <li
                    key={routine.id}
                    className="flex flex-wrap items-center gap-3 border-t border-bolu-line p-[12px_0]"
                  >
                    <div className="min-w-[200px] flex-1">
                      <div className="font-semibold">{routine.name}</div>
                      <div className="text-[13px] text-bolu-muted">{routine.instruction}</div>
                    </div>
                    <div className="min-w-[160px] flex-none text-[14px]">
                      <div className="font-semibold">{routine.schedule}</div>
                      <div className="text-[13px] text-bolu-muted">
                        {routine.enabled
                          ? routine.next_runs.length > 0
                            ? `berikutnya ${formatNextRun(routine.next_runs[0], now, zone)}`
                            : routine.kind === "trigger"
                              ? "menunggu kejadian"
                              : "jadwal belum terdaftar"
                          : routine.paused_reason || "dijeda"}
                      </div>
                    </div>
                    <span
                      className="flex-none rounded-full px-2.5 py-1 text-[12px] font-bold"
                      style={
                        routine.enabled
                          ? { backgroundColor: "#1E7A431F", color: "#1E7A43" }
                          : { backgroundColor: "#8B88A01F", color: "#5E5B70" }
                      }
                    >
                      {routine.enabled ? "Aktif" : "Dijeda"}
                    </span>
                    <div className="flex flex-none gap-2">
                      <button
                        type="button"
                        onClick={() => void toggle(routine)}
                        className="min-h-9 rounded-full border border-bolu-border px-4 text-[13px] font-semibold text-bolu-ink"
                      >
                        {routine.enabled ? "Jeda" : "Lanjutkan"}
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setEditing(routine.id);
                          setForm(formFor(routine));
                        }}
                        className="min-h-9 rounded-full border border-bolu-border px-4 text-[13px] font-semibold text-bolu-ink"
                      >
                        Ubah
                      </button>
                      <button
                        type="button"
                        onClick={() => void remove(routine)}
                        className="min-h-9 rounded-full border border-bolu-border px-4 text-[13px] font-semibold text-bolu-belum"
                      >
                        Hapus
                      </button>
                    </div>
                  </li>
                ))}
              </ul>
            </section>
          );
        })
      )}

      <section className={`${CARD} flex flex-col gap-3 p-5`}>
        <div className="flex flex-wrap items-center justify-between gap-2.5">
          <h2 className={HEADING}>Riwayat</h2>
          <div className="flex flex-wrap items-center gap-2.5">
            <span className="text-[14px] text-bolu-muted">{shownRuns.length} eksekusi</span>
            <select
              value={filterAgent}
              onChange={(event) => setFilterAgent(event.target.value)}
              className="min-h-9 cursor-pointer rounded-full border border-bolu-border bg-white px-3 text-[13px] font-semibold text-bolu-ink outline-none"
            >
              <option value="">Semua Bolu</option>
              {agents.map((agent) => (
                <option key={agent.id} value={agent.id}>
                  {agent.name}
                </option>
              ))}
            </select>
          </div>
        </div>

        {shownRuns.length === 0 ? (
          <p className="text-[14px] text-bolu-muted">
            {loading ? "Memuat riwayat…" : "Belum ada eksekusi. Riwayat terisi sendiri saat jadwalnya jalan."}
          </p>
        ) : (
          <ul className="flex flex-col">
            {shownRuns.slice(0, 20).map((run) => {
              const agentId = routineAgent(run, routines);
              const agent = agents.find((item) => item.id === agentId) ?? null;
              const style = STATUS_STYLE[run.status] ?? STATUS_STYLE.skipped;
              return (
                <li
                  key={run.run_id}
                  className="flex flex-wrap items-center gap-3 border-t border-bolu-line p-[10px_0]"
                >
                  <span className="w-[60px] flex-none text-[14px] font-semibold text-bolu-muted">
                    {clockIn(run.started_at, zone)}
                  </span>
                  <div className="flex-none">
                    <Bolu
                      member={agent ? crewFor(agent) : ALL_BOTS[0]}
                      expression={expressionOf(agent ? stateOf(agent) : "idle")}
                      color={agent?.color || undefined}
                      size={30}
                    />
                  </div>
                  <div className="min-w-[220px] flex-1">
                    <div className="truncate font-semibold">{run.routine_name}</div>
                    {/* A failed run carries the task's own refusal, which can be a
                        sentence rather than a line, so it is clamped: the history is
                        scanned by eye and a paragraph per row is not a history. The
                        whole text stays in the DOM's title for a hover. */}
                    <div
                      className="line-clamp-2 text-[13px] text-bolu-muted"
                      title={run.summary || undefined}
                    >
                      {run.summary || (run.status === "running" ? "sedang dikerjakan" : "tidak ada ringkasan")}
                    </div>
                  </div>
                  <span className="flex-none text-[13px] text-bolu-muted">
                    {relativeAge(run.started_at, now)}
                  </span>
                  <span
                    className="flex-none rounded-full px-2.5 py-1 text-[12px] font-bold"
                    style={{ backgroundColor: style.bg, color: style.fg }}
                  >
                    {STATUS_LABEL[run.status] ?? run.status}
                  </span>
                  {/* A routine's work is a task, and the place this product reads a
                      task is the thread of the Bolu that ran it — there is no task
                      screen, so the link opens that thread and says so rather than
                      promising a page that does not exist. */}
                  {agentId ? (
                    <Link
                      href={`/chat?agent=${agentId}`}
                      className="flex-none text-[13px] font-semibold text-bolu-ink underline"
                    >
                      Buka obrolan
                    </Link>
                  ) : (
                    <span className="flex-none text-[13px] text-bolu-muted">rutinitas dihapus</span>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </div>
  );
}

/** routineAgent finds which Bolu a run belongs to, through its routine. */
function routineAgent(run: RoutineRun, routines: Routine[]): string | null {
  if (!run.routine_id) return null;
  return routines.find((routine) => routine.id === run.routine_id)?.agent_id ?? null;
}
