/**
 * The Bolu character generator: 16 body shapes, 10 expressions, the feet, and
 * the 200×200 viewBox — as plain data and pure functions.
 *
 * This file has no JSX and no React import on purpose. `charSVG()` returns a
 * `BoluDescription`: an element tree (paths, circles, rects, polygons, groups,
 * transforms, paint) that a renderer maps one-to-one. `dom.tsx` maps it to web
 * SVG today; the Expo app will map the same tree to `react-native-svg` later
 * (#302). The descriptor carries every coordinate, so an adapter never has to
 * recompute geometry.
 *
 * Geometry is copied 1:1 from the reference port. Do not "tidy" the numbers:
 * rounding a coordinate changes the drawing.
 */

import type { BoluExpressionName, BoluShapeName } from "./names.ts";
import { feetColorFor, hexToHsl, inkFor } from "./palette.ts";

/** The viewBox and intrinsic size every character is drawn in. */
export const BOLU_VIEW_BOX = "0 0 200 200";
export const BOLU_WIDTH = 200;
export const BOLU_HEIGHT = 200;

/** What a Bolu is when the caller says nothing. */
export const BOLU_DEFAULT_COLOR = "#F28C4E";
export const BOLU_DEFAULT_SHAPE: BoluShapeName = "kacang";
export const BOLU_DEFAULT_EXPRESSION: BoluExpressionName = "senang";

/**
 * One drawable element. The union is deliberately small: it is exactly what the
 * reference emits, so both adapters (SVG DOM and `react-native-svg`) can switch
 * on `kind` without guessing.
 */
export type BoluElement =
  | {
      kind: "path";
      d: string;
      className?: string;
      fill?: string;
      stroke?: string;
      strokeWidth?: number;
      strokeLinecap?: "round";
    }
  | {
      kind: "circle";
      cx: number;
      cy: number;
      r: number;
      className?: string;
      fill?: string;
    }
  | {
      kind: "ellipse";
      cx: number;
      cy: number;
      rx: number;
      ry: number;
      fill?: string;
    }
  | {
      kind: "rect";
      x: number;
      y: number;
      width: number;
      height: number;
      rx?: number;
      fill?: string;
      stroke?: string;
      strokeWidth?: number;
    }
  | { kind: "polygon"; points: string }
  | {
      kind: "group";
      className?: string;
      transform?: string;
      fill?: string;
      clipPath?: string;
      children: BoluElement[];
    };

export type BoluShape = {
  label: string;
  /** Body outline stroke width. */
  sw: number;
  /** Baseline the feet sit on; `null` when the shape has no feet. */
  foot: number | null;
  /** Face origin and scale: `[x, y, scale]`. */
  face: [number, number, number];
  /** Body geometry, drawn in the body colour. */
  body: BoluElement[];
};

/** A regular star, used by the `bintang` body. */
function starPath(cx: number, cy: number, outer: number, inner: number): string {
  let d = "";
  for (let i = 0; i < 10; i++) {
    const angle = -Math.PI / 2 + (i * Math.PI) / 5;
    const radius = i % 2 ? inner : outer;
    d += `${i ? "L" : "M"}${(cx + radius * Math.cos(angle)).toFixed(1)} ${(cy + radius * Math.sin(angle)).toFixed(1)} `;
  }
  return `${d}Z`;
}

/**
 * The two flat ellipse feet, drawn just above the shape's baseline.
 * A port of the reference's `footPath()`.
 */
export function footPath(baseline: number): string {
  const y = baseline - 3;
  return `M70 ${y} a12 8 0 1 0 24 0 a12 8 0 1 0 -24 0 Z M106 ${y} a12 8 0 1 0 24 0 a12 8 0 1 0 -24 0 Z`;
}

export const BOLU_SHAPES: Record<BoluShapeName, BoluShape> = {
  kacang: {
    label: "Kacang",
    sw: 4,
    foot: 168,
    face: [100, 104, 1.1],
    body: [
      {
        kind: "path",
        d: "M60 50 C100 30 160 40 165 90 C170 140 140 170 95 168 C50 166 28 140 32 100 C34 75 40 60 60 50 Z",
      },
    ],
  },
  hantu: {
    label: "Hantu",
    sw: 6,
    foot: null,
    face: [98, 86, 1],
    body: [
      {
        kind: "path",
        d: "M90 30 C130 26 150 60 146 100 C143 126 154 142 162 156 Q172 174 152 174 C130 174 112 168 100 160 C70 168 52 140 52 104 C52 60 60 34 90 30 Z",
      },
    ],
  },
  gumpal: {
    label: "Gumpal",
    sw: 4,
    foot: 166,
    face: [100, 100, 1.15],
    body: [
      {
        kind: "path",
        d: "M50 52 C80 36 130 38 158 52 C176 70 172 130 160 152 C140 172 70 174 46 154 C30 130 30 72 50 52 Z",
      },
    ],
  },
  awan: {
    label: "Awan",
    sw: 4,
    foot: 162,
    face: [102, 116, 1.05],
    body: [
      {
        kind: "path",
        d: "M50 162 Q20 162 22 132 Q24 106 50 102 Q50 64 88 60 Q112 36 140 58 Q176 60 172 100 Q190 114 182 140 Q176 164 150 162 Z",
      },
    ],
  },
  daun: {
    label: "Daun",
    sw: 4,
    foot: null,
    face: [98, 98, 1.05],
    body: [
      {
        kind: "path",
        d: "M40 60 C70 46 150 50 166 66 C176 80 150 140 110 158 C80 170 46 160 38 130 C30 100 26 70 40 60 Z",
      },
    ],
  },
  percik: {
    label: "Percik",
    sw: 4,
    foot: null,
    face: [104, 108, 1],
    body: [
      {
        kind: "path",
        d: "M60 60 C90 40 120 70 150 58 C176 50 178 90 164 110 C152 130 172 150 150 162 C120 176 100 150 72 162 C44 172 28 140 40 116 C50 96 36 76 60 60 Z",
      },
    ],
  },
  bintang: {
    label: "Bintang",
    sw: 36,
    foot: null,
    face: [100, 112, 0.95],
    body: [{ kind: "path", d: starPath(100, 110, 66, 40) }],
  },
  genangan: {
    label: "Genangan",
    sw: 4,
    foot: null,
    face: [104, 128, 0.9],
    body: [
      {
        kind: "path",
        d: "M30 140 C24 110 50 104 70 112 C90 120 100 80 130 82 C160 84 178 110 172 140 C168 164 130 166 100 162 C70 160 34 168 30 140 Z",
      },
    ],
  },
  melambai: {
    label: "Melambai",
    sw: 4,
    foot: 172,
    face: [100, 96, 0.9],
    body: [
      { kind: "rect", x: 64, y: 46, width: 72, height: 126, rx: 36 },
      {
        kind: "path",
        className: "bolu-wave",
        d: "M132 110 Q152 98 158 74",
        fill: "none",
        strokeWidth: 16,
        strokeLinecap: "round",
      },
      {
        kind: "path",
        d: "M68 122 Q54 134 52 148",
        fill: "none",
        strokeWidth: 14,
        strokeLinecap: "round",
      },
    ],
  },
  tetes: {
    label: "Tetes",
    sw: 6,
    foot: 172,
    face: [100, 126, 1.05],
    body: [
      {
        kind: "path",
        d: "M88 42 Q100 22 112 42 Q150 92 160 120 Q166 172 100 172 Q34 172 40 120 Q50 92 88 42 Z",
      },
    ],
  },
  mochi: {
    label: "Mochi",
    sw: 4,
    foot: 172,
    face: [100, 124, 1.1],
    body: [
      {
        kind: "path",
        d: "M28 142 Q24 72 100 68 Q176 72 172 142 Q172 172 100 172 Q28 172 28 142 Z",
      },
    ],
  },
  bulat: {
    label: "Bulat",
    sw: 4,
    foot: 174,
    face: [100, 104, 1.15],
    body: [{ kind: "circle", cx: 100, cy: 106, r: 68 }],
  },
  kotak: {
    label: "Kotak",
    sw: 4,
    foot: 172,
    face: [100, 106, 1.2],
    body: [{ kind: "rect", x: 34, y: 40, width: 132, height: 132, rx: 30 }],
  },
  segitiga: {
    label: "Segitiga",
    sw: 40,
    foot: 170,
    face: [100, 124, 0.85],
    body: [{ kind: "path", d: "M100 52 L154 152 L46 152 Z" }],
  },
  hati: {
    label: "Hati",
    sw: 6,
    foot: null,
    face: [100, 104, 0.95],
    body: [
      {
        kind: "path",
        d: "M108 162 Q100 170 92 162 C40 128 18 98 32 70 C46 42 84 44 100 72 C116 44 154 42 168 70 C182 98 160 128 108 162 Z",
      },
    ],
  },
  kucing: {
    label: "Kucing",
    sw: 6,
    foot: 176,
    face: [100, 114, 1.05],
    body: [
      {
        kind: "path",
        d: "M46 80 Q40 36 52 30 Q62 26 92 52 Z M154 80 Q160 36 148 30 Q138 26 108 52 Z",
      },
      { kind: "circle", cx: 100, cy: 112, r: 64 },
    ],
  },
};

export type BoluEyeOptions = {
  /** Eye radius. */
  rx?: number;
  ry?: number;
  /** Eyelid line, as `[outerY, innerY]`. Adds the lid and clips the eye under it. */
  lid?: [number, number] | null;
  /** Pupil radius. */
  pr?: number;
};

/**
 * Builders an expression draws with. They are per-instance, so the clip paths
 * they emit get unique ids.
 */
export type BoluFaceContext = {
  eye: (cx: number, options?: BoluEyeOptions) => BoluElement;
  /** A heart-shaped pupil. */
  heart: (cx: number) => BoluElement;
  /** A falling tear; `t2` defers it, so the two tears run out of phase. */
  tear: (cx: number, delay?: "t2") => BoluElement;
};

export type BoluExpression = {
  label: string;
  /** Line the reference shows under the character, e.g. "lagi mikir". */
  say: string;
  /** Body animation class, `a-bounce` and friends. */
  anim: string;
  /** Whether the eyes blink on their own, as in the reference. */
  blink: boolean;
  /** Resting gaze offset; the design's own `look`. */
  look?: [number, number];
  /** Cheek blush opacity; the reference defaults it to 0.55. */
  blush?: number;
  face: (ctx: BoluFaceContext) => BoluElement[];
};

export const BOLU_EXPRESSIONS: Record<BoluExpressionName, BoluExpression> = {
  senang: {
    label: "Senang",
    say: "lagi senang",
    anim: "a-bounce",
    blink: false,
    blush: 0.75,
    face: () => [
      {
        kind: "group",
        className: "bolu-look",
        children: [
          {
            kind: "path",
            d: "M-25 -5 Q-17 -16 -9 -5 M9 -5 Q17 -16 25 -5",
            className: "bolu-ln",
            strokeWidth: 5,
          },
        ],
      },
      { kind: "path", d: "M-12 9 Q0 11 12 9 Q10 26 0 26 Q-10 26 -12 9 Z", fill: "var(--bolu-ink)" },
      { kind: "path", d: "M-6 22 Q0 17 6 22 Q3 25.5 0 25.5 Q-3 25.5 -6 22 Z", fill: "#ff7d93" },
    ],
  },
  iseng: {
    label: "Iseng",
    say: "lagi iseng",
    anim: "a-wiggle",
    blink: false,
    face: ({ eye }) => [
      eye(-17),
      { kind: "path", d: "M9 -7 Q17 -14 25 -7", className: "bolu-ln", strokeWidth: 5 },
      { kind: "ellipse", cx: 5, cy: 18, rx: 4.5, ry: 5.5, fill: "#ff7d93" },
      { kind: "path", d: "M-10 11 Q0 20 10 11", className: "bolu-ln", strokeWidth: 4.5 },
    ],
  },
  bosan: {
    label: "Bosan",
    say: "lagi bosan…",
    anim: "a-sway",
    blink: true,
    look: [-3, 1.5],
    face: ({ eye }) => [
      eye(-17, { lid: [-8, -8] }),
      eye(17, { lid: [-8, -8] }),
      { kind: "path", d: "M-9 18 L7 16", className: "bolu-ln", strokeWidth: 4.5 },
    ],
  },
  mikir: {
    label: "Mikir",
    say: "lagi mikir",
    anim: "a-calm",
    blink: true,
    look: [3, -3.5],
    face: ({ eye }) => [
      eye(-17),
      eye(17),
      { kind: "path", d: "M-26 -25 L-10 -23", className: "bolu-ln", strokeWidth: 4.5 },
      { kind: "path", d: "M9 -28 Q17 -34 26 -28", className: "bolu-ln", strokeWidth: 4.5 },
      { kind: "path", d: "M-4 18 Q0 15 4 18 Q8 21 12 18", className: "bolu-ln", strokeWidth: 4.5 },
      {
        kind: "group",
        fill: "var(--bolu-ink)",
        children: [
          { kind: "circle", className: "bolu-dot", cx: 38, cy: -40, r: 3.5 },
          { kind: "circle", className: "bolu-dot bolu-d2", cx: 48, cy: -52, r: 4.5 },
          { kind: "circle", className: "bolu-dot bolu-d3", cx: 61, cy: -65, r: 6 },
        ],
      },
    ],
  },
  marah: {
    label: "Marah",
    say: "lagi marah!",
    anim: "a-shake",
    blink: false,
    face: ({ eye }) => [
      eye(-17, { lid: [-15, -5] }),
      eye(17, { lid: [-15, -5] }),
      { kind: "path", d: "M-29 -23 L-8 -14 M29 -23 L8 -14", className: "bolu-ln", strokeWidth: 5 },
      {
        kind: "rect",
        x: -12,
        y: 13,
        width: 24,
        height: 10,
        rx: 4,
        fill: "#fff",
        stroke: "var(--bolu-ink)",
        strokeWidth: 3.5,
      },
      {
        kind: "path",
        d: "M-10 18 L10 18 M-4 13 L-4 23 M4 13 L4 23",
        stroke: "var(--bolu-ink)",
        strokeWidth: 2,
      },
      {
        kind: "group",
        transform: "translate(42 -44)",
        children: [
          {
            kind: "group",
            className: "bolu-pulse",
            children: [
              {
                kind: "path",
                d: "M-10 -3 Q-3 -3 -3 -10 M3 -10 Q3 -3 10 -3 M10 3 Q3 3 3 10 M-3 10 Q-3 3 -10 3",
                stroke: "#ff3d4f",
                strokeWidth: 4,
                fill: "none",
                strokeLinecap: "round",
              },
            ],
          },
        ],
      },
    ],
  },
  capek: {
    label: "Capek",
    say: "capek banget",
    anim: "a-sag",
    blink: true,
    look: [0, 2],
    face: ({ eye }) => [
      eye(-17, { lid: [-2, -11] }),
      eye(17, { lid: [-2, -11] }),
      { kind: "ellipse", cx: 0, cy: 19, rx: 5, ry: 3.5, fill: "var(--bolu-ink)" },
      {
        kind: "path",
        className: "bolu-sweat",
        fill: "#9ad8ff",
        d: "M36 -40 Q43 -30 43 -26 A7 7 0 0 1 29 -26 Q29 -30 36 -40 Z",
      },
    ],
  },
  kaget: {
    label: "Kaget",
    say: "kaget!",
    anim: "a-jump",
    blink: false,
    face: ({ eye }) => [
      eye(-17, { rx: 12, ry: 13, pr: 3.5 }),
      eye(17, { rx: 12, ry: 13, pr: 3.5 }),
      { kind: "ellipse", cx: 0, cy: 20, rx: 6, ry: 8, fill: "var(--bolu-ink)" },
      {
        kind: "path",
        d: "M-36 -38 L-42 -48 M0 -44 L0 -56 M36 -38 L42 -48",
        className: "bolu-ln",
        strokeWidth: 4.5,
      },
    ],
  },
  sedih: {
    label: "Sedih",
    say: "lagi sedih",
    anim: "a-calm",
    blink: true,
    look: [0, 2.5],
    face: ({ eye, tear }) => [
      eye(-17),
      eye(17),
      { kind: "path", d: "M-27 -20 L-11 -26 M27 -20 L11 -26", className: "bolu-ln", strokeWidth: 4.5 },
      { kind: "path", d: "M-9 22 Q0 14 9 22", className: "bolu-ln", strokeWidth: 4.5 },
      tear(-20),
      tear(20, "t2"),
    ],
  },
  gemas: {
    label: "Gemas",
    say: "lagi gemas >w<",
    anim: "a-wiggle",
    blink: false,
    blush: 0.95,
    face: () => [
      {
        kind: "group",
        className: "bolu-look",
        children: [
          {
            kind: "path",
            d: "M-25 -15 L-12 -7.5 L-25 0 M25 -15 L12 -7.5 L25 0",
            className: "bolu-ln",
            strokeWidth: 5,
          },
        ],
      },
      { kind: "path", d: "M-9 13 Q-4.5 19 0 13 Q4.5 19 9 13", className: "bolu-ln", strokeWidth: 4 },
    ],
  },
  cinta: {
    label: "Jatuh cinta",
    say: "lagi jatuh cinta",
    anim: "a-love",
    blink: false,
    blush: 0.85,
    face: ({ heart }) => [
      heart(-17),
      heart(17),
      { kind: "path", d: "M-9 12 Q0 20 9 12", className: "bolu-ln", strokeWidth: 4.5 },
      {
        kind: "path",
        className: "bolu-float",
        fill: "#ff3d6e",
        d: "M44 -38 C37 -42 38 -49 41 -48 C43 -48 44 -46 44 -45 C44 -46 45 -48 47 -48 C50 -49 51 -42 44 -38 Z",
      },
    ],
  },
};

/** A clip path an expression asked the eye builder for. */
export type BoluClipPath = { id: string; points: string };

/**
 * Everything a renderer needs to draw one character, with no geometry left to
 * compute. `clipPaths` and `blush.id` are per-instance: pass a unique
 * `idPrefix` when several characters share a document.
 */
export type BoluDescription = {
  viewBox: string;
  width: number;
  height: number;
  /** Face and line colour, exposed as `--bolu-ink` on the SVG. */
  ink: string;
  /** Class on the animated group; `null` when the character is frozen. */
  animation: string | null;
  clipPaths: BoluClipPath[];
  blush: { id: string; color: string; opacity: number };
  feet: { className: string; color: string; d: string } | null;
  body: { className: string; color: string; strokeWidth: number; elements: BoluElement[] };
  face: { className: string; transform: string; elements: BoluElement[] };
};

export type BoluDescriptionOptions = {
  shape: BoluShapeName;
  expression: BoluExpressionName;
  color: string;
  /** Draw the little feet. Defaults to true. */
  feet?: boolean;
  /** Freeze the character's animation. */
  still?: boolean;
  /** Prefix for the per-instance clip and gradient ids. Defaults to `bolu`. */
  idPrefix?: string;
};

/**
 * The reference's `charSVG()`: describe a character as data. Pure — the same
 * options always produce the same description.
 */
export function charSVG({
  shape,
  expression,
  color,
  feet = true,
  still = false,
  idPrefix = "bolu",
}: BoluDescriptionOptions): BoluDescription {
  // Data lama atau data asing dari luar tidak boleh menjatuhkan seluruh layar (#564).
  // Jika shape atau expression tidak dikenali, jatuh ke default yang sah.
  const body = BOLU_SHAPES[shape] ?? BOLU_SHAPES[BOLU_DEFAULT_SHAPE];
  const expressionBody = BOLU_EXPRESSIONS[expression] ?? BOLU_EXPRESSIONS[BOLU_DEFAULT_EXPRESSION];

  const restingLook = expressionBody.look ?? [0, 0];
  const clipPaths: BoluClipPath[] = [];
  let clipSeq = 0;

  const eye: BoluFaceContext["eye"] = (
    cx,
    { rx = 10.5, ry = 11.5, lid = null, pr = 7 }: BoluEyeOptions = {},
  ) => {
    let clip = "";
    let lidLine: BoluElement | null = null;
    if (lid) {
      // The lid clips the eye from the outer corner down to the inner one.
      const side = Math.sign(cx);
      const outer = cx + side * (rx + 2);
      const inner = cx - side * (rx + 2);
      const id = `${idPrefix}c${clipSeq++}`;
      clipPaths.push({
        id,
        points: `${outer},${lid[0]} ${inner},${lid[1]} ${inner},20 ${outer},20`,
      });
      clip = `url(#${id})`;
      lidLine = {
        kind: "path",
        d: `M${outer} ${lid[0]} L${inner} ${lid[1]}`,
        className: "bolu-ln",
        strokeWidth: 4,
      };
    }

    const children: BoluElement[] = [
      {
        kind: "group",
        clipPath: clip || undefined,
        children: [
          { kind: "ellipse", cx, cy: -8, rx, ry, fill: "#fff" },
          {
            kind: "group",
            className: "bolu-look",
            transform: `translate(${restingLook[0]} ${restingLook[1]})`,
            children: [
              { kind: "circle", cx, cy: -7, r: pr, fill: "var(--bolu-ink)" },
              { kind: "circle", cx: cx + pr * 0.4, cy: -7 - pr * 0.45, r: pr * 0.42, fill: "#fff" },
              { kind: "circle", cx: cx - pr * 0.42, cy: -7 + pr * 0.4, r: pr * 0.2, fill: "#fff" },
            ],
          },
        ],
      },
    ];
    if (lidLine) children.push(lidLine);

    return {
      kind: "group",
      className: expressionBody.blink && !still ? "bolu-eye bolu-blink" : "bolu-eye",
      children,
    };
  };

  const heart: BoluFaceContext["heart"] = (cx) => ({
    kind: "group",
    className: "bolu-eye",
    children: [
      {
        kind: "group",
        className: "bolu-look",
        children: [
          {
            kind: "group",
            className: "bolu-pulse",
            children: [
              {
                kind: "path",
                fill: "#ff3d6e",
                d: `M${cx} 0 C${cx - 11} -7 ${cx - 10} -17 ${cx - 4} -16 C${cx - 1.5} -15 ${cx} -13 ${cx} -11.5 C${cx} -13 ${cx + 1.5} -15 ${cx + 4} -16 C${cx + 10} -17 ${cx + 11} -7 ${cx} 0 Z`,
              },
            ],
          },
        ],
      },
    ],
  });

  const tear: BoluFaceContext["tear"] = (cx, delay) => ({
    kind: "path",
    className: `bolu-tear ${delay === "t2" ? "bolu-t2" : ""}`,
    fill: "#9ad8ff",
    d: `M${cx} 4 Q${cx + 4} 10 ${cx + 4} 13 A4 4 0 0 1 ${cx - 4} 13 Q${cx - 4} 10 ${cx} 4 Z`,
  });

  const face = expressionBody.face({ eye, heart, tear });
  const [faceX, faceY, faceScale] = body.face;

  return {
    viewBox: BOLU_VIEW_BOX,
    width: BOLU_WIDTH,
    height: BOLU_HEIGHT,
    ink: inkFor(hexToHsl(color)[2]),
    animation: still ? null : `bolu-anim ${expressionBody.anim}`,
    clipPaths,
    blush: { id: `${idPrefix}g`, color: "#ff5a86", opacity: expressionBody.blush ?? 0.55 },
    feet:
      feet && body.foot
        ? { className: "bolu-feet", color: feetColorFor(color), d: footPath(body.foot) }
        : null,
    body: { className: "bolu-body", color, strokeWidth: body.sw, elements: body.body },
    face: {
      className: "bolu-face",
      transform: `translate(${faceX} ${faceY + 3}) scale(${faceScale})`,
      elements: face,
    },
  };
}
