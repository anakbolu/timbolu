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

  // The address can arrive in the URL: the sign-up form sends somebody here when
  // its own call ran out of time, and making them retype what they just entered
  // would be the second thing that attempt cost them.
  const params = useSearchParams();
  const [email, setEmail] = useState(() => params.get("email") ?? "");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  // Whether the error above is a call that ran out of time. It is kept apart from
  // the message because that failure has a next step rather than a retry.
  const [timedOut, setTimedOut] = useState(false);
  // Two things bring somebody here with something to say: a link that just verified
  // their address, and a session that ended underneath them. Both are said above the
  // form rather than as a refusal, because neither is the visitor's mistake.
  const [notice, setNotice] = useState<string | null>(() => {
    if (params.get("verified") === "1") {
      return "Email terverifikasi. Masuk untuk melanjutkan.";
    }
    if (params.get("sesi") === "berakhir") {
      return "Sesi berakhir. Masuk lagi.";
    }
    return null;
  });
  const [pending, setPending] = useState(false);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    setError(null);
    setNotice(null);
    setTimedOut(false);
    setPending(true);

    // Every exit below runs through `finally`, because the busy state is the only
    // thing standing between the person and a second submission: a path that
    // returned early — or threw — would leave the button saying "Mendaftarkan…"
    // with no way to try again.
    try {
      if (isRegister) {
        const result = await register(email, password);

        if (!result.ok) {
          setTimedOut(result.code === "timeout");
          setError(result.message);
          return;
        }

        // A deployment with verification off creates the account already usable, so
        // there is no link to wait for: sign in and land where the account belongs.
        // The register response carries the answer, which is what keeps the two
        // sides from having to agree on a second flag.
        if (result.data.email_verified) {
          const session = await login(email, password);
          if (!session.ok) {
            setTimedOut(session.code === "timeout");
            setError(session.message);
            return;
          }
          router.push(session.data.user.onboarded ? "/dashboard" : "/onboarding");
          return;
        }

        // The account exists but is not verified yet: the next step is the link
        // that was just emailed.
        router.push(`/verify?email=${encodeURIComponent(email)}`);
        return;
      }

      const result = await login(email, password);

      if (!result.ok) {
        setTimedOut(result.code === "timeout");
        setError(result.message);
        return;
      }
      if (!result.data.user.email_verified) {
        setNotice("Emailmu belum diverifikasi. Cek tautan yang kami kirim.");
        return;
      }
      router.push(result.data.user.onboarded ? "/dashboard" : "/onboarding");
    } finally {
      setPending(false);
    }
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
      {error ? (
        <Alert tone="error">
          {error}
          {/* Sign-up only, because only sign-up can leave an account behind: the
              deadline may have cut off a call that still created one, and the
              link is the way to reach it with the address already filled in. A
              login that ran out of time created nothing, so a link to the page
              the person is already on would be noise — that failure is a retry. */}
          {timedOut && isRegister ? (
            <>
              {" "}
              <Link className="underline" href={`/login?email=${encodeURIComponent(email)}`}>
                Masuk
              </Link>
            </>
          ) : null}
        </Alert>
      ) : null}
      {notice ? <Alert tone="info">{notice}</Alert> : null}

      {/* Tanpa method, submit native memakai GET dan kata sandi berakhir di URL
          (history, log akses, Referer) saat React belum sempat hydrate. */}
      <form method="post" action={isRegister ? "/signup" : "/login"} onSubmit={onSubmit}>
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
