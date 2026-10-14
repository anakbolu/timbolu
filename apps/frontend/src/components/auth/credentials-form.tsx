"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";

import { Alert, AuthShell, Field, GoogleButton, SubmitButton } from "@/components/auth/auth-shell";
import { PasswordRules, passwordMeetsPolicy } from "@/components/auth/password-rules";
import { AuthScene } from "@/components/auth/auth-scene";
import { useSearchParams } from "next/navigation";

import { googleStartURL, login, register } from "@/lib/api";

/**
 * Whether this deployment shows the Google button. It is a build-time flag
 * because the value is inlined into the client bundle, and it is off unless the
 * deployment says otherwise — a button that leads to a "not configured" page is
 * worse than no button.
 */
const GOOGLE_LOGIN_ENABLED = process.env.NEXT_PUBLIC_GOOGLE_LOGIN_ENABLED === "true";

/**
 * One page for both entry points, because they share every field and only
 * differ in which endpoint they call and where they land afterwards.
 */
export function CredentialsForm({ mode }: { mode: "signup" | "login" }) {
  const router = useRouter();
  const isRegister = mode === "signup";

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const params = useSearchParams();
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(
    params.get("verified") === "1" ? "Email terverifikasi. Masuk untuk melanjutkan." : null,
  );
  const [pending, setPending] = useState(false);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setNotice(null);
    setPending(true);

    if (isRegister) {
      const result = await register(email, password);

      if (!result.ok) {
        setPending(false);
        setError(result.message);
        return;
      }

      // A deployment with verification off creates the account already usable, so
      // there is no link to wait for: sign in and land where the account belongs.
      // The register response carries the answer, which is what keeps the two
      // sides from having to agree on a second flag.
      if (result.data.email_verified) {
        const session = await login(email, password);
        setPending(false);
        if (!session.ok) {
          setError(session.message);
          return;
        }
        router.push(session.data.user.onboarded ? "/dashboard" : "/onboarding");
        return;
      }

      // The account exists but is not verified yet: the next step is the link
      // that was just emailed.
      setPending(false);
      router.push(`/verify?email=${encodeURIComponent(email)}`);
      return;
    }

    const result = await login(email, password);
    setPending(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }
    if (!result.data.user.email_verified) {
      setNotice("Emailmu belum diverifikasi. Cek tautan yang kami kirim.");
      return;
    }
    router.push(result.data.user.onboarded ? "/dashboard" : "/onboarding");
  }

  return (
    <AuthShell
      title={isRegister ? "Buat akun Bolu" : "Masuk ke Bolu"}
      subtitle={
        isRegister
          ? "Satu akun untuk semua usaha dan semua Bolu-mu."
          : "Lanjutkan pekerjaan yang sudah disiapkan Bolu."
      }
      scene={<AuthScene mode={isRegister ? "signup" : "login"} />}
      footer={
        isRegister ? (
          <>
            Sudah punya akun? <Link href="/login">Masuk</Link>
          </>
        ) : (
          <>
            Belum punya akun? <Link href="/signup">Daftar</Link>
          </>
        )
      }
    >
      {error ? <Alert tone="error">{error}</Alert> : null}
      {notice ? <Alert tone="info">{notice}</Alert> : null}

      <form onSubmit={onSubmit}>
        <Field
          id="email"
          label="Email"
          type="email"
          value={email}
          onChange={setEmail}
          placeholder="nama@usaha.com"
          autoComplete="email"
        />
        <Field
          id="password"
          label="Kata sandi"
          type="password"
          value={password}
          onChange={setPassword}
          placeholder="Minimal 8 karakter"
          autoComplete={isRegister ? "new-password" : "current-password"}
          minLength={8}
          hint={isRegister ? <PasswordRules value={password} /> : undefined}
        />

        <SubmitButton
          label={isRegister ? "Daftar" : "Masuk"}
          pending={pending}
          pendingLabel={isRegister ? "Mendaftarkan…" : "Masuk…"}
          disabled={isRegister && !passwordMeetsPolicy(password)}
        />
      </form>

      {GOOGLE_LOGIN_ENABLED ? <GoogleButton href={googleStartURL()} /> : null}

      {!isRegister ? (
        <p className="mt-4 text-center text-sm">
          <Link href="/forgot-password">Lupa kata sandi?</Link>
        </p>
      ) : null}
    </AuthShell>
  );
}
