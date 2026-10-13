"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useCallback, useEffect, useState } from "react";

import { Alert, AuthShell, Field, SubmitButton } from "@/components/auth/auth-shell";
import { acceptInvitation, currentUser, onboard, refreshSession, setActiveWorkspaceId } from "@/lib/api";

const TIMEZONES = [
  { value: "Asia/Jakarta", label: "WIB — Jakarta" },
  { value: "Asia/Makassar", label: "WITA — Makassar" },
  { value: "Asia/Jayapura", label: "WIT — Jayapura" },
];

/**
 * Onboarding: the short form from the sign-up flow. It creates the workspace,
 * makes the caller its owner, and creates Tim Bolu and Tim Hore.
 */
export function OnboardingForm() {
  const router = useRouter();

  const [name, setName] = useState("");
  const [businessField, setBusinessField] = useState("");
  const [timezone, setTimezone] = useState(TIMEZONES[0].value);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [ready, setReady] = useState(false);

  // A page reload loses the in-memory access token, so it is restored from the
  // refresh cookie before the form is used.
  useEffect(() => {
    void (async () => {
      const result = await refreshSession();
      if (!result.ok) {
        router.replace("/login");
        return;
      }
      setReady(true);
    })();
  }, [router]);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setPending(true);

    const result = await onboard({ name, business_field: businessField, timezone });
    setPending(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setActiveWorkspaceId(result.data.workspace.id);
    router.push("/dashboard");
  }

  return (
    <AuthShell
      title="Kenalan dulu"
      subtitle="Tiga hal singkat, lalu tim Bolu-mu siap bekerja."
      footer={
        <>
          Sudah punya workspace? <Link href="/login">Masuk</Link>
        </>
      }
    >
      {error ? <Alert tone="error">{error}</Alert> : null}

      <form onSubmit={onSubmit}>
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
            className="w-full rounded border border-bolu-border bg-bolu-surface px-3.5 py-2.5 text-[15px] text-bolu-ink outline-none focus:border-bolu-accent focus:ring-2 focus:ring-bolu-accent/20"
          >
            {TIMEZONES.map((zone) => (
              <option key={zone.value} value={zone.value}>
                {zone.label}
              </option>
            ))}
          </select>
          <p className="mt-1.5 text-xs text-bolu-muted">Dipakai untuk jadwal rutinitas dan jam kerja.</p>
        </div>

        <SubmitButton
          label={ready ? "Buat workspace" : "Menyiapkan…"}
          pending={pending || !ready}
          pendingLabel="Membuat workspace…"
        />
      </form>
    </AuthShell>
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
          className="w-full rounded bg-bolu-accent px-4 py-2.5 text-[15px] font-semibold text-white transition hover:bg-[#1a6cba] disabled:opacity-60"
        >
          {status === "joining" ? "Bergabung…" : "Terima undangan"}
        </button>
      ) : null}
    </AuthShell>
  );
}
