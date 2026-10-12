"use client";

/**
 * The dashboard: the front door.
 *
 * It reads the workspace's real state — its Bolu, its conversations, and the
 * drafts waiting for a decision — and links to the screens that do the work. What
 * it deliberately does not do is invent anything: a new workspace has no Bolu, and
 * the screen says so and offers to make one. A front door that shows colleagues
 * who do not exist is worse than an empty one, because the first thing a person
 * learns about the product is wrong.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { SignOutButton } from "@/components/auth/sign-out-button";
import {
  Agent,
  ChatConversation,
  Draft,
  ensureActiveWorkspace,
  listAgentTeams,
  listConversations,
  listDrafts,
  refreshSession,
} from "@/lib/api";

/** Overview is what the dashboard reads once. */
type Overview = {
  workspaceID: string;
  bots: Agent[];
  conversations: ChatConversation[];
  pending: Draft[];
};

export function DashboardOverview() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    // The session is checked first, because it is the reason that matters to a
    // person: with no session the answer is "sign in", and only an account that is
    // signed in but owns no workspace is asked to pick one.
    const session = await refreshSession();
    if (!session.ok) {
      setLoading(false);
      setError("Sesi berakhir. Masuk lagi.");
      return;
    }

    const workspace = await ensureActiveWorkspace();
    if (!workspace) {
      setLoading(false);
      setError("Pilih workspace dulu.");
      return;
    }

    const [teamResult, conversationResult, draftResult] = await Promise.all([
      listAgentTeams(workspace),
      listConversations(workspace),
      listDrafts(workspace, { status: "pending" }),
    ]);

    setLoading(false);

    if (!teamResult.ok) {
      setError(teamResult.message);
      return;
    }

    setError(null);
    setOverview({
      workspaceID: workspace,
      bots: teamResult.data.flatMap((team) => team.agents),
      conversations: conversationResult.ok ? conversationResult.data : [],
      pending: draftResult.ok ? draftResult.data : [],
    });
  }, []);

  useEffect(() => {
    void (async () => {
      await load();
    })();
  }, [load]);

  if (loading) {
    return (
      <main className="mx-auto w-full max-w-5xl px-5 py-10">
        <p className="text-bolu-muted">Memuat…</p>
      </main>
    );
  }

  if (!overview) {
    return (
      <main className="mx-auto w-full max-w-5xl px-5 py-10">
        <Alert tone="info">
          {error ?? "Belum ada workspace aktif."} <Link href="/login">Masuk</Link> dulu.
        </Alert>
      </main>
    );
  }

  const { bots, conversations, pending } = overview;

  return (
    <main className="mx-auto w-full max-w-5xl px-5 py-10">
      <header className="mb-8 flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="font-display text-3xl font-semibold">Dasbor</h1>
          <p className="mt-2 text-bolu-body">
            {bots.length === 0
              ? "Tim Bolu-mu masih kosong. Buat anggota pertama untuk mulai bekerja."
              : `${bots.length} Bolu siap bekerja.`}
          </p>
        </div>
        <SignOutButton className="min-h-10 cursor-pointer rounded-full border border-bolu-border bg-white px-4 text-sm font-semibold text-bolu-ink disabled:opacity-60" />
      </header>

      {error ? <Alert tone="error">{error}</Alert> : null}

      {/* The empty state is the first thing a new workspace sees, so it is the
          thing that has to be most useful. */}
      {bots.length === 0 ? (
        <section className="rounded-3xl border border-dashed border-bolu-border bg-bolu-surface px-6 py-10 text-center">
          {/* A Bolu with no crew shape yet is drawn as the plain disc the rest of
              the app uses for it, rather than borrowing a character's face. */}
          <div
            aria-hidden
            className="mx-auto mb-4 size-16 rounded-full bg-bolu-accent/20 ring-8 ring-bolu-accent/10"
          />
          <h2 className="font-display text-xl font-semibold">Belum ada Bolu</h2>
          <p className="mx-auto mt-2 max-w-md text-bolu-body">
            Bolu adalah anggota tim yang mengerjakan tugas untukmu. Buat satu, beri nama dan peran,
            lalu suruh dia bekerja lewat obrolan.
          </p>
          <Link
            href="/settings/team"
            className="mt-5 inline-flex min-h-11 items-center rounded-full bg-bolu-accent px-5 font-semibold text-white"
          >
            Buat Bolu pertama
          </Link>
        </section>
      ) : (
        <section className="mb-8">
          <h2 className="mb-3 font-display text-xl font-semibold">Tim Bolu</h2>
          <ul className="grid gap-2 sm:grid-cols-2">
            {bots.map((bot) => (
              <li key={bot.id}>
                <Link
                  href="/chat"
                  className="flex items-center gap-3 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3 transition hover:bg-bolu-panel/50"
                >
                  <span
                    aria-hidden
                    className="inline-block size-8 shrink-0 rounded-full"
                    style={{ backgroundColor: bot.color || "#8b88a0" }}
                  />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate font-medium">{bot.name}</span>
                    <span className="block truncate text-sm text-bolu-muted">{bot.role || "—"}</span>
                  </span>
                  <span className="shrink-0 rounded-full bg-bolu-panel px-2.5 py-1 text-xs font-medium text-bolu-muted">
                    {bot.display_status === "working"
                      ? "Bekerja"
                      : bot.display_status === "waiting"
                        ? "Menunggu kamu"
                        : bot.display_status === "resting"
                          ? "Istirahat"
                          : "Santai"}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        </section>
      )}

      <div className="grid gap-4 sm:grid-cols-3">
        <Tile
          href="/chat"
          label="Obrolan"
          value={String(conversations.length)}
          note={conversations.length === 0 ? "Belum ada obrolan" : "Percakapan aktif"}
        />
        <Tile
          href="/groups"
          label="Grup"
          value={String(conversations.filter((row) => row.kind === "group").length)}
          note="Beberapa Bolu dalam satu ruang"
        />
        <Tile
          href="/dashboard/integrations"
          label="Integrasi"
          value="—"
          note="Sambungkan aplikasi luar"
        />
      </div>

      {/* A draft is the one thing on this screen that is waiting on a person, so it
          is shown rather than counted. */}
      {pending.length > 0 ? (
        <section className="mt-8">
          <h2 className="mb-3 font-display text-xl font-semibold">Menunggu keputusanmu</h2>
          <ul className="space-y-2">
            {pending.map((draft) => (
              <li
                key={draft.draft_id}
                className="flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-bolu-flag/30 bg-bolu-flag/5 px-4 py-3"
              >
                <div className="min-w-0">
                  <p className="text-xs uppercase tracking-wide text-bolu-muted">{draft.action_kind}</p>
                  <p className="truncate font-semibold">{draft.title}</p>
                </div>
                <Link
                  href="/chat"
                  className="min-h-9 rounded-full bg-bolu-ink px-3.5 py-2 text-sm font-semibold text-white"
                >
                  Lihat
                </Link>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <nav className="mt-8 flex flex-wrap gap-2 border-t border-bolu-line pt-6">
        {[
          ["/settings/team", "Tim"],
          ["/chat", "Obrolan"],
          ["/groups", "Grup"],
          ["/office", "Kantor"],
          ["/dashboard/integrations", "Integrasi"],
          ["/settings/model", "Model"],
        ].map(([href, label]) => (
          <Link
            key={href}
            href={href}
            className="min-h-10 rounded-full border border-bolu-border bg-white px-4 py-2 text-sm font-semibold text-bolu-ink"
          >
            {label}
          </Link>
        ))}
      </nav>
    </main>
  );
}

/** Tile is one counted link. */
function Tile({
  href,
  label,
  value,
  note,
}: {
  href: string;
  label: string;
  value: string;
  note: string;
}) {
  return (
    <Link
      href={href}
      className="flex flex-col gap-1 rounded-2xl border border-bolu-border bg-bolu-surface p-4 transition hover:bg-bolu-panel/50"
    >
      <span className="text-sm text-bolu-muted">{label}</span>
      <span className="font-display text-2xl font-semibold">{value}</span>
      <span className="text-sm text-bolu-muted">{note}</span>
    </Link>
  );
}
