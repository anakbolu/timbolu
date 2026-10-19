/**
 * What an upload is allowed to be, and what it is stored as.
 *
 * The extension and the bytes are checked together because either alone is a
 * hole. An extension is what a client types, so `invoice.exe` renamed to
 * `invoice.png` would pass on the name; the sniffed type is what the bytes are,
 * so a text file named `logo.png` would pass on the content. A file is accepted
 * only when both agree, and what is stored is the type derived from the
 * extension — never the header the client sent, which is a claim and not a fact.
 *
 * Ported 1:1 from internal/modules/chat/service/attachment_type.go.
 */

import { extname } from "node:path";

import type { Logger } from "../../../platform/logger.ts";
import { ErrFileTypeNotAllowed, WrappedError } from "../domain/conversation.ts";

/** One extension an upload may carry: what it is stored as, and what it may hold. */
interface AttachmentType {
  contentType: string;
  sniffed: string[];
}

/**
 * What an OLE compound file may sniff as. Go's sniffer answers a bare octet
 * stream for it; the other two are what other sniffers name the same container.
 */
const legacyOffice = ["application/octet-stream", "application/x-ole-storage", "application/vnd.ms-office"];

/**
 * The accepted extensions and what each may hold.
 *
 * The office formats are the interesting entries: `docx` and its two siblings
 * are zip containers, and the legacy `doc`/`xls`/`ppt` are OLE compound files,
 * which Go's sniffer reports as a bare octet stream. Accepting the container is
 * the most a sniffer can say about them without parsing the archive.
 */
const knownAttachmentTypes: Record<string, AttachmentType> = {
  png: { contentType: "image/png", sniffed: ["image/png"] },
  jpg: { contentType: "image/jpeg", sniffed: ["image/jpeg"] },
  jpeg: { contentType: "image/jpeg", sniffed: ["image/jpeg"] },
  gif: { contentType: "image/gif", sniffed: ["image/gif"] },
  webp: { contentType: "image/webp", sniffed: ["image/webp"] },
  pdf: { contentType: "application/pdf", sniffed: ["application/pdf"] },
  txt: { contentType: "text/plain", sniffed: ["text/plain"] },
  csv: { contentType: "text/csv", sniffed: ["text/plain"] },
  md: { contentType: "text/markdown", sniffed: ["text/plain"] },
  docx: {
    contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    sniffed: ["application/zip"],
  },
  xlsx: {
    contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    sniffed: ["application/zip"],
  },
  pptx: {
    contentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    sniffed: ["application/zip"],
  },
  doc: { contentType: "application/msword", sniffed: legacyOffice },
  xls: { contentType: "application/vnd.ms-excel", sniffed: legacyOffice },
  ppt: { contentType: "application/vnd.ms-powerpoint", sniffed: legacyOffice },
};

/** The allowlist of a deployment that configured none. */
const defaultAttachmentExtensions = [
  "png",
  "jpg",
  "jpeg",
  "gif",
  "webp",
  "pdf",
  "txt",
  "csv",
  "md",
  "docx",
  "xlsx",
  "pptx",
  "doc",
  "xls",
  "ppt",
];

/**
 * Refused whatever the configuration says. An executable uploaded to a chat is
 * a payload handed to whoever opens it, so no configuration can turn it on.
 */
const forbiddenExtensions: Record<string, true> = {
  exe: true,
  dll: true,
  com: true,
  bat: true,
  cmd: true,
  msi: true,
  scr: true,
  ps1: true,
  sh: true,
  jar: true,
  apk: true,
  app: true,
  elf: true,
  bin: true,
};

/**
 * The leading bytes of a program, whatever it is named.
 *
 * The sniffed type is not enough for these: a sniffer reports a bare octet
 * stream for a PE, an ELF, and a Mach-O image alike, and a shebang script sniffs
 * as plain text. Reading the magic is what makes "executables are refused" a
 * rule about the bytes rather than about the name.
 */
const executableMagics: number[][] = [
  [0x4d, 0x5a], // PE
  [0x7f, 0x45, 0x4c, 0x46], // ELF
  [0xfe, 0xed, 0xfa, 0xce], // Mach-O, 32-bit big endian
  [0xce, 0xfa, 0xed, 0xfe], // Mach-O, 32-bit little endian
  [0xfe, 0xed, 0xfa, 0xcf], // Mach-O, 64-bit big endian
  [0xcf, 0xfa, 0xed, 0xfe], // Mach-O, 64-bit little endian
  [0xca, 0xfe, 0xba, 0xbe], // universal binary, and a Java class
  [0x23, 0x21], // shebang
];

/**
 * Checks one upload against the allowlist and answers the type it is stored as.
 *
 * It is deliberately the first thing a store does: a refused upload must not
 * open a message, write an object, or publish an event, so nothing may happen
 * before this returns.
 */
export function validateAttachment(
  filename: string,
  content: Uint8Array,
  allowed: string[],
): string {
  if (executableContent(content)) {
    throw new WrappedError(
      ErrFileTypeNotAllowed,
      "the content is an executable, which is never accepted",
    );
  }

  const extension = normalizeExtension(extname(filename.trim()));
  if (extension === "") {
    throw new WrappedError(
      ErrFileTypeNotAllowed,
      `the file needs an extension; allowed: ${allowed.join(", ")}`,
    );
  }
  if (forbiddenExtensions[extension] === true) {
    throw new WrappedError(
      ErrFileTypeNotAllowed,
      `.${extension} is an executable, which is never accepted`,
    );
  }
  if (!allowed.includes(extension)) {
    throw new WrappedError(
      ErrFileTypeNotAllowed,
      `.${extension} is not accepted; allowed: ${allowed.join(", ")}`,
    );
  }

  const known = knownAttachmentTypes[extension];
  if (!known) {
    // Unreachable through the allowlist, which only ever names a known
    // extension, and kept so a caller that hands this function its own list
    // cannot skip the content check.
    throw new WrappedError(
      ErrFileTypeNotAllowed,
      `.${extension} is not a type this service knows`,
    );
  }
  if (!sniffMatches(detectContentType(content), known.sniffed)) {
    throw new WrappedError(
      ErrFileTypeNotAllowed,
      `the content of .${extension} is not a ${known.contentType} file`,
    );
  }

  return known.contentType;
}

/** Reports whether content opens with the bytes of a program. */
function executableContent(content: Uint8Array): boolean {
  for (const magic of executableMagics) {
    if (content.length < magic.length) {
      continue;
    }
    if (magic.every((byte, index) => content[index] === byte)) {
      return true;
    }
  }
  return false;
}

/**
 * Compares a sniffed type against the ones an extension may hold. The
 * parameters are dropped first: a sniffer answers `text/plain; charset=utf-8`,
 * and the charset is not part of the type a caller accepts.
 */
function sniffMatches(sniffed: string, accepted: string[]): boolean {
  const semicolon = sniffed.indexOf(";");
  const base = (semicolon >= 0 ? sniffed.slice(0, semicolon) : sniffed).trim();
  return accepted.includes(base);
}

/**
 * Reduces an extension to the one spelling the tables use: no leading dot, no
 * case. `PNG`, `.png`, and `png` all name the same type.
 */
function normalizeExtension(extension: string): string {
  return extension.trim().toLowerCase().replace(/^\./, "");
}

/**
 * Turns what a deployment configured into the list the validator uses.
 *
 * The configuration may only narrow what the service knows: an extension
 * outside the table above has no content check to apply, so naming one would be
 * a way to accept any bytes at all, and naming an executable is refused
 * outright. Both are ignored with a warning rather than failing the start-up.
 */
export function allowedExtensions(configured: string[], log: Logger | null): string[] {
  if (configured.length === 0) {
    return defaultAttachmentExtensions;
  }

  const allowed: string[] = [];
  const seen = new Set<string>();
  for (const raw of configured) {
    const extension = normalizeExtension(raw);
    if (extension === "" || seen.has(extension)) {
      continue;
    }
    if (forbiddenExtensions[extension] === true) {
      log?.warn({ extension, reason: "it is an executable" }, "ignoring a configured attachment extension");
    } else if (!knownAttachmentTypes[extension]) {
      log?.warn(
        { extension, reason: "it is not a type this service knows" },
        "ignoring a configured attachment extension",
      );
    } else {
      seen.add(extension);
      allowed.push(extension);
    }
  }

  if (allowed.length === 0) {
    log?.warn({ configured }, "no configured attachment extension is usable: using the default list");
    return defaultAttachmentExtensions;
  }

  return allowed;
}

/**
 * The content type of a byte sequence, the way Go's `http.DetectContentType`
 * answers: a magic-number check for the formats this service accepts, and
 * `text/plain` for bytes that read as text.
 *
 * Only the types the allowlist above names are ever compared, so the table is
 * limited to them plus the generic answers a sniffer gives.
 */
export function detectContentType(content: Uint8Array): string {
  const head = content.subarray(0, 512);

  if (startsWith(head, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) return "image/png";
  if (startsWith(head, [0xff, 0xd8, 0xff])) return "image/jpeg";
  if (startsWith(head, [0x47, 0x49, 0x46, 0x38])) return "image/gif";
  if (startsWith(head, [0x52, 0x49, 0x46, 0x46]) && ascii(head, 8, 12) === "WEBP") return "image/webp";
  if (ascii(head, 0, 5) === "%PDF-") return "application/pdf";
  if (startsWith(head, [0x50, 0x4b, 0x03, 0x04]) || startsWith(head, [0x50, 0x4b, 0x05, 0x06])) {
    return "application/zip";
  }
  // OLE compound file, which is what the legacy office formats are.
  if (startsWith(head, [0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1])) {
    return "application/octet-stream";
  }

  if (isPlainText(head)) {
    return "text/plain";
  }
  return "application/octet-stream";
}

/** True when the bytes begin with a fixed prefix. */
function startsWith(content: Uint8Array, prefix: number[]): boolean {
  if (content.length < prefix.length) {
    return false;
  }
  return prefix.every((byte, index) => content[index] === byte);
}

/** The ASCII of a byte range, which is how a container names its own format. */
function ascii(content: Uint8Array, from: number, to: number): string {
  if (content.length < to) {
    return "";
  }
  return String.fromCharCode(...content.subarray(from, to));
}

/** Reports whether the bytes read as text: printable, with the usual whitespace. */
function isPlainText(content: Uint8Array): boolean {
  if (content.length === 0) {
    return true;
  }
  for (const byte of content) {
    if (byte === 0x00) {
      return false;
    }
    if (byte < 0x09 || (byte > 0x0d && byte < 0x20) || byte === 0x7f) {
      return false;
    }
  }
  return true;
}
