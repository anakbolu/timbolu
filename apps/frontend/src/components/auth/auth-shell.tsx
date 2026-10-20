"use client";

/**
 * Shared shell for the authentication pages: the bolu mark, the card, the
 * heading, the error line, the primary button, and — where a page passes one —
 * the crew working in the room beside the card. Every page in this flow is a
 * client component, because the access token lives in memory and the forms are
 * interactive.
 */

import Link from "next/link";
import type { ReactNode } from "react";

import { TimboluMark } from "@/components/bolu/timbolu-mark";
import { ThemeToggle } from "@/components/ui/theme-toggle";

type AuthShellProps = {
  title: string;
  subtitle: string;
  children: ReactNode;
  footer?: ReactNode;
  /** The crew at work, shown beside the card. Login and register pass one. */
  scene?: ReactNode;
};

export function AuthShell({ title, subtitle, children, footer, scene }: AuthShellProps) {
  return (
    <main className="mx-auto flex w-full max-w-[1120px] flex-1 flex-col justify-center gap-10 px-5 py-12 lg:flex-row lg:items-center lg:justify-center lg:gap-16">
      {/* The auth screens have no nav of their own, and the theme is the one
          preference a person may want before they even have an account. */}
      <div className="fixed right-5 top-5 z-20">
        <ThemeToggle />
      </div>
      <div className="mx-auto flex w-full max-w-[460px] flex-col lg:mx-0 lg:flex-none">
        <Link href="/" className="mx-auto mb-8 block w-fit text-bolu-ink no-underline">
          <TimboluMark />
        </Link>

        <div className="rounded-md border border-bolu-border bg-bolu-surface p-7 shadow-[0_1px_2px_rgb(30_27_46_/_0.04)]">
          <h1 className="font-display text-2xl font-semibold text-bolu-ink">{title}</h1>
          <p className="mt-2 text-sm text-bolu-muted">{subtitle}</p>
          <div className="mt-6">{children}</div>
        </div>

        {footer ? <div className="mt-6 text-center text-sm text-bolu-muted">{footer}</div> : null}
      </div>

      {/* The room is a wide-screen treat: on phones it would only push the form down. */}
      {scene ? <div className="hidden w-full lg:block lg:min-w-0 lg:flex-1">{scene}</div> : null}
    </main>
  );
}

type FieldProps = {
  id: string;
  label: string;
  type?: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  autoComplete?: string;
  required?: boolean;
  minLength?: number;
  /** Extra guidance under the field, e.g. the password policy checklist. */
  hint?: ReactNode;
};

export function Field({
  id,
  label,
  type = "text",
  value,
  onChange,
  placeholder,
  autoComplete,
  required = true,
  minLength,
  hint,
}: FieldProps) {
  return (
    <div className="mb-4">
      <label htmlFor={id} className="mb-1.5 block text-sm font-medium text-bolu-body">
        {label}
      </label>
      <input
        id={id}
        name={id}
        type={type}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        autoComplete={autoComplete}
        required={required}
        minLength={minLength}
        className="w-full rounded-md border border-bolu-border bg-bolu-surface px-3.5 py-2.5 text-[15px] text-bolu-ink outline-none transition focus:border-bolu-accent focus:ring-2 focus:ring-bolu-accent/20"
      />
      {hint ? <div className="mt-1.5 text-xs text-bolu-muted">{hint}</div> : null}
    </div>
  );
}

export function SubmitButton({
  label,
  pending,
  pendingLabel,
  disabled = false,
}: {
  label: string;
  pending: boolean;
  pendingLabel: string;
  /** Blocks submission while the form is not yet valid, e.g. a weak password. */
  disabled?: boolean;
}) {
  return (
    <button
      type="submit"
      disabled={pending || disabled}
      className="w-full rounded-md bg-bolu-accent px-4 py-2.5 text-[15px] font-semibold text-bolu-solid-ink transition hover:bg-bolu-accent-strong disabled:cursor-not-allowed disabled:opacity-60"
    >
      {pending ? pendingLabel : label}
    </button>
  );
}

export function Alert({ tone, children }: { tone: "error" | "success" | "info"; children: ReactNode }) {
  const styles = {
    error: "border-bolu-belum/30 bg-bolu-belum/8 text-bolu-belum",
    success: "border-bolu-lunas/30 bg-bolu-lunas/8 text-bolu-lunas",
    info: "border-bolu-border bg-bolu-panel/60 text-bolu-body",
  } as const;

  return (
    <p role={tone === "error" ? "alert" : "status"} className={`mb-4 rounded-md border px-3.5 py-2.5 text-sm ${styles[tone]}`}>
      {children}
    </p>
  );
}

export function GoogleButton({ href }: { href: string }) {
  return (
    <a
      href={href}
      className="mt-3 flex w-full items-center justify-center gap-2.5 rounded-md border border-bolu-border bg-bolu-surface px-4 py-2.5 text-[15px] font-medium text-bolu-ink transition hover:bg-bolu-hover/60"
    >
      {/* The four-colour Google "G" mark, inlined so the button needs no icon package. */}
      <svg viewBox="0 0 48 48" aria-hidden="true" className="size-[18px] flex-none">
        <path
          fill="#EA4335"
          d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"
        />
        <path
          fill="#4285F4"
          d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"
        />
        <path
          fill="#FBBC05"
          d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"
        />
        <path
          fill="#34A853"
          d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"
        />
      </svg>
      Masuk dengan Google
    </a>
  );
}
