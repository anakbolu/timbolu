"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useState } from "react";

import { Alert, AuthShell, Field, SubmitButton } from "@/components/auth/auth-shell";
import { PasswordRules, passwordMeetsPolicy } from "@/components/auth/password-rules";
import { forgotPassword, resetPassword } from "@/lib/api";

/**
 * Password reset. With a token in the URL it asks for the new password;
 * without one it asks for the address and emails the link. The backend answers
 * identically whether or not the address exists, so this page must not suggest
 * otherwise either.
 */
export function PasswordResetForm() {
  const router = useRouter();
  const token = useSearchParams().get("token");

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  async function onRequest(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setMessage(null);
    setPending(true);

    const result = await forgotPassword(email);
    setPending(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    setMessage("Kalau alamat itu terdaftar, tautan atur ulang sudah dikirim. Cek emailmu.");
  }

  async function onReset(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setMessage(null);
    setPending(true);

    const result = await resetPassword(token ?? "", password);
    setPending(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    setMessage("Kata sandi diperbarui. Semua sesi lama sudah diakhiri — silakan masuk lagi.");
    setTimeout(() => router.push("/login"), 1200);
  }

  if (token) {
    return (
      <AuthShell
        title="Kata sandi baru"
        subtitle="Pilih kata sandi baru untuk akunmu."
        footer={
          <>
            Sudah ingat? <Link href="/login">Masuk</Link>
          </>
        }
      >
        {error ? <Alert tone="error">{error}</Alert> : null}
        {message ? <Alert tone="success">{message}</Alert> : null}

        <form onSubmit={onReset}>
          <Field
            id="password"
            label="Kata sandi baru"
            type="password"
            value={password}
            onChange={setPassword}
            autoComplete="new-password"
            minLength={8}
            hint={<PasswordRules value={password} />}
          />
          <SubmitButton
            label="Simpan kata sandi"
            pending={pending}
            pendingLabel="Menyimpan…"
            disabled={!passwordMeetsPolicy(password)}
          />
        </form>
      </AuthShell>
    );
  }

  return (
    <AuthShell
      title="Lupa kata sandi"
      subtitle="Kami kirim tautan untuk mengatur ulang kata sandimu."
      footer={
        <>
          Ingat kata sandimu? <Link href="/login">Masuk</Link>
        </>
      }
    >
      {error ? <Alert tone="error">{error}</Alert> : null}
      {message ? <Alert tone="success">{message}</Alert> : null}

      <form onSubmit={onRequest}>
        <Field
          id="email"
          label="Email"
          type="email"
          value={email}
          onChange={setEmail}
          placeholder="nama@usaha.com"
          autoComplete="email"
        />
        <SubmitButton label="Kirim tautan" pending={pending} pendingLabel="Mengirim…" />
      </form>
    </AuthShell>
  );
}
