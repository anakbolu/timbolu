/**
 * The colours a Bolu is drawn with.
 *
 * Copied from `packages/bolu-character/palette.ts` rather than imported: that
 * package's entry points are TypeScript sources outside this project's root, and
 * pointing Metro at them would mean a custom `metro.config.js` and a second
 * resolution rule for a scaffold that renders no SVG yet. The numbers are the
 * contract, and the web package remains their owner; when the
 * `react-native-svg` adapter lands, it should import the package and this file
 * should go away.
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

/**
 * A colour that will not parse as `#rrggbb` falls back to the first swatch,
 * because a blank avatar reads as a rendering bug rather than as missing data.
 */
export function safeBoluColor(color: string | undefined): string {
  if (color && /^#[0-9a-fA-F]{6}$/.test(color)) return color;
  return BOLU_SWATCHES[0];
}
