"use client";

/**
 * Persetujuan: every draft waiting for a person, in one place.
 *
 * A draft was reachable only from the task that produced it, so a person who did
 * not remember the task had no way back to a decision they still owed. This screen
 * asks the API for the pending drafts directly — newest first — and each row opens
 * the draft's own page, where the payload and the decision live.
 *
 * The list is its own read rather than the store's drafts because the store keeps
 * every status: this screen's whole promise is "nothing else is waiting", and that
 * is only true of a list the API filtered.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { Bolu } from "@/components/bolu/bolu";
import { type Draft, ensureActiveWorkspace, listDrafts } from "@/lib/api";
import { allBot } from "@/lib/crew";
import { crewFor, expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";
import { NO_WORKSPACE_MESSAGE } from "@/lib/workspace-messages";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-bolu-surface";
const HEADING = "font-display text-[22px] font-semibold";
const BUTTON =
  "min-h-10 cursor-pointer rounded-md border border-bolu-border bg-bolu-surface px-3.5 text-[14px] font-medium text-bolu-ink transition hover:border-bolu-accent disabled:cursor-default disabled:opacity-50";

/**
 * ACTION_LABEL names the kinds a person actually meets. A kind that is not here
 * falls back to its own words, so a new action reads as "kirim faktur" rather
 * than as an id.
 */
const ACTION_LABEL: Record<string, string> = {
  "gmail.send": "Kirim email",
  "whatsapp.send_message": "Kirim WhatsApp",
};

/** ApprovalInbox is the list of drafts waiting for a decision. */
export function ApprovalInbox() {
  const { workspaceID, agents } = useWorkspace();

  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [load, setLoad] = useState<"loading" | "ready" | "error">("loading");
  const [message, setMessage] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  const read = useCallback(async (workspace: string) => {
    setLoad("loading");
    setMessage(null);

    const result = await listDrafts(workspace, { status: "pending", limit: 50 });

    if (!result.ok) {
      // The screen says what it could not do rather than printing the server's
      // envelope: a raw code is an operator's sentence, not a person's.
      setLoad("error");
      setMessage("Daftar draf gagal dimuat. Cek koneksimu lalu coba lagi.");
      return;
    }

    setDrafts(result.data);
    setLoad("ready");
  }, []);

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const workspace = workspaceID ?? (await ensureActiveWorkspace());
      if (cancelled) return;
      if (!workspace) {
        setLoad("error");
        setMessage(NO_WORKSPACE_MESSAGE);
        return;
      }

      await read(workspace);
    })();

    return () => {
      cancelled = true;
    };
  }, [workspaceID, read, attempt]);

  const count = drafts.length;

  return (
    <div className="flex flex-col gap-6 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <div>
        <h1 className={HEADING}>Persetujuan</h1>
        <p className="mt-1 text-[15px] text-bolu-muted">Draf yang menunggu keputusanmu, terbaru dulu.</p>
      </div>

      {load === "loading" ? (
        <p className="text-[15px] text-bolu-muted">Memuat draf…</p>
      ) : load === "error" ? (
        <div className="flex flex-col items-start gap-3">
          <Alert tone="error">{message ?? "Daftar draf gagal dimuat."}</Alert>
          <button type="button" className={BUTTON} onClick={() => setAttempt((value) => value + 1)}>
            Coba lagi
          </button>
        </div>
      ) : count === 0 ? (
        <div className={`${CARD} flex flex-col items-center gap-2.5 px-3 py-10 text-center`}>
          <Bolu
            member={agents[0] ? crewFor(agents[0]) : allBot("Pinky")}
            expression={expressionOf(agents[0] ? stateOf(agents[0]) : "idle")}
            color={agents[0]?.color || undefined}
            size={90}
          />
          <div className="font-display text-[17px] font-semibold">Tidak ada draf yang menunggu keputusan.</div>
          <p className="max-w-[420px] text-[14px] text-bolu-muted">
            Draf muncul di sini begitu ada Bolu yang menyiapkan aksi untuk kamu setujui.
          </p>
        </div>
      ) : (
        <section className={`${CARD} flex flex-col gap-2 p-5`}>
          <div className="flex flex-wrap items-center justify-between gap-2.5">
            <h2 className={HEADING}>Menunggu keputusanmu</h2>
            <span className="text-[14px] text-bolu-muted">{count === 1 ? "1 draf" : `${count} draf`}</span>
          </div>

          <div className="flex flex-col gap-2">
            {drafts.map((draft) => {
              const agent = agents.find((item) => item.id === draft.agent_id) ?? null;
              // The kind is the draft's own vocabulary, so a kind this screen does
              // not name is shown with its underscores read as spaces rather than
              // hidden behind "Draf".
              const kind = ACTION_LABEL[draft.action_kind] ?? draft.action_kind.replace(/[._]/g, " ").trim();
              return (
                <Link
                  key={draft.draft_id}
                  href={`/approval/${draft.draft_id}`}
                  className="flex flex-wrap items-start gap-3 rounded-md border border-bolu-line p-[10px_12px] text-bolu-ink no-underline transition hover:border-bolu-accent hover:text-bolu-ink"
                >
                  <div className="size-11 flex-none">
                    {agent ? (
                      <Bolu
                        member={crewFor(agent)}
                        expression={expressionOf(stateOf(agent))}
                        color={agent.color || undefined}
                        size={44}
                      />
                    ) : null}
                  </div>

                  <div className="min-w-0 flex-[1_1_240px]">
                    <div className="flex flex-wrap items-center gap-2 text-[13px] text-bolu-muted">
                      <span className="font-semibold">{agent?.name ?? "Bolu"}</span>
                      <span aria-hidden="true">·</span>
                      <span className="rounded-full bg-bolu-flag/12 px-2 py-0.5 font-semibold text-bolu-flag">
                        {kind || "Draf"}
                      </span>
                    </div>
                    <div className="mt-1 truncate font-semibold">{draft.title || "Draf menunggu persetujuan"}</div>
                    {draft.summary ? (
                      <p className="mt-0.5 line-clamp-2 text-[14px] text-bolu-muted">{draft.summary}</p>
                    ) : null}
                  </div>

                  <div className="flex-none text-[13px] text-bolu-muted">{relativeAge(draft.created_at)}</div>
                </Link>
              );
            })}
          </div>
        </section>
      )}
    </div>
  );
}
