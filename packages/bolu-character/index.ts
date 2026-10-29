/**
 * The platform-neutral half of the Bolu character: the shape and expression
 * names, the palette, and the three colour helpers.
 *
 * It is a package because the app needs the same names and the same colours as
 * the web, and a copy in each client is a second answer. Nothing here may import
 * React or emit JSX: the geometry that draws a body is still in
 * `apps/frontend/src/components/bolu/characters.tsx` until the DOM/SVG and
 * `react-native-svg` adapters exist (roadmap #302 Fase 0 butir 2), and that file
 * re-exports every name below so no screen changes.
 *
 * There is no build step: a bundler consumes this file as source, the same way
 * `@timbolu/contracts` is consumed.
 */

/**
 * The sixteen body shapes, in the reference's order.
 *
 * The union and the list are written out rather than derived from the geometry:
 * `Object.keys(BOLU_SHAPES)` was the derivation while the geometry lived beside
 * them, and the geometry is JSX, so it stayed in the frontend. They sit together
 * here so adding a shape is one edit; `BOLU_SHAPES` in the frontend is keyed by
 * this union, so a shape without a name — or a name without a shape — fails to
 * compile.
 */
export type BoluShapeName =
  | "kacang"
  | "hantu"
  | "gumpal"
  | "awan"
  | "daun"
  | "percik"
  | "bintang"
  | "genangan"
  | "melambai"
  | "tetes"
  | "mochi"
  | "bulat"
  | "kotak"
  | "segitiga"
  | "hati"
  | "kucing";

/** Every body shape, in the reference's order. */
export const BOLU_SHAPE_NAMES: BoluShapeName[] = [
  "kacang",
  "hantu",
  "gumpal",
  "awan",
  "daun",
  "percik",
  "bintang",
  "genangan",
  "melambai",
  "tetes",
  "mochi",
  "bulat",
  "kotak",
  "segitiga",
  "hati",
  "kucing",
];

/** The ten expressions, in the reference's order. */
export type BoluExpressionName =
  | "senang"
  | "iseng"
  | "bosan"
  | "mikir"
  | "marah"
  | "capek"
  | "kaget"
  | "sedih"
  | "gemas"
  | "cinta";

/** Every expression, in the reference's order. */
export const BOLU_EXPRESSION_NAMES: BoluExpressionName[] = [
  "senang",
  "iseng",
  "bosan",
  "mikir",
  "marah",
  "capek",
  "kaget",
  "sedih",
  "gemas",
  "cinta",
];

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

/**
 * What an expression may ask of an eye. It names no React type, so the app's
 * adapter can honour the same options with `react-native-svg`.
 */
export type BoluEyeOptions = {
  /** Eye radius. */
  rx?: number;
  ry?: number;
  /** Eyelid line, as `[outerY, innerY]`. Adds the lid and clips the eye under it. */
  lid?: [number, number] | null;
  /** Pupil radius. */
  pr?: number;
};
