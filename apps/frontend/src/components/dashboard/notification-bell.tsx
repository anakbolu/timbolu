"use client";

import { Bell } from "lucide-react";
import Link from "next/link";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  type AttentionTask,
  type Draft,
  listAttentionTasks,
} from "@/lib/api";

import { useWorkspace } from "./workspace-data";

/** ATTENTION is how each reason maps to display tokens in the notification panel. */
const ATTENTION_LABELS: Record<AttentionTask["attention"], { label: string; tone: string }> = {
  failed: {
    label: "Gagal",
    tone: "border-bolu-belum/30 bg-bolu-belum/8 text-bolu-belum",
  },
  stuck: {
    label: "Macet",
    tone: "border-bolu-amber/30 bg-bolu-amber/8 text-bolu-amber",
  },
  waiting: {
    label: "Menunggu",
    tone: "border-bolu-track bg-bolu-track/10 text-bolu-muted",
  },
};

/**
 * NotificationBell provides a quick notification popover from the dashboard header.
 *
 * It reports:
 * 1. Menunggu persetujuan (N) - drafts waiting for a decision, linking to /approval/[draft_id]
 * 2. Butuh perhatian (N) - tasks that failed, are stuck, or need attention, linking to /tasks/[task_id]
 */
export function NotificationBell() {
  const { workspaceID, pendingDrafts, pendingCount, ready } = useWorkspace();
  const [open, setOpen] = useState(false);
  const [attentionTasks, setAttentionTasks] = useState<AttentionTask[]>([]);
  const [loadingAttention, setLoadingAttention] = useState(false);
  const [attentionError, setAttentionError] = useState<string | null>(null);

  const containerRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);

  const totalCount = (pendingCount || pendingDrafts.length) + attentionTasks.length;

  const fetchAttention = useCallback((wsId: string) => {
    void (async () => {
      await Promise.resolve();
      setLoadingAttention(true);
      setAttentionError(null);
      const result = await listAttentionTasks(wsId);
      if (!result.ok) {
        setAttentionError("Gagal memuat notifikasi.");
        setAttentionTasks([]);
      } else {
        setAttentionTasks(result.data);
      }
      setLoadingAttention(false);
    })();
  }, []);

  // Fetch attention tasks once workspace is ready or changes
  useEffect(() => {
    if (!ready || !workspaceID) return;
    void fetchAttention(workspaceID);
  }, [ready, workspaceID, fetchAttention]);

  // Re-fetch when the popover opens so it's always up to date
  useEffect(() => {
    if (open && workspaceID) {
      void fetchAttention(workspaceID);
    }
  }, [open, workspaceID, fetchAttention]);

  // Handle outside click & escape key to close
  useEffect(() => {
    if (!open) return;

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") {
        setOpen(false);
        buttonRef.current?.focus();
      }
    }

    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    }

    document.addEventListener("keydown", handleKeyDown);
    document.addEventListener("mousedown", handleClickOutside);

    return () => {
      document.removeEventListener("keydown", handleKeyDown);
      document.removeEventListener("mousedown", handleClickOutside);
    };
  }, [open]);

  const hasDrafts = pendingDrafts.length > 0;
  const hasAttention = attentionTasks.length > 0;
  const isEmpty = !hasDrafts && !hasAttention;

  return (
    <div ref={containerRef} className="relative">
      <button
        ref={buttonRef}
        type="button"
        aria-label="Notifikasi"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((prev) => !prev)}
        className="relative flex size-11 cursor-pointer items-center justify-center rounded-full border border-bolu-border bg-bolu-surface text-bolu-ink transition hover:border-bolu-track focus-visible:outline-2 focus-visible:outline-bolu-accent"
      >
        <Bell size={20} strokeWidth={2} aria-hidden="true" />
        {totalCount > 0 ? (
          <span className="absolute -top-1 -right-1 flex min-w-5 h-5 items-center justify-center rounded-full bg-bolu-belum px-1 text-[11px] font-bold text-bolu-solid-ink ring-2 ring-bolu-surface">
            {totalCount > 99 ? "99+" : totalCount}
          </span>
        ) : null}
      </button>

      {open ? (
        <div
          role="dialog"
          aria-label="Notifikasi"
          className="absolute right-0 z-50 mt-2 w-[340px] max-w-[calc(100vw-32px)] rounded-md border border-bolu-border bg-bolu-surface p-4 shadow-lg text-bolu-ink max-[760px]:right-[-50px]"
        >
          <div className="flex items-center justify-between border-b border-bolu-line pb-2.5">
            <span className="font-display text-[16px] font-semibold">Notifikasi</span>
            {totalCount > 0 ? (
              <span className="text-[12px] text-bolu-muted">{totalCount} item</span>
            ) : null}
          </div>

          <div className="mt-3 max-h-[380px] overflow-y-auto flex flex-col gap-4">
            {attentionError ? (
              <div className="py-2 text-[14px] text-bolu-muted">
                {attentionError}
              </div>
            ) : null}

            {isEmpty && !attentionError && !loadingAttention ? (
              <div className="py-6 text-center text-[14px] text-bolu-muted">
                Tidak ada yang menunggu.
              </div>
            ) : null}

            {/* Section 1: Menunggu persetujuan */}
            {hasDrafts ? (
              <div>
                <div className="flex items-center justify-between text-[13px] font-semibold text-bolu-muted mb-2">
                  <span>Menunggu persetujuan ({pendingDrafts.length})</span>
                  <Link
                    href="/approval"
                    onClick={() => setOpen(false)}
                    className="text-[12px] text-bolu-link hover:underline font-normal"
                  >
                    Lihat semua
                  </Link>
                </div>
                <div className="flex flex-col gap-1.5">
                  {pendingDrafts.slice(0, 5).map((draft: Draft) => (
                    <Link
                      key={draft.draft_id}
                      href={`/approval/${draft.draft_id}`}
                      onClick={() => setOpen(false)}
                      className="group flex flex-col rounded border border-bolu-line p-2 text-bolu-ink no-underline transition hover:border-bolu-accent hover:bg-bolu-hover/30"
                    >
                      <div className="truncate text-[13px] font-semibold group-hover:text-bolu-accent">
                        {draft.title || draft.action_kind}
                      </div>
                      <div className="truncate text-[12px] text-bolu-muted">
                        {draft.action_kind}
                      </div>
                    </Link>
                  ))}
                </div>
              </div>
            ) : null}

            {/* Section 2: Butuh perhatian */}
            {hasAttention ? (
              <div>
                <div className="flex items-center justify-between text-[13px] font-semibold text-bolu-muted mb-2">
                  <span>Butuh perhatian ({attentionTasks.length})</span>
                </div>
                <div className="flex flex-col gap-1.5">
                  {attentionTasks.slice(0, 5).map((task) => {
                    const reason = ATTENTION_LABELS[task.attention];
                    return (
                      <Link
                        key={task.task_id}
                        href={`/tasks/${task.task_id}`}
                        onClick={() => setOpen(false)}
                        className="group flex items-center justify-between gap-2 rounded border border-bolu-line p-2 text-bolu-ink no-underline transition hover:border-bolu-accent hover:bg-bolu-hover/30"
                      >
                        <span className="min-w-0 flex-1 truncate text-[13px] font-medium group-hover:text-bolu-accent">
                          {task.title || task.task_id}
                        </span>
                        <span
                          className={`flex-none rounded-full border px-2 py-0.5 text-[11px] font-semibold ${reason.tone}`}
                        >
                          {reason.label}
                        </span>
                      </Link>
                    );
                  })}
                </div>
              </div>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
