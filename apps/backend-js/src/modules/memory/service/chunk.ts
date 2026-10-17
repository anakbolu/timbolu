/**
 * Splitting a document into knowledge chunks.
 *
 * Every length here is in **runes**, which Go counts as Unicode code points.
 * `String.length` counts UTF-16 code units and would split a surrogate pair —
 * half an emoji stored as a chunk — so the text is always cut through
 * `Array.from`, and the same rule applies to the trims: Go's `unicode.IsSpace`
 * is a different set from JavaScript's `trim()`.
 *
 * Ported 1:1 from internal/modules/memory/service/chunk.go.
 */

/** Bounds one upload, so a paste of a whole book cannot fill the table and the bill in one request. */
export const maxDocumentRunes = 200_000;

/** Bounds how many pieces one upload becomes, for the same reason. */
export const maxChunks = 200;

/**
 * The characters Go's `unicode.IsSpace` accepts: `\t`, `\n`, `\v`, `\f`, `\r`,
 * space, U+0085, U+00A0, and the Unicode Zs category.
 *
 * `\s` is close but not equal — it also accepts U+2028, U+2029 and U+FEFF — and
 * a chunk boundary that disagrees with the Go side by one character is still a
 * boundary in a different place.
 */
const GO_SPACE =
  "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u202f\u205f\u3000";

function isGoSpace(ch: string): boolean {
  return GO_SPACE.includes(ch);
}

/** Go's `strings.TrimSpace`. */
export function trimSpace(text: string): string {
  const runes = Array.from(text);
  let start = 0;
  let end = runes.length;
  while (start < end && isGoSpace(runes[start])) start++;
  while (end > start && isGoSpace(runes[end - 1])) end--;
  return runes.slice(start, end).join("");
}

/** Go's `strings.TrimLeft(text, cutset)`. */
export function trimLeft(text: string, cutset: string): string {
  const runes = Array.from(text);
  let start = 0;
  while (start < runes.length && cutset.includes(runes[start])) start++;
  return runes.slice(start).join("");
}

/** Go's `strings.TrimRight(text, cutset)`. */
export function trimRight(text: string, cutset: string): string {
  const runes = Array.from(text);
  let end = runes.length;
  while (end > 0 && cutset.includes(runes[end - 1])) end--;
  return runes.slice(0, end).join("");
}

/** Go's `strings.Fields`: splits on any run of `unicode.IsSpace`. */
export function fields(text: string): string[] {
  const out: string[] = [];
  let current = "";
  for (const ch of Array.from(text)) {
    if (isGoSpace(ch)) {
      if (current !== "") {
        out.push(current);
        current = "";
      }
      continue;
    }
    current += ch;
  }
  if (current !== "") out.push(current);
  return out;
}

/** The number of runes, which is Go's `len([]rune(s))`. */
export function runeLength(text: string): number {
  return Array.from(text).length;
}

/** The first `limit` runes, which is Go's `string([]rune(s)[:limit])`. */
export function sliceRunes(text: string, start: number, end: number): string {
  return Array.from(text).slice(start, end).join("");
}

/**
 * Splits a document into pieces of about `size` runes, breaking on blank lines
 * first and on rune boundaries second.
 *
 * The pieces overlap by `overlap` runes so a sentence that falls on a boundary
 * is still findable from either side, and the split is on paragraphs before it
 * is on characters, because a chunk that starts in the middle of a sentence
 * embeds worse than one that starts at a heading.
 */
export function chunkText(text: string, size: number, overlap: number): string[] {
  if (size <= 0) {
    size = 1000;
  }
  if (overlap < 0 || overlap >= size) {
    overlap = 0;
  }

  const normalised = text.split("\r\n").join("\n");
  const paragraphs = normalised.split("\n\n");

  const chunks: string[] = [];
  let current: string[] = [];
  let currentLen = 0;

  const flush = () => {
    const joined = trimSpace(current.join("\n\n"));
    if (joined !== "") {
      chunks.push(joined);
    }
    current = [];
    currentLen = 0;
  };

  for (const paragraph of paragraphs) {
    const trimmed = trimSpace(paragraph);
    if (trimmed === "") {
      continue;
    }

    const trimmedLen = runeLength(trimmed);
    if (trimmedLen > size) {
      // A paragraph nobody ended is cut at rune boundaries, with the tail of
      // one piece repeated at the head of the next.
      flush();
      chunks.push(...splitLong(trimmed, size, overlap));
      continue;
    }

    if (currentLen > 0 && currentLen + trimmedLen > size) {
      flush();
    }

    current.push(trimmed);
    currentLen += trimmedLen;
  }

  flush();

  return chunks;
}

/** Cuts one over-long paragraph into overlapping pieces. */
export function splitLong(text: string, size: number, overlap: number): string[] {
  const runes = Array.from(text);
  const pieces: string[] = [];
  const step = size - overlap;

  for (let start = 0; start < runes.length; start += step) {
    let end = start + size;
    if (end > runes.length) {
      end = runes.length;
    }

    const piece = trimSpace(runes.slice(start, end).join(""));
    if (piece !== "") {
      pieces.push(piece);
    }

    if (end === runes.length) {
      break;
    }
  }

  return pieces;
}
