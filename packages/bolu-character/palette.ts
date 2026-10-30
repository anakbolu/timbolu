/**
 * The Bolu palette and the colour rules derived from it.
 *
 * This file has no JSX and no React import on purpose: the web app renders it
 * into SVG attributes, and the Expo app will read the same numbers through
 * `react-native-svg`. The body geometry sits beside it in `characters.ts`.
 *
 * The arithmetic is copied verbatim from the reference port. Do not "fix" it:
 * `hexToHsl` returns degrees and percentages, and `feetColorFor` interpolates
 * its string the way the reference did, so a rounded value would change the
 * rendered SVG.
 */

/** The palette the reference offers, plus a free colour picker in the UI. */
export const BOLU_SWATCHES = [
  "#F28C4E",
  "#2E9BEF",
  "#A77DC9",
  "#4DBB72",
  "#F59ABF",
  "#B8D435",
  "#E0389A",
  "#FFCD2E",
  "#F2645A",
  "#1BB4C8",
  "#7CCBF2",
  "#8B6FD6",
];

/** HSL of a `#rrggbb` colour; the body colour drives feet and ink. */
export function hexToHsl(hex: string): [number, number, number] {
  const r = parseInt(hex.slice(1, 3), 16) / 255;
  const g = parseInt(hex.slice(3, 5), 16) / 255;
  const b = parseInt(hex.slice(5, 7), 16) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  let h = 0;
  let s = 0;
  const l = (max + min) / 2;
  if (max !== min) {
    const d = max - min;
    s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
    h = max === r ? (g - b) / d + (g < b ? 6 : 0) : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
    h *= 60;
  }
  return [h, s * 100, l * 100];
}

/** Faces and lines flip to a warm off-white on a dark body. */
export function inkFor(lightness: number): string {
  return lightness < 32 ? "#FFF6EA" : "#1E1B2E";
}

/** Feet read as a darker shade of the body colour. */
export function feetColorFor(hex: string): string {
  const [h, s, l] = hexToHsl(hex);
  return `hsl(${h} ${s}% ${Math.max(l - 12, 8)}%)`;
}
