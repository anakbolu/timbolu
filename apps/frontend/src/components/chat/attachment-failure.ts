/**
 * Which staged files a refused send is about, and nothing else.
 *
 * The composer keeps a chip for every file it is holding, so a refusal must
 * name the chips that are actually wrong. A refusal about quota, permissions,
 * or the server says nothing about the files: the message belongs in the
 * composer's error line while every chip stays `uploaded`, otherwise a 429 or a
 * 500 would strand files that the next send could still use.
 */

import type { ApiError } from "@/lib/api";

/** The codes that mean the file itself is the problem, so its chip is what fails. */
const ATTACHMENT_CODES: Record<string, true> = {
  invalid_attachment_id: true,
  attachment_not_found: true,
  attachment_already_bound: true,
};

/**
 * blamedAttachmentIds names the attachment chips a refused send marks as failed.
 *
 * Only `invalid_attachment_id` carries an id, quoted in the message because it
 * is the id the API could not read. `attachment_not_found` and
 * `attachment_already_bound` are reported without naming one: the API checked
 * the ids as a set and found a bad one among them, so pinning the failure on
 * every chip would be the same bug as a quota error killing the composer.
 */
export function blamedAttachmentIds(error: ApiError): string[] {
  if (!error.code || !ATTACHMENT_CODES[error.code]) {
    return [];
  }
  if (error.code !== "invalid_attachment_id") {
    return [];
  }
  const quoted = /"([^"]+)"/.exec(error.message);
  return quoted ? [quoted[1]] : [];
}
