"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useCallback, useEffect, useState } from "react";

import { Alert, AuthShell, Field, SubmitButton } from "@/components/auth/auth-shell";
import { Bolu } from "@/components/bolu/bolu";
import {
  BOLU_SHAPES,
  BOLU_SHAPE_NAMES,
  BOLU_SWATCHES,
  type BoluShapeName,
} from "@/components/bolu/characters";
import {
  acceptInvitation,
  createAgent,
  currentUser,
  listWorkspaces,
  onboard,
  refreshSession,
  setActiveWorkspaceId,
} from "@/lib/api";

const TIMEZONES = [
  { value: "Asia/Jakarta", label: "WIB — Jakarta" },
  { value: "Asia/Makassar", label: "WITA — Makassar" },
  { value: "Asia/Jayapura", label: "WIT — Jayapura" },
];

/**
 * What the workspace and its first Bolu start as. Both are editable, but neither
 * has to be touched: the form is a "Lanjut" away from a working workspace, which
 * is what somebody who just signed up is owed. The first Bolu is the seeded
 * "oren" profile — a sales assistant — so the workspace is not just empty rooms.
 */
const DEFAULT_WORKSPACE_NAME = "Usaha Saya";
const DEFAULT_BOLU = {
  template: "oren",
  name: "Oren",
  role: "Penjualan",
  shape: "kacang" as BoluShapeName,
  color: "#F28C4E",
};

const STEPS = ["Usaha", "Bolu pertama"] as const;

/**
 * Onboarding: the two steps from the sign-up flow. The first names the workspace;
 * the second makes its first Bolu.
 *
 * Only the last step writes. Nothing exists in the backend until the person is
 * through both, so an abandoned form leaves no half-built workspace behind — and
 * the onboard call is idempotent, so a retry after a failed Bolu reuses the
 * workspace it already made rather than starting a second one.
 */
export function OnboardingForm() {
  const router = useRouter();

  const [step, setStep] = useState<1 | 2>(1);

  const [name, setName] = useState(DEFAULT_WORKSPACE_NAME);
  const [businessField, setBusinessField] = useState("");
  const [timezone, setTimezone] = useState(TIMEZONES[0].value);

  const [boluName, setBoluName] = useState(DEFAULT_BOLU.name);
  const [boluRole, setBoluRole] = useState(DEFAULT_BOLU.role);
  const [shape, setShape] = useState<BoluShapeName>(DEFAULT_BOLU.shape);
  const [color, setColor] = useState<string>(DEFAULT_BOLU.color);

  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [ready, setReady] = useState(false);

  // A page reload loses the in-memory access token, so it is restored from the
  // refresh cookie before the form is used. An account that already has a
  // workspace has nothing left to do here and is sent on.
  //
  // What "already has one" means is the account's workspace list, not its
  // `onboarded` flag: the flag is the account's own claim, while the list is the
  // answer every other screen acts on. Trusting the flag alone would leave an
  // account that carries it with no workspace — the state the dashboard shell now
  // sends here — bouncing between /dashboard and this form, each screen sure the
  // other one is where the work is.
  useEffect(() => {
    void (async () => {
      const session = await refreshSession();
      if (!session.ok) {
        router.replace("/login");
        return;
      }
      const workspaces = await listWorkspaces();
      if (workspaces.ok && workspaces.data.length > 0) {
        router.replace("/dashboard");
        return;
      }
      setReady(true);
    })();
  }, [router]);

  function nextStep(event: React.FormEvent) {
    event.preventDefault();
    if (name.trim() === "") {
      setError("Beri nama usahamu dulu, ya.");
      return;
    }
    setError(null);
    setStep(2);
  }

  async function finish(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setPending(true);

    // The workspace first: its id is what the Bolu is created inside.
    const workspace = await onboard({ name: name.trim(), business_field: businessField.trim(), timezone });
    if (!workspace.ok) {
      setPending(false);
      setError(workspace.message);
      return;
    }

    const workspaceID = workspace.data.workspace.id;
    setActiveWorkspaceId(workspaceID);

    // Onboarding seeds Tim Bolu, so the team the first Bolu belongs to is the
    // workspace's own and the person is not asked to invent one first.
    const team = workspace.data.teams[0]?.id;
    if (team) {
      const bolu = await createAgent(workspaceID, {
        team_id: team,
        name: boluName.trim() || DEFAULT_BOLU.name,
        role: boluRole.trim(),
        shape,
        color,
        template_key: DEFAULT_BOLU.template,
      });
      if (!bolu.ok) {
        setPending(false);
        setError(`Workspace tersimpan, tapi Bolu pertama gagal dibuat: ${bolu.message}`);
        return;
      }
    }

    setPending(false);
    router.push("/dashboard");
  }

  return (
    <AuthShell
      title={step === 1 ? "Kenalan dulu" : "Bolu pertamamu"}
      subtitle={
        step === 1
          ? "Beri nama usahamu, lalu buat Bolu pertamamu. Semuanya bisa diubah nanti."
          : "Kami sudah menyiapkan satu Bolu untukmu. Ubah kalau mau, atau langsung selesai."
      }
      footer={
        <>
          Sudah punya workspace? <Link href="/login">Masuk</Link>
        </>
      }
    >
      {error ? <Alert tone="error">{error}</Alert> : null}

      <StepDots step={step} />

      {step === 1 ? (
        <form onSubmit={nextStep}>
          <Field
            id="name"
            label="Nama usaha"
            value={name}
            onChange={setName}
            placeholder="Toko Kaos Sinar"
            autoComplete="organization"
          />
          <Field
            id="business_field"
            label="Bidang usaha"
            value={businessField}
            onChange={setBusinessField}
            placeholder="Retail, kuliner, jasa…"
            required={false}
          />

          <div className="mb-5">
            <label htmlFor="timezone" className="mb-1.5 block text-sm font-medium text-bolu-body">
              Zona waktu
            </label>
            <select
              id="timezone"
              value={timezone}
              onChange={(event) => setTimezone(event.target.value)}
              className="w-full rounded-md border border-bolu-border bg-bolu-surface px-3.5 py-2.5 text-[15px] text-bolu-ink outline-none focus:border-bolu-accent focus:ring-2 focus:ring-bolu-accent/20"
            >
              {TIMEZONES.map((zone) => (
                <option key={zone.value} value={zone.value}>
                  {zone.label}
                </option>
              ))}
            </select>
            <p className="mt-1.5 text-xs text-bolu-muted">Dipakai untuk jadwal rutinitas dan jam kerja.</p>
          </div>

          <SubmitButton label="Lanjut" pending={!ready} pendingLabel="Menyiapkan…" />
        </form>
      ) : (
        <form onSubmit={finish}>
          <div className="mb-5 flex flex-col items-center gap-2">
            <Bolu shape={shape} color={color} size={120} />
            <p className="font-display text-[20px] font-semibold text-bolu-ink">
              {boluName.trim() || "Bolu baru"}
            </p>
            <p className="text-center text-[13px] text-bolu-muted">
              Bentuk ini yang muncul di sidebar dan di obrolan.
            </p>
          </div>

          <fieldset className="mb-5">
            <legend className="mb-1.5 block text-sm font-medium text-bolu-body">Bentuknya</legend>
            <div className="flex flex-wrap gap-2">
              {BOLU_SHAPE_NAMES.map((candidate) => (
                <button
                  key={candidate}
                  type="button"
                  aria-pressed={candidate === shape}
                  aria-label={BOLU_SHAPES[candidate].label}
                  title={BOLU_SHAPES[candidate].label}
                  onClick={() => setShape(candidate)}
                  className={`flex size-10 cursor-pointer items-center justify-center rounded-md border-2 ${
                    candidate === shape
                      ? "border-bolu-ink bg-bolu-panel"
                      : "border-bolu-line bg-bolu-surface hover:border-bolu-track"
                  }`}
                >
                  <Bolu shape={candidate} color={color} size={30} still />
                </button>
              ))}
            </div>
          </fieldset>

          <fieldset className="mb-5">
            <legend className="mb-1.5 block text-sm font-medium text-bolu-body">Warnanya</legend>
            <div className="flex flex-wrap items-center gap-2">
              {BOLU_SWATCHES.map((swatch) => (
                <button
                  key={swatch}
                  type="button"
                  aria-pressed={swatch === color}
                  aria-label={`Warna ${swatch}`}
                  title={swatch}
                  onClick={() => setColor(swatch)}
                  style={{ backgroundColor: swatch }}
                  className={`size-7 cursor-pointer rounded-full border-2 ${
                    swatch === color ? "border-bolu-ink" : "border-bolu-line"
                  }`}
                />
              ))}
              <label className="flex min-h-7 cursor-pointer items-center gap-2 rounded-md border border-bolu-border px-2.5 text-[13px] text-bolu-body">
                <input
                  type="color"
                  value={color}
                  onChange={(event) => setColor(event.target.value)}
                  aria-label="Warna lain"
                  className="size-4 cursor-pointer border-0 bg-transparent p-0"
                />
                Warna lain
              </label>
            </div>
          </fieldset>

          <Field
            id="bolu_name"
            label="Nama Bolu"
            value={boluName}
            onChange={setBoluName}
            placeholder="mis. Oren"
          />
          <Field
            id="bolu_role"
            label="Tugasnya"
            value={boluRole}
            onChange={setBoluRole}
            placeholder="mis. Menjawab WhatsApp dan mencatat pesanan"
            required={false}
          />

          <SubmitButton
            label={ready ? "Buat & mulai" : "Menyiapkan…"}
            pending={pending || !ready}
            pendingLabel="Menyiapkan workspace…"
          />

          <p className="mt-4 text-center text-sm text-bolu-muted">
            <button type="button" onClick={() => setStep(1)} className="cursor-pointer font-medium text-bolu-accent">
              Kembali
            </button>
          </p>
        </form>
      )}
    </AuthShell>
  );
}

/** StepDots is the small progress trail above the form. */
function StepDots({ step }: { step: 1 | 2 }) {
  return (
    <ol className="mb-5 flex items-center gap-2.5">
      {STEPS.map((label, index) => {
        const number = index + 1;
        const current = step === number;
        const done = step > number;
        return (
          <li key={label} className="flex items-center gap-2.5">
            {index > 0 ? <span aria-hidden className="h-px w-6 bg-bolu-border" /> : null}
            <span
              className={`flex size-6 flex-none items-center justify-center rounded-full border text-[12px] font-semibold ${
                current
                  ? "border-bolu-accent bg-bolu-accent text-bolu-solid-ink"
                  : done
                    ? "border-bolu-accent text-bolu-accent"
                    : "border-bolu-border text-bolu-muted"
              }`}
            >
              {done ? "✓" : number}
            </span>
            <span className={`text-[13px] font-medium ${current ? "text-bolu-ink" : "text-bolu-muted"}`}>
              {label}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

/** InviteAccept joins the signed-in account to the workspace that invited it. */
export function InviteAccept() {
  const router = useRouter();
  const token = useSearchParams().get("token");

  const [status, setStatus] = useState<"checking" | "ready" | "joining" | "failed">("checking");
  const [message, setMessage] = useState<string | null>(null);

  const join = useCallback(async () => {
    setStatus("joining");
    const result = await acceptInvitation(token ?? "");

    if (!result.ok) {
      setStatus("failed");
      setMessage(result.message);
      return;
    }

    setActiveWorkspaceId(result.data.id);
    router.push("/dashboard");
  }, [router, token]);

  useEffect(() => {
    void (async () => {
      if (!token) {
        setStatus("failed");
        setMessage("Tautan undangan tidak lengkap.");
        return;
      }

      // Accepting requires a signed-in account whose email matches the
      // invitation, so the session is restored first.
      const session = await refreshSession();
      if (!session.ok) {
        setStatus("failed");
        setMessage("Masuk dulu dengan email yang diundang, lalu buka tautan ini lagi.");
        return;
      }

      const user = await currentUser();
      if (!user.ok) {
        setStatus("failed");
        setMessage(user.message);
        return;
      }

      setStatus("ready");
      void user;
    })();
  }, [token]);

  return (
    <AuthShell title="Undangan bergabung" subtitle="Kami siapkan aksesmu ke workspace Bolu.">
      {status === "checking" ? <Alert tone="info">Memeriksa undangan…</Alert> : null}
      {status === "failed" ? (
        <>
          <Alert tone="error">{message ?? "Undangan tidak bisa dipakai."}</Alert>
          <p className="text-sm text-bolu-muted">
            Minta pengundang mengirim undangan baru, atau <Link href="/login">masuk</Link> dengan email yang diundang.
          </p>
        </>
      ) : null}
      {status === "ready" || status === "joining" ? (
        <button
          type="button"
          onClick={() => void join()}
          disabled={status === "joining"}
          className="w-full rounded-md bg-bolu-accent px-4 py-2.5 text-[15px] font-semibold text-bolu-solid-ink transition hover:bg-bolu-accent-strong disabled:opacity-60"
        >
          {status === "joining" ? "Bergabung…" : "Terima undangan"}
        </button>
      ) : null}
    </AuthShell>
  );
}
