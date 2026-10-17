/**
 * Turns the files a message carries into the text — and the images — a model
 * reads.
 *
 * Ported from apps/backend/internal/platform/attachtext/attachtext.go. Three
 * rules run through the package:
 *
 *   - one bad file never fails a reply; every failure becomes a note in the
 *     prompt, so the model is told the file exists and could not be read;
 *   - the budget is spent on the newest files first;
 *   - what a file costs is bounded twice: per file, and across the request.
 */

import { randomBytes } from "node:crypto";
import { extname } from "node:path";
import { extractPDF } from "./pdf";

/** The largest file that is fetched at all. */
export const DefaultMaxFileBytes = 10 << 20;
/** Bounds one file's text. */
export const DefaultMaxCharsPerFile = 20_000;
/** Bounds the text of every attachment of one request. */
export const DefaultMaxCharsTotal = 60_000;
/** Bounds how many pictures one request carries. */
export const DefaultMaxImages = 4;
/** The largest picture sent to a vision model. */
export const DefaultMaxImageBytes = 5 << 20;

/** Bounds the reason quoted in a note. */
const reasonMaxRunes = 200;
/** Sits between two attachments' contributions to one message. */
const partSeparator = "\n\n";
/** The width of a boundary nonce: eight hex characters. */
const nonceBytes = 4;

/** The reason a file was never fetched. */
export const ErrTooLarge = new Error("berkas lebih besar dari batas baca");

/** Bounds what attachments contribute to one request. */
export interface Limits {
  /** The largest file fetched. A bigger one is not downloaded at all. */
  maxFileBytes?: number;
  maxCharsPerFile?: number;
  maxCharsTotal?: number;
  maxImages?: number;
  maxImageBytes?: number;
}

interface ResolvedLimits {
  maxFileBytes: number;
  maxCharsPerFile: number;
  maxCharsTotal: number;
  maxImages: number;
  maxImageBytes: number;
}

/** Fills every unset field from the defaults. */
export function normalise(limits: Limits): ResolvedLimits {
  return {
    maxFileBytes: limits.maxFileBytes && limits.maxFileBytes > 0 ? limits.maxFileBytes : DefaultMaxFileBytes,
    maxCharsPerFile:
      limits.maxCharsPerFile && limits.maxCharsPerFile > 0 ? limits.maxCharsPerFile : DefaultMaxCharsPerFile,
    maxCharsTotal:
      limits.maxCharsTotal && limits.maxCharsTotal > 0 ? limits.maxCharsTotal : DefaultMaxCharsTotal,
    maxImages: limits.maxImages && limits.maxImages > 0 ? limits.maxImages : DefaultMaxImages,
    maxImageBytes:
      limits.maxImageBytes && limits.maxImageBytes > 0 ? limits.maxImageBytes : DefaultMaxImageBytes,
  };
}

/** True when a file is too large to fetch, asked before reading the object. */
export function oversized(limits: Limits, byteSize: number): boolean {
  return byteSize > normalise(limits).maxFileBytes;
}

/** One attachment: what is known about it without reading it, and its bytes. */
export interface File {
  filename: string;
  contentType: string;
  byteSize: number;
  /** Empty when `err` is set, and also for an empty file. */
  content: Uint8Array;
  /** Why the bytes are absent: ErrTooLarge, or whatever storage reported. */
  err: Error | null;
}

/** A picture that travels to the model as an image rather than as text. */
export interface Image {
  mediaType: string;
  /** Base64 without a `data:` prefix. */
  data: string;
}

/** One attachment's contribution to a message. */
export interface Part {
  text: string;
  /** Replaces `text` when the active chain cannot see pictures. */
  noVision: string;
  image: Image | null;
  /** Why a file that was meant to be read was not. */
  err: Error | null;
}

/** What the attachments of one message contribute to it. */
export class Turn {
  private readonly parts: Part[] = [];

  /** Appends a rendered attachment; an empty text contributes nothing. */
  add(part: Part): void {
    if (part.text === "") return;
    this.parts.push(part);
  }

  /** True when the turn contributes nothing. */
  empty(): boolean {
    return this.parts.length === 0;
  }

  /** Renders the turn. Without vision every picture is replaced by its note. */
  text(vision: boolean): string {
    if (this.parts.length === 0) return "";
    return this.parts
      .map((part) => (!vision && part.noVision !== "" ? part.noVision : part.text))
      .join(partSeparator);
  }

  /** The pictures the turn carries, in the order they were added. */
  images(): Image[] {
    const images: Image[] = [];
    for (const part of this.parts) {
      if (part.image) images.push(part.image);
    }
    return images;
  }
}

/**
 * Renders the attachments of one request, spending that request's budget as it
 * goes. One renderer per request, called once per attachment, newest first.
 */
export class Renderer {
  private readonly limits: ResolvedLimits;
  private chars = 0;
  private images = 0;

  constructor(limits: Limits = {}) {
    this.limits = normalise(limits);
  }

  /** Turns one attachment into the part it contributes. Never throws. */
  render(file: File): Part {
    if (file.err) {
      return this.unreadable(file, reasonOf(file.err));
    }

    // The stored type is a hint: the bytes decide whether this is a picture.
    const mediaType = sniffImage(file.content);
    if (mediaType !== "") {
      return this.image(file, mediaType);
    }

    // A marker shared by every file of a request is one marker a file could
    // contain, so the boundary is drawn per file.
    const nonce = boundaryNonce();

    if (isPDF(file)) {
      try {
        const text = extractPDF(file.content);
        return this.document(file, nonce, text, null);
      } catch (err) {
        return this.document(file, nonce, "", err instanceof Error ? err : new Error(String(err)));
      }
    }
    if (isText(file)) {
      return this.document(file, nonce, textOf(file.content), null);
    }
    return this.note(file, "isi tidak dibaca — tipe ini belum didukung");
  }

  private image(file: File, mediaType: string): Part {
    if (file.content.length > this.limits.maxImageBytes) {
      return this.note(
        file,
        `isi tidak dibaca — gambar lebih besar dari batas ${sizeLabel(this.limits.maxImageBytes)}`,
      );
    }
    if (this.images >= this.limits.maxImages) {
      return this.note(file, "isi tidak dibaca — batas jumlah gambar tercapai");
    }

    this.images++;
    const name = sanitiseName(file.filename);
    const text = `[Gambar ${name} (${mediaType}, ${sizeLabel(file.byteSize)})]`;
    this.chars += runeCount(text);

    return {
      text,
      noVision: `[Gambar ${name} tidak bisa dibaca: model yang aktif tidak mendukung gambar]`,
      image: { mediaType, data: Buffer.from(file.content).toString("base64") },
      err: null,
    };
  }

  /**
   * Renders a file whose content is text, whatever the source: a text file, or
   * the text extracted from a PDF.
   */
  private document(file: File, nonce: string, text: string, err: Error | null): Part {
    if (err) {
      return this.unreadable(file, reasonOf(err));
    }
    if (isPDF(file) && text.trim() === "") {
      return this.note(file, "PDF tanpa teks yang bisa diekstrak");
    }

    const truncated = truncate(text, this.limits.maxCharsPerFile);
    const rendered = section(file, nonce, truncated.text, truncated.marker);

    // The total budget is a hard bound: a file whose text does not fit in what
    // is left contributes its note and nothing else.
    if (runeCount(rendered) > this.remaining()) {
      return this.note(file, "isi tidak dibaca — batas total konteks lampiran tercapai");
    }

    this.chars += runeCount(rendered);
    return { text: rendered, noVision: "", image: null, err: null };
  }

  /** The metadata-only line for a file whose content is deliberately not included. */
  private note(file: File, reason: string): Part {
    const text = `[Lampiran ${sanitiseName(file.filename)} (${mimeOf(file)}, ${sizeLabel(
      file.byteSize,
    )}): ${reason}]`;
    this.chars += runeCount(text);
    return { text, noVision: "", image: null, err: null };
  }

  /** The note for a file that was meant to be read and was not. */
  private unreadable(file: File, reason: string): Part {
    const text = `[Lampiran ${sanitiseName(file.filename)} tidak bisa dibaca: ${reason}]`;
    this.chars += runeCount(text);
    return { text, noVision: "", image: null, err: new Error(reason) };
  }

  private remaining(): number {
    return this.limits.maxCharsTotal - this.chars;
  }
}

/**
 * Counts code points, which is what Go's `utf8.RuneCountInString` counts —
 * not UTF-16 code units.
 */
export function runeCount(value: string): number {
  return Array.from(value).length;
}

/** A file's own text must not be able to spell either marker tag. */
const openTagPattern = /\[Lampiran/gi;
const closeTagPattern = /\[\/Lampiran/gi;

/** Removes what would let a filename break the marker it is quoted in. */
function sanitiseName(name: string): string {
  return name.replace(/[\r\n\t\[\]]/g, "").trim();
}

/** Makes a file's text unable to pass for the markers around it. */
export function neutralise(text: string, nonce: string): string {
  // The closing tag is broken first and the opening one second, so the
  // replacement of the first cannot be read as the second.
  const closed = text.replace(closeTagPattern, "[/ Lampiran");
  const opened = closed.replace(openTagPattern, "[ Lampiran");
  return opened.split(nonce).join("#");
}

/** Wraps content in the markers that tell the model where the file begins and ends. */
function section(file: File, nonce: string, text: string, marker: string): string {
  const body = marker !== "" ? `${text}\n${marker}` : text;
  return `[Lampiran: ${sanitiseName(file.filename)} (${mimeOf(file)}, ${sizeLabel(file.byteSize)}) #${nonce}]\n${neutralise(
    body,
    nonce,
  )}\n[/Lampiran #${nonce}]`;
}

/** Keeps at most `limit` characters and returns the marker that says what was dropped. */
export function truncate(text: string, limit: number): { text: string; marker: string } {
  const chars = Array.from(text);
  if (chars.length <= limit) {
    return { text, marker: "" };
  }
  return {
    text: chars.slice(0, limit).join(""),
    marker: truncationMarker(limit, chars.length),
  };
}

/** The line a truncated file carries, so the model knows it reads an excerpt. */
function truncationMarker(shown: number, total: number): string {
  return `[... terpotong: ${shown} dari ${total} karakter ditampilkan]`;
}

/** Reads bytes as text, replacing anything that is not valid UTF-8. */
export function textOf(content: Uint8Array): string {
  return new TextDecoder("utf-8", { fatal: false }).decode(content);
}

/** Whether a file is one of the text-like types the model reads directly. */
export function isText(file: File): boolean {
  switch (mimeOf(file)) {
    case "text/plain":
    case "text/csv":
    case "text/markdown":
    case "application/json":
      return true;
  }

  switch (extension(file.filename)) {
    case ".txt":
    case ".csv":
    case ".md":
    case ".json":
    case ".log":
    case ".tsv":
      return true;
  }
  return false;
}

/** Whether a file is a PDF, by its type or by its extension. */
export function isPDF(file: File): boolean {
  return mimeOf(file) === "application/pdf" || extension(file.filename) === ".pdf";
}

/** Which of the four supported picture types the bytes are, or "". */
export function sniffImage(content: Uint8Array): string {
  if (content.length === 0) return "";

  const head = content.subarray(0, 12);
  if (
    head.length >= 8 &&
    head[0] === 0x89 &&
    head[1] === 0x50 &&
    head[2] === 0x4e &&
    head[3] === 0x47 &&
    head[4] === 0x0d &&
    head[5] === 0x0a &&
    head[6] === 0x1a &&
    head[7] === 0x0a
  ) {
    return "image/png";
  }
  if (head.length >= 3 && head[0] === 0xff && head[1] === 0xd8 && head[2] === 0xff) {
    return "image/jpeg";
  }
  if (head.length >= 6) {
    const signature = Buffer.from(head.subarray(0, 6)).toString("latin1");
    if (signature === "GIF87a" || signature === "GIF89a") {
      return "image/gif";
    }
  }
  if (
    head.length >= 12 &&
    Buffer.from(head.subarray(0, 4)).toString("latin1") === "RIFF" &&
    Buffer.from(head.subarray(8, 12)).toString("latin1") === "WEBP"
  ) {
    return "image/webp";
  }
  return "";
}

/** The type a section header shows: the stored one without its parameters. */
export function mimeOf(file: File): string {
  const base = baseMime(file.contentType);
  return base !== "" ? base : "application/octet-stream";
}

/** Strips the parameters of a content type. */
export function baseMime(contentType: string): string {
  const cut = contentType.indexOf(";");
  const base = cut >= 0 ? contentType.slice(0, cut) : contentType;
  return base.trim().toLowerCase();
}

/** The lower-case extension of a filename, including the dot. */
function extension(filename: string): string {
  return extname(filename.trim()).toLowerCase();
}

/** Renders why a file could not be read, bounded and on one line. */
export function reasonOf(err: Error): string {
  const reason = err.message.split(/\s+/).filter(Boolean).join(" ");
  const chars = Array.from(reason);
  return chars.length > reasonMaxRunes ? `${chars.slice(0, reasonMaxRunes).join("")}…` : reason;
}

/** Renders a byte count the way a person reads it. */
export function sizeLabel(byteSize: number): string {
  if (byteSize < 1024) {
    return `${byteSize} B`;
  }
  if (byteSize < 1 << 20) {
    return `${(byteSize / 1024).toFixed(1)} KB`;
  }
  return `${(byteSize / (1 << 20)).toFixed(1)} MB`;
}

/** Returns the marker that ends one file's section. */
function boundaryNonce(): string {
  return randomBytes(nonceBytes).toString("hex");
}
