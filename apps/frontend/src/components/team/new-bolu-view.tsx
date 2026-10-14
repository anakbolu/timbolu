"use client";

/**
 * Tambah Bolu: the one screen that makes a Bolu.
 *
 * The form asks for the three things a new Bolu cannot be without — the body it
 * is drawn as, the name it answers to, and the job it does — and stops there.
 * The persona, the tone, the tools and the rest hours belong to the Tim screen:
 * asking for them here would turn a minute's work into a form.
 *
 * The body chosen here is the body the sidebar and the thread will draw, because
 * the roster reads the stored shape rather than hashing the name.
 */

import { useRouter } from "next/navigation";
import { useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { Bolu } from "@/components/bolu/bolu";
import {
  BOLU_SHAPES,
  BOLU_SHAPE_NAMES,
  BOLU_SWATCHES,
  type BoluShapeName,
} from "@/components/bolu/characters";
import { useWorkspace } from "@/components/dashboard/workspace-data";
import { createAgent } from "@/lib/api";

const CARD = "rounded border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";
const FIELD =
  "min-h-11 w-full rounded border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent";
const LABEL = "flex flex-col gap-1.5 text-[14px] font-semibold";

export function NewBoluView() {
  const { teams, workspaceID, ready, reloadAgents } = useWorkspace();
  const router = useRouter();

  const [name, setName] = useState("");
  const [role, setRole] = useState("");
  const [shape, setShape] = useState<BoluShapeName>("kacang");
  const [color, setColor] = useState<string>(BOLU_SWATCHES[0]);
  const [teamID, setTeamID] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The registry needs a team. A workspace with one team is not asked a question
  // it has no answer to, so the first team is the default and the picker only
  // appears when there is a choice to make.
  const team = teamID || teams[0]?.id || "";
  const canSubmit = Boolean(workspaceID && team && name.trim() !== "" && !busy);

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!workspaceID || !canSubmit) {
      return;
    }

    setBusy(true);
    const result = await createAgent(workspaceID, {
      team_id: team,
      name: name.trim(),
      role: role.trim(),
      shape,
      color,
    });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    // The sidebar and the thread read one store, so the roster is reloaded before
    // the new Bolu's thread opens: otherwise the screen a person lands on shows a
    // conversation the sidebar does not list yet.
    await reloadAgents();
    router.push(`/chat?agent=${result.data.id}`);
  }

  if (ready && teams.length === 0) {
    return (
      <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Tambah Bolu
        </h1>
        <Alert tone="info">
          Belum ada tim untuk ditempati. Buat timnya dulu di halaman Tim.
        </Alert>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div>
        <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
          Tambah Bolu
        </h1>
        <div className="text-bolu-muted">
          Pilih wujudnya, beri nama, dan sebutkan tugasnya. Persona, gaya bahasa, dan alat bisa
          diatur kemudian di halaman Tim.
        </div>
      </div>

      {error ? <Alert tone="error">{error}</Alert> : null}

      <div className="grid gap-[18px] lg:grid-cols-[1fr_320px]">
        <section className={`${CARD} p-[22px]`}>
          <form onSubmit={submit} className="flex flex-col gap-5">
            <fieldset>
              <legend className={LABEL}>Ikon Bolu</legend>
              <div className="mt-1.5 flex flex-wrap gap-2">
                {BOLU_SHAPE_NAMES.map((candidate) => (
                  <button
                    key={candidate}
                    type="button"
                    aria-pressed={candidate === shape}
                    aria-label={BOLU_SHAPES[candidate].label}
                    title={BOLU_SHAPES[candidate].label}
                    onClick={() => setShape(candidate)}
                    className={`flex size-[54px] cursor-pointer items-center justify-center rounded border-2 ${
                      candidate === shape
                        ? "border-bolu-ink bg-bolu-panel"
                        : "border-bolu-line bg-white hover:border-bolu-track"
                    }`}
                  >
                    <Bolu shape={candidate} color={color} size={40} still />
                  </button>
                ))}
              </div>

              <div className="mt-3 flex flex-wrap items-center gap-2">
                {BOLU_SWATCHES.map((swatch) => (
                  <button
                    key={swatch}
                    type="button"
                    aria-pressed={swatch === color}
                    aria-label={`Warna ${swatch}`}
                    title={swatch}
                    onClick={() => setColor(swatch)}
                    style={{ backgroundColor: swatch }}
                    className={`size-8 cursor-pointer rounded-full border-2 ${
                      swatch === color ? "border-bolu-ink" : "border-bolu-line"
                    }`}
                  />
                ))}
                <label className="flex min-h-8 cursor-pointer items-center gap-2 rounded border border-bolu-border px-2.5 text-[14px] font-normal">
                  <input
                    type="color"
                    value={color}
                    onChange={(event) => setColor(event.target.value)}
                    aria-label="Warna lain"
                    className="size-5 cursor-pointer border-0 bg-transparent p-0"
                  />
                  Warna lain
                </label>
              </div>
            </fieldset>

            <label className={LABEL}>
              Nama Bolu
              <input
                value={name}
                onChange={(event) => setName(event.target.value)}
                required
                maxLength={80}
                placeholder="mis. Oren"
                className={FIELD}
              />
            </label>

            <label className={LABEL}>
              Tugas Bolu
              <input
                value={role}
                onChange={(event) => setRole(event.target.value)}
                placeholder="mis. Menjawab WhatsApp dan mencatat pesanan"
                className={FIELD}
              />
            </label>

            {teams.length > 1 ? (
              <label className={LABEL}>
                Tim
                <select
                  value={team}
                  onChange={(event) => setTeamID(event.target.value)}
                  className={FIELD}
                >
                  {teams.map((candidate) => (
                    <option key={candidate.id} value={candidate.id}>
                      {candidate.name}
                    </option>
                  ))}
                </select>
              </label>
            ) : null}

            <div>
              <button
                type="submit"
                disabled={!canSubmit}
                className="min-h-11 cursor-pointer rounded-full bg-bolu-ink px-6 font-bold text-white disabled:opacity-50"
              >
                {busy ? "Membuat…" : "Buat Bolu"}
              </button>
            </div>
          </form>
        </section>

        <aside className={`${CARD} flex flex-col items-center gap-3 p-[22px]`}>
          <h2 className={HEADING}>Wujudnya</h2>
          <Bolu shape={shape} color={color} size={132} />
          <p className="text-center font-display text-[20px] font-semibold">
            {name.trim() || "Bolu baru"}
          </p>
          <p className="text-center text-[14px] text-bolu-muted">
            {role.trim() || "Tugasnya belum ditulis."}
          </p>
          <p className="text-center text-[13px] text-bolu-muted">
            Wujud ini yang muncul di sidebar dan di obrolan.
          </p>
        </aside>
      </div>
    </div>
  );
}
