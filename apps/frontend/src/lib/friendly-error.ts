/**
 * What a deep-linked page says when its record cannot be read.
 *
 * The API's messages are written for whoever calls it next: `task id is not a
 * uuid`, `task not found`, `internal server error`. A person who followed a
 * notification lands on one of those sentences and has no idea what to do, so
 * the screens that open a single record translate the reason into one of three
 * answers — the link is broken, the record is gone, or the read can be retried —
 * and keep the exit back to a list.
 *
 * The mapping is shared rather than written in each page because the task page
 * and the draft page must not disagree about what a 404 means.
 */

import type { ApiError } from "@/lib/api";

export type DeepLinkKind = "task" | "draft" | "conversation" | "agent";

export type LoadFailure = {
  /** The sentence the screen shows. Never the API's own wording for a broken id. */
  message: string;
  /** Whether repeating the same read could give a different answer. */
  retryable: boolean;
};

const INVALID_LINK: Record<DeepLinkKind, string> = {
  task: "Tautan tidak valid — id tugas di tautan ini tidak dikenali. Buka dasbor lalu pilih tugas dari kartunya.",
  draft: "Tautan tidak valid — id draf di tautan ini tidak dikenali. Buka daftar draf lalu pilih dari sana.",
  conversation:
    "Tautan tidak valid — id obrolan di tautan ini tidak dikenali. Buka daftar obrolan lalu pilih dari sana.",
  agent:
    "Tautan tidak valid — id Bolu di tautan ini tidak dikenali. Buka daftar obrolan lalu pilih dari sana.",
};

const NOT_FOUND: Record<DeepLinkKind, string> = {
  task: "Tugas tidak ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain.",
  draft: "Draf tidak ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain.",
  conversation:
    "Obrolan tidak ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain.",
  agent: "Bolu tidak ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain.",
};

/**
 * The sentence for a failure that repeating the read could fix.
 *
 * It says what happened and stops there: every screen that shows it also shows a
 * "Coba lagi" button next to it, and a sentence ending in the same words printed
 * "Coba lagi.Coba lagi" on the chat banner. The sentence explains, the button acts.
 */
const RETRY: Record<DeepLinkKind, string> = {
  task: "Tugas gagal dimuat — server atau jaringannya sedang bermasalah.",
  draft: "Draf gagal dimuat — server atau jaringannya sedang bermasalah.",
  conversation: "Obrolan gagal dimuat — server atau jaringannya sedang bermasalah.",
  agent: "Bolu gagal dimuat — server atau jaringannya sedang bermasalah.",
};

/**
 * The sentence for a conversation the workspace's list does not hold.
 *
 * The chat screen finds a conversation by looking it up in that list rather than
 * by asking the API, so a missing id never arrives as a 404 to map. It shares the
 * sentence with the 404 case instead of writing a second one that could drift.
 */
export const CONVERSATION_NOT_FOUND = NOT_FOUND.conversation;

/**
 * The API's message is only shown when it reads like prose rather than a status
 * label: a screen that printed `task id is not a uuid` is the bug this module
 * exists to fix. The patterns cover the refusals these endpoints actually send;
 * anything with snake_case or a `key: value` shape is treated the same way.
 */
const TECHNICAL_PATTERNS = [
  /not a uuid/i,
  /not found/i,
  /internal server error/i,
  /bad request/i,
  /unauthorized/i,
  /forbidden/i,
  /\binvalid\b/i,
  /:\s/,
  /\b[a-z]+_[a-z_]+\b/,
];

function looksTechnical(message: string): boolean {
  const text = message.trim();
  return text === "" || TECHNICAL_PATTERNS.some((pattern) => pattern.test(text));
}

/**
 * friendlyLoadFailure maps a failed single-record read to the sentence the page
 * shows.
 *
 * A 400 (or any `invalid_*` code) is the API saying the segment is not an id at
 * all, and a 404 is it saying the id is fine but the record is not there. Both
 * are permanent for that link, so neither offers a retry; everything else is the
 * server or the network, where the same request can succeed a moment later.
 */
export function friendlyLoadFailure(error: ApiError, kind: DeepLinkKind): LoadFailure {
  if (error.status === 400 || error.code?.toLowerCase().includes("invalid")) {
    return { message: INVALID_LINK[kind], retryable: false };
  }

  if (error.status === 404) {
    return { message: NOT_FOUND[kind], retryable: false };
  }

  // Some client-written messages already end in the same invitation the banner's
  // button carries (`Server terlalu lama menjawab. Coba lagi.`); appending that
  // verbatim would print "Coba lagi.Coba lagi" again, so the trailing invitation
  // is dropped before the rest is shown.
  const detail = error.message.trim().replace(/\s*coba lagi\.?$/i, "").trim();
  return {
    // Last resort: an unexpected failure whose own sentence is readable is better
    // than a generic one, but a technical one is replaced rather than shown.
    message: looksTechnical(detail) ? RETRY[kind] : `${RETRY[kind]} ${detail}`,
    retryable: true,
  };
}