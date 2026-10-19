/**
 * The QR code a phone scans is rendered here rather than in the browser.
 *
 * Two reasons. The payload is a capability — whoever scans it links the number —
 * so it must reach the person who started the attempt and nobody else, and the
 * frame it travels in is already the authenticated per-attempt socket. And the
 * browser has no QR encoder.
 *
 * What crosses the wire is the matrix rather than an image: a list of rows of
 * "1" and "0", which the screen draws. That is exactly what the Go handler
 * returns (`go-qrcode`'s `Bitmap()`, quiet zone included), so this file is a
 * small pure-TS encoder rather than a dependency.
 *
 * Ported 1:1 from internal/modules/whatsapp/handler/qr.go, with the piece of
 * `github.com/skip2/go-qrcode` the handler uses (byte mode, EC level L, version
 * auto-select, 4-module quiet zone) implemented here. The Go library picks the
 * numeric/alphanumeric encoders when the payload allows it; a byte-mode symbol
 * of the same level decodes identically, which is the contract the screen and
 * the phone care about.
 */

/** The four-module border a scanner needs around the symbol. */
const quietZone = 4;

/** Error-correction codewords per block, and the block layout, at level L. */
const levelL: ReadonlyArray<readonly [ecPerBlock: number, group1: number, data1: number, group2: number, data2: number]> = [
  [7, 1, 19, 0, 0],
  [10, 1, 34, 0, 0],
  [15, 1, 55, 0, 0],
  [20, 1, 80, 0, 0],
  [26, 1, 108, 0, 0],
  [18, 2, 68, 0, 0],
  [20, 2, 78, 0, 0],
  [24, 2, 97, 0, 0],
  [30, 2, 116, 0, 0],
  [18, 2, 68, 2, 69],
  [20, 4, 81, 0, 0],
  [24, 2, 92, 2, 93],
  [26, 4, 107, 0, 0],
  [30, 3, 115, 1, 116],
  [22, 5, 87, 1, 88],
  [24, 5, 98, 1, 99],
  [28, 1, 107, 5, 108],
  [30, 5, 120, 1, 121],
  [28, 3, 113, 4, 114],
  [28, 3, 107, 5, 108],
  [28, 4, 116, 4, 117],
  [28, 2, 111, 7, 112],
  [30, 4, 121, 5, 122],
  [30, 6, 117, 4, 118],
  [26, 8, 106, 4, 107],
  [28, 10, 114, 2, 115],
  [30, 8, 122, 4, 123],
  [30, 3, 117, 10, 118],
  [30, 7, 116, 7, 117],
  [30, 5, 115, 10, 116],
  [30, 13, 115, 3, 116],
  [30, 17, 115, 0, 0],
  [30, 17, 115, 1, 116],
  [30, 13, 115, 6, 116],
  [30, 12, 121, 7, 122],
  [30, 6, 121, 14, 122],
  [30, 17, 122, 4, 123],
  [30, 4, 122, 18, 123],
  [30, 20, 117, 4, 118],
  [30, 19, 118, 6, 119],
];

/** Alignment-pattern centre coordinates for each version (index = version - 1). */
const alignmentCenters: ReadonlyArray<readonly number[]> = [
  [],
  [6, 18],
  [6, 22],
  [6, 26],
  [6, 30],
  [6, 34],
  [6, 22, 38],
  [6, 24, 42],
  [6, 26, 46],
  [6, 28, 50],
  [6, 30, 54],
  [6, 32, 58],
  [6, 34, 62],
  [6, 26, 46, 66],
  [6, 26, 48, 70],
  [6, 26, 50, 74],
  [6, 30, 54, 78],
  [6, 30, 56, 82],
  [6, 30, 58, 86],
  [6, 34, 62, 90],
  [6, 28, 50, 72, 94],
  [6, 26, 50, 74, 98],
  [6, 30, 54, 78, 102],
  [6, 28, 54, 80, 106],
  [6, 32, 58, 84, 110],
  [6, 30, 58, 86, 114],
  [6, 34, 62, 90, 118],
  [6, 26, 50, 74, 98, 122],
  [6, 30, 54, 78, 102, 126],
  [6, 26, 52, 78, 104, 130],
  [6, 30, 56, 82, 108, 134],
  [6, 34, 60, 86, 112, 138],
  [6, 30, 58, 86, 114, 142],
  [6, 34, 62, 90, 118, 146],
  [6, 30, 54, 78, 102, 126, 150],
  [6, 24, 50, 76, 102, 128, 154],
  [6, 28, 54, 80, 106, 132, 158],
  [6, 32, 58, 84, 110, 136, 162],
  [6, 26, 54, 82, 110, 138, 166],
  [6, 30, 58, 86, 114, 142, 170],
];

// --------------------------------------------------------------- GF(256)

const exp = new Uint8Array(512);
const log = new Uint8Array(256);

for (let i = 0, value = 1; i < 255; i++) {
  exp[i] = value;
  log[value] = i;
  value <<= 1;
  if (value & 0x100) value ^= 0x11d;
}
for (let i = 255; i < 512; i++) exp[i] = exp[i - 255];

/** Multiplies two field elements. */
function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0;
  return exp[log[a] + log[b]];
}

/** Builds the generator polynomial for `degree` error-correction codewords. */
function generator(degree: number): Uint8Array {
  let poly = new Uint8Array([1]);
  for (let i = 0; i < degree; i++) {
    const next = new Uint8Array(poly.length + 1);
    for (let j = 0; j < poly.length; j++) {
      next[j] ^= poly[j];
      next[j + 1] ^= gfMul(poly[j], exp[i]);
    }
    poly = next;
  }
  return poly;
}

/** Computes the error-correction codewords for one block. */
function reedSolomon(data: Uint8Array, ecLength: number): Uint8Array {
  const gen = generator(ecLength);
  const remainder = new Uint8Array(ecLength);

  for (const byte of data) {
    const factor = byte ^ remainder[0];
    remainder.copyWithin(0, 1);
    remainder[ecLength - 1] = 0;
    for (let i = 0; i < ecLength; i++) {
      remainder[i] ^= gfMul(gen[i + 1], factor);
    }
  }
  return remainder;
}

// ------------------------------------------------------------- bit stream

/** The bits of a byte-mode payload, before version selection. */
function payloadBits(text: string): number[] {
  const bytes = new TextEncoder().encode(text);
  const bits: number[] = [];

  const push = (value: number, length: number): void => {
    for (let i = length - 1; i >= 0; i--) bits.push((value >>> i) & 1);
  };

  push(0b0100, 4); // byte mode
  push(bytes.length, bytes.length < 256 ? 8 : 16);
  for (const byte of bytes) push(byte, 8);

  return bits;
}

/** The data codewords of one version, zero-padded and terminated. */
function dataCodewords(bits: number[], capacityBytes: number): Uint8Array {
  const capacity = capacityBytes * 8;
  const out = bits.slice(0, capacity);

  // Terminator, then to a byte boundary, then the two pad codewords.
  out.push(...new Array(Math.min(4, capacity - out.length)).fill(0));
  while (out.length % 8 !== 0) out.push(0);

  const codewords = new Uint8Array(capacityBytes);
  for (let i = 0; i < out.length; i += 8) {
    let byte = 0;
    for (let j = 0; j < 8; j++) byte = (byte << 1) | (out[i + j] ?? 0);
    codewords[i / 8] = byte;
  }

  for (let i = out.length / 8, pad = 0; i < capacityBytes; i++, pad++) {
    codewords[i] = pad % 2 === 0 ? 0xec : 0x11;
  }

  return codewords;
}

/** Splits one version's data codewords into blocks and interleaves them with their EC. */
function interleave(data: Uint8Array, version: number): Uint8Array {
  const [ecPerBlock, group1, data1, group2, data2] = levelL[version - 1];

  const blocks: Uint8Array[] = [];
  const ecBlocks: Uint8Array[] = [];
  let offset = 0;

  for (let i = 0; i < group1 + group2; i++) {
    const size = i < group1 ? data1 : data2;
    const block = data.slice(offset, offset + size);
    offset += size;
    blocks.push(block);
    ecBlocks.push(reedSolomon(block, ecPerBlock));
  }

  const out: number[] = [];
  const longest = Math.max(data1, data2);
  for (let i = 0; i < longest; i++) {
    for (const block of blocks) {
      if (i < block.length) out.push(block[i]);
    }
  }
  for (let i = 0; i < ecPerBlock; i++) {
    for (const block of ecBlocks) out.push(block[i]);
  }

  return Uint8Array.from(out);
}

// ---------------------------------------------------------------- matrix

/** A square of modules, `null` where a data bit may go. */
type Matrix = (boolean | null)[][];

/** Builds an empty matrix for one version. */
function emptyMatrix(size: number): Matrix {
  return Array.from({ length: size }, () => new Array<boolean | null>(size).fill(null));
}

/** Writes one module when it is inside the symbol. */
function set(matrix: Matrix, x: number, y: number, dark: boolean): void {
  if (y >= 0 && y < matrix.length && x >= 0 && x < matrix.length) {
    matrix[y][x] = dark;
  }
}

/** Draws a finder pattern with its separator at (x, y). */
function finder(matrix: Matrix, x: number, y: number): void {
  for (let dy = -1; dy <= 7; dy++) {
    for (let dx = -1; dx <= 7; dx++) {
      const inside = dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6;
      const ring = inside && (dx === 0 || dx === 6 || dy === 0 || dy === 6);
      const core = inside && dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4;
      set(matrix, x + dx, y + dy, ring || core);
    }
  }
}

/** Draws one alignment pattern centred at (cx, cy). */
function alignment(matrix: Matrix, cx: number, cy: number): void {
  for (let dy = -2; dy <= 2; dy++) {
    for (let dx = -2; dx <= 2; dx++) {
      const dark = Math.max(Math.abs(dx), Math.abs(dy)) !== 1;
      set(matrix, cx + dx, cy + dy, dark);
    }
  }
}

/** The 15-bit format word for level L and one mask. */
function formatBits(mask: number): number {
  const data = (0b01 << 3) | mask; // 01 is level L
  let value = data << 10;
  for (let i = 14; i >= 10; i--) {
    if ((value >>> i) & 1) value ^= 0x537 << (i - 10);
  }
  return ((data << 10) | value) ^ 0x5412;
}

/** The 18-bit version word, for versions 7 and up. */
function versionBits(version: number): number {
  let value = version << 12;
  for (let i = 17; i >= 12; i--) {
    if ((value >>> i) & 1) value ^= 0x1f25 << (i - 12);
  }
  return (version << 12) | value;
}

/** Draws the function patterns, leaving the data area `null`. */
function functionPatterns(version: number): Matrix {
  const size = version * 4 + 17;
  const matrix = emptyMatrix(size);

  // Timing patterns.
  for (let i = 8; i < size - 8; i++) {
    set(matrix, i, 6, i % 2 === 0);
    set(matrix, 6, i, i % 2 === 0);
  }

  finder(matrix, 0, 0);
  finder(matrix, size - 7, 0);
  finder(matrix, 0, size - 7);

  // The dark module, which is always set.
  set(matrix, 8, size - 8, true);

  // Format information, reserved now and written after the mask is chosen.
  for (let i = 0; i < 9; i++) {
    if (matrix[8][i] === null) set(matrix, i, 8, false);
    if (matrix[i][8] === null) set(matrix, 8, i, false);
  }
  for (let i = 0; i < 8; i++) {
    set(matrix, size - 1 - i, 8, false);
    set(matrix, 8, size - 1 - i, false);
  }

  const centers = alignmentCenters[version - 1];
  for (let i = 0; i < centers.length; i++) {
    for (let j = 0; j < centers.length; j++) {
      const cx = centers[i];
      const cy = centers[j];
      // The three corners already hold finder patterns.
      const corner =
        (cx === 6 && cy === 6) ||
        (cx === 6 && cy === size - 7) ||
        (cx === size - 7 && cy === 6);
      if (!corner) alignment(matrix, cx, cy);
    }
  }

  if (version >= 7) {
    const word = versionBits(version);
    for (let i = 0; i < 18; i++) {
      const bit = ((word >>> i) & 1) === 1;
      const a = Math.floor(i / 3);
      const b = (i % 3) + size - 11;
      set(matrix, a, b, bit);
      set(matrix, b, a, bit);
    }
  }

  return matrix;
}

/** Places the interleaved codewords in the zigzag order the spec defines. */
function placeData(matrix: Matrix, codewords: Uint8Array): void {
  const size = matrix.length;
  let bit = 0;

  for (let right = size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5; // the vertical timing column is skipped
    const upward = ((right + 1) & 2) === 0;

    for (let vert = 0; vert < size; vert++) {
      const y = upward ? size - 1 - vert : vert;
      for (let j = 0; j < 2; j++) {
        const x = right - j;
        if (matrix[y][x] !== null) continue;
        const byte = codewords[bit >> 3] ?? 0;
        matrix[y][x] = ((byte >>> (7 - (bit & 7))) & 1) === 1;
        bit++;
      }
    }
  }
}

/**
 * Applies one mask to the data modules.
 *
 * `reserved` is the function-pattern skeleton, where a non-null module is one
 * the spec does not mask.
 */
function applyMask(matrix: Matrix, reserved: Matrix, mask: number): Matrix {
  const size = matrix.length;
  const out = matrix.map((row) => row.slice());

  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      if (reserved[y][x] !== null) continue;
      if (maskAt(mask, x, y)) out[y][x] = !out[y][x];
    }
  }
  return out;
}

/** The mask predicate. */
function maskAt(mask: number, x: number, y: number): boolean {
  switch (mask) {
    case 0:
      return (x + y) % 2 === 0;
    case 1:
      return y % 2 === 0;
    case 2:
      return x % 3 === 0;
    case 3:
      return (x + y) % 3 === 0;
    case 4:
      return (Math.floor(y / 2) + Math.floor(x / 3)) % 2 === 0;
    case 5:
      return ((x * y) % 2) + ((x * y) % 3) === 0;
    case 6:
      return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0;
    default:
      return (((x + y) % 2) + ((x * y) % 3)) % 2 === 0;
  }
}

/** The four penalty rules the spec scores a mask with. */
function penalty(matrix: Matrix): number {
  const size = matrix.length;
  const at = (x: number, y: number): boolean => matrix[y][x] === true;
  let score = 0;

  // Rule 1: runs of five or more.
  for (let y = 0; y < size; y++) {
    for (const horizontal of [true, false]) {
      let run = 1;
      for (let i = 1; i < size; i++) {
        const current = horizontal ? at(i, y) : at(y, i);
        const previous = horizontal ? at(i - 1, y) : at(y, i - 1);
        if (current === previous) {
          run++;
        } else {
          if (run >= 5) score += 3 + (run - 5);
          run = 1;
        }
      }
      if (run >= 5) score += 3 + (run - 5);
    }
  }

  // Rule 2: 2x2 blocks of one colour.
  for (let y = 0; y < size - 1; y++) {
    for (let x = 0; x < size - 1; x++) {
      const colour = at(x, y);
      if (colour === at(x + 1, y) && colour === at(x, y + 1) && colour === at(x + 1, y + 1)) {
        score += 3;
      }
    }
  }

  // Rule 3: finder-like 1:1:3:1:1 patterns with four light modules beside them.
  const pattern = [true, false, true, true, true, false, true];
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      for (const horizontal of [true, false]) {
        let matches = true;
        for (let i = 0; i < pattern.length && matches; i++) {
          const px = horizontal ? x + i : x;
          const py = horizontal ? y : y + i;
          if (px >= size || py >= size) {
            matches = false;
          } else if (at(px, py) !== pattern[i]) {
            matches = false;
          }
        }
        if (!matches) continue;

        const beforeLight = [1, 2, 3, 4].every((d) => {
          const px = horizontal ? x - d : x;
          const py = horizontal ? y : y - d;
          return px < 0 || py < 0 || !at(px, py);
        });
        const afterLight = [7, 8, 9, 10].every((d) => {
          const px = horizontal ? x + d : x;
          const py = horizontal ? y : y + d;
          return px >= size || py >= size || !at(px, py);
        });
        if (beforeLight || afterLight) score += 40;
      }
    }
  }

  // Rule 4: the balance of dark and light.
  let dark = 0;
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) if (at(x, y)) dark++;
  }
  const percent = (dark * 100) / (size * size);
  score += Math.floor(Math.abs(percent - 50) / 5) * 10;

  return score;
}

/** Writes the format word for one mask into its two places. */
function writeFormat(matrix: Matrix, mask: number): void {
  const size = matrix.length;
  const word = formatBits(mask);
  const bit = (i: number): boolean => ((word >>> i) & 1) === 1;

  // The first copy, around the top-left finder.
  for (let i = 0; i <= 5; i++) set(matrix, 8, i, bit(i));
  set(matrix, 8, 7, bit(6));
  set(matrix, 8, 8, bit(7));
  set(matrix, 7, 8, bit(8));
  for (let i = 9; i < 15; i++) set(matrix, 14 - i, 8, bit(i));

  // The second copy, split between the bottom-left and top-right finders.
  for (let i = 0; i < 8; i++) set(matrix, size - 1 - i, 8, bit(i));
  for (let i = 8; i < 15; i++) set(matrix, 8, size - 15 + i, bit(i));
  set(matrix, 8, size - 8, true);
}

/**
 * Renders a QR code as rows of bits, or null when the payload cannot be
 * encoded.
 *
 * The recovery level is low because the payload is a fixed-size protocol string
 * and the code is shown on a screen rather than printed. An unencodable payload
 * is a protocol problem rather than a user's, and the attempt is reported by its
 * state: answering with no matrix means the screen offers the phone-code
 * alternative instead of a broken picture.
 */
export function qrMatrix(payload: string): string[] | null {
  if (payload === "") return null;

  const bits = payloadBits(payload);

  // The smallest version that fits, which is what go-qrcode chooses.
  let version = 0;
  for (let candidate = 1; candidate <= 40; candidate++) {
    const [, group1, data1, group2, data2] = levelL[candidate - 1];
    const capacity = (group1 * data1 + group2 * data2) * 8;
    if (bits.length <= capacity) {
      version = candidate;
      break;
    }
  }
  if (version === 0) return null;

  const [, group1, data1, group2, data2] = levelL[version - 1];
  const codewords = interleave(
    dataCodewords(bits, group1 * data1 + group2 * data2),
    version,
  );

  const reserved = functionPatterns(version);
  const base = reserved.map((row) => row.slice());
  placeData(base, codewords);

  // The mask with the lowest penalty is the one the spec says to use.
  let best = applyMask(base, reserved, 0);
  let bestScore = penalty(best);
  let bestMask = 0;
  for (let mask = 1; mask < 8; mask++) {
    const candidate = applyMask(base, reserved, mask);
    const score = penalty(candidate);
    if (score < bestScore) {
      best = candidate;
      bestScore = score;
      bestMask = mask;
    }
  }

  writeFormat(best, bestMask);

  const size = best.length;
  const rows: string[] = [];
  for (let y = -quietZone; y < size + quietZone; y++) {
    let row = "";
    for (let x = -quietZone; x < size + quietZone; x++) {
      const dark = x >= 0 && x < size && y >= 0 && y < size ? best[y][x] === true : false;
      row += dark ? "1" : "0";
    }
    rows.push(row);
  }
  return rows;
}
