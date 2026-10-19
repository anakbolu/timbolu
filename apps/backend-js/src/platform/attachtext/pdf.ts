/**
 * Reads the text layer of a PDF.
 *
 * Ported from apps/backend/internal/platform/attachtext/pdf.go, which used
 * `ledongthuc/pdf`. That library has no JS equivalent, so this is a minimal
 * extractor: it inflates the content streams and reads the text-showing
 * operators out of them.
 *
 * Limits, stated so they are not mistaken for completeness:
 *   - compressed streams are decoded with FlateDecode (and raw deflate); other
 *     filters (LZW, ASCII85, RunLength, DCT) are skipped;
 *   - text is decoded as Latin-1 or UTF-16BE (BOM), not through a ToUnicode
 *     CMap, so a PDF whose fonts use a custom encoding may produce wrong
 *     characters;
 *   - no attempt is made to reconstruct layout, kerning or reading order.
 */

import { inflateRawSync, inflateSync } from "node:zlib";

/** Bounds one extraction. */
export const pdfTimeoutMs = 5_000;
/** Bounds what is read out of one PDF before the per-file limit is applied. */
export const pdfMaxTextBytes = 4 << 20;

/** The reason a PDF that took too long is reported with. */
export const errPDFTimeout = new Error("ekstraksi PDF melebihi batas waktu");

/** Reads the text layer of a PDF. Never panics; bounded by `pdfTimeoutMs`. */
export function extractPDF(content: Uint8Array): string {
  if (content.length === 0) {
    return "";
  }

  const deadline = Date.now() + pdfTimeoutMs;
  try {
    return readPDF(content, deadline);
  } catch (err) {
    if (err === errPDFTimeout) {
      throw err;
    }
    const detail = err instanceof Error ? err.message : String(err);
    throw new Error(`PDF tidak bisa dibaca: ${detail}`);
  }
}

/** Does the extraction, without any of the guards. */
function readPDF(content: Uint8Array, deadline: number): string {
  const raw = Buffer.from(content);
  const header = raw.subarray(0, 1024).toString("latin1");
  if (!header.includes("%PDF-")) {
    throw new Error("bukan berkas PDF");
  }

  const source = raw.toString("latin1");
  const pieces: string[] = [];

  let offset = 0;
  while (offset < source.length) {
    if (Date.now() > deadline) throw errPDFTimeout;

    const start = findKeyword(source, "stream", offset);
    if (start < 0) break;

    const dictionary = source.slice(Math.max(0, start - 2048), start);
    let bodyStart = start + "stream".length;
    // The keyword is followed by CRLF or LF, never by other whitespace.
    if (source.startsWith("\r\n", bodyStart)) bodyStart += 2;
    else if (source[bodyStart] === "\n") bodyStart += 1;

    const end = source.indexOf("endstream", bodyStart);
    if (end < 0) break;

    let body: Buffer | null = raw.subarray(bodyStart, end);
    const filter = /\/Filter\s*(\[[^\]]*\]|\/[A-Za-z0-9]+)/.exec(dictionary)?.[1] ?? "";

    if (/FlateDecode/.test(filter)) {
      body = inflate(body);
    } else if (filter !== "") {
      // An unsupported filter: skip this stream rather than emitting garbage.
      offset = end + "endstream".length;
      continue;
    }

    if (body) {
      const text = extractTextFromContent(body.toString("latin1"), deadline);
      if (text.trim() !== "") pieces.push(text);
    }

    offset = end + "endstream".length;
  }

  const joined = pieces.join("\n");
  return joined.length > pdfMaxTextBytes ? joined.slice(0, pdfMaxTextBytes) : joined;
}

/** Finds `keyword` at or after `from`, skipping the `end`-prefixed spelling. */
function findKeyword(source: string, keyword: string, from: number): number {
  let index = source.indexOf(keyword, from);
  while (index >= 0 && source.startsWith("end", index - 3)) {
    index = source.indexOf(keyword, index + keyword.length);
  }
  return index;
}

/** Inflates a stream, tolerating the missing-zlib-header form some writers emit. */
function inflate(body: Buffer): Buffer | null {
  try {
    return inflateSync(body);
  } catch {
    try {
      return inflateRawSync(body);
    } catch {
      return null;
    }
  }
}

/** Pulls the operands of Tj/TJ/'/" out of one content stream. */
function extractTextFromContent(content: string, deadline: number): string {
  const out: string[] = [];
  let i = 0;
  let lastString = "";
  let pending: Array<string | number> = [];

  const emit = (value: string) => {
    if (value === "") return;
    out.push(value);
    lastString = value;
  };

  while (i < content.length) {
    if ((i & 0x3fff) === 0 && Date.now() > deadline) throw errPDFTimeout;

    const ch = content[i];

    if (ch === "(") {
      const parsed = readLiteralString(content, i);
      if (!parsed) break;
      pending.push(parsed.value);
      i = parsed.next;
      continue;
    }

    if (ch === "<" && content[i + 1] !== "<") {
      const close = content.indexOf(">", i + 1);
      if (close < 0) break;
      const hex = content.slice(i + 1, close).replace(/\s+/g, "");
      if (/^[0-9a-fA-F]*$/.test(hex)) {
        pending.push(decodeText(Buffer.from(hex.length % 2 === 0 ? hex : `${hex}0`, "hex")));
      }
      i = close + 1;
      continue;
    }

    if (ch === "[") {
      // A TJ array: strings and kerning numbers, up to the closing bracket.
      const close = content.indexOf("]", i + 1);
      if (close < 0) break;
      pending = parseArray(content.slice(i + 1, close));
      i = close + 1;
      continue;
    }

    if (ch === "<" && content[i + 1] === "<") {
      // A nested dictionary inside the content stream: skip it.
      const close = content.indexOf(">>", i + 2);
      if (close < 0) break;
      i = close + 2;
      continue;
    }

    if (/[A-Za-z'"*]/.test(ch)) {
      let end = i + 1;
      while (end < content.length && /[A-Za-z0-9'"*]/.test(content[end])) end++;
      const operator = content.slice(i, end);
      i = end;

      switch (operator) {
        case "Tj":
          emit(pending.length > 0 ? String(pending[pending.length - 1]) : "");
          pending = [];
          break;
        case "TJ": {
          let line = "";
          for (const item of pending) {
            if (typeof item === "string") line += item;
            else if (item <= -100) line += " ";
          }
          emit(line);
          pending = [];
          break;
        }
        case "'":
        case '"':
          emit(`${pending.length > 0 ? String(pending[pending.length - 1]) : ""}\n`);
          pending = [];
          break;
        case "T*":
        case "Td":
        case "TD":
        case "Tm":
        case "ET":
          if (out.length > 0 && !out[out.length - 1].endsWith("\n")) out.push("\n");
          pending = [];
          break;
        default:
          pending = [];
      }
      continue;
    }

    i++;
  }

  // Collapse the runs of whitespace the operators leave behind.
  return out
    .join("")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

/** Reads a `(...)` literal string with escapes and balanced parentheses. */
function readLiteralString(
  content: string,
  start: number,
): { value: string; next: number } | null {
  const bytes: number[] = [];
  let depth = 1;
  let i = start + 1;

  while (i < content.length) {
    const ch = content[i];

    if (ch === "\\") {
      const next = content[i + 1];
      switch (next) {
        case "n":
          bytes.push(0x0a);
          i += 2;
          continue;
        case "r":
          bytes.push(0x0d);
          i += 2;
          continue;
        case "t":
          bytes.push(0x09);
          i += 2;
          continue;
        case "b":
          bytes.push(0x08);
          i += 2;
          continue;
        case "f":
          bytes.push(0x0c);
          i += 2;
          continue;
        case "(":
        case ")":
        case "\\":
          bytes.push(next.charCodeAt(0));
          i += 2;
          continue;
        case "\r":
          i += content[i + 2] === "\n" ? 3 : 2;
          continue;
        case "\n":
          i += 2;
          continue;
        default: {
          const octal = /^[0-7]{1,3}/.exec(content.slice(i + 1, i + 4));
          if (octal) {
            bytes.push(Number.parseInt(octal[0], 8) & 0xff);
            i += 1 + octal[0].length;
            continue;
          }
          bytes.push(next ? next.charCodeAt(0) : 0x5c);
          i += 2;
          continue;
        }
      }
    }

    if (ch === "(") {
      depth++;
      bytes.push(0x28);
      i++;
      continue;
    }
    if (ch === ")") {
      depth--;
      i++;
      if (depth === 0) {
        return { value: decodeText(Buffer.from(bytes)), next: i };
      }
      bytes.push(0x29);
      continue;
    }

    bytes.push(ch.charCodeAt(0) & 0xff);
    i++;
  }

  return null;
}

/** Parses the body of a TJ array into strings and kerning numbers. */
function parseArray(body: string): Array<string | number> {
  const items: Array<string | number> = [];
  let i = 0;

  while (i < body.length) {
    const ch = body[i];

    if (ch === "(") {
      const parsed = readLiteralString(body, i);
      if (!parsed) break;
      items.push(parsed.value);
      i = parsed.next;
      continue;
    }
    if (ch === "<") {
      const close = body.indexOf(">", i + 1);
      if (close < 0) break;
      const hex = body.slice(i + 1, close).replace(/\s+/g, "");
      if (/^[0-9a-fA-F]*$/.test(hex)) {
        items.push(decodeText(Buffer.from(hex.length % 2 === 0 ? hex : `${hex}0`, "hex")));
      }
      i = close + 1;
      continue;
    }
    const number = /^-?\d+(?:\.\d+)?/.exec(body.slice(i));
    if (number) {
      items.push(Number.parseFloat(number[0]));
      i += number[0].length;
      continue;
    }
    i++;
  }

  return items;
}

/** Decodes a PDF string: UTF-16BE when it carries a BOM, Latin-1 otherwise. */
function decodeText(bytes: Buffer): string {
  if (bytes.length >= 2 && bytes[0] === 0xfe && bytes[1] === 0xff) {
    return bytes.subarray(2).swap16().toString("utf16le");
  }
  return bytes.toString("latin1");
}
