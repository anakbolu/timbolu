/**
 * Locks the character generator to the geometry the web app rendered before the
 * move (#387). The tables below were copied from the old implementation in
 * `apps/frontend/src/components/bolu/characters.tsx`, not chosen by hand: a
 * rounded or "tidied" coordinate would change the drawing.
 *
 *   node --test packages/bolu-character/characters.test.ts
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  BOLU_EXPRESSIONS,
  BOLU_HEIGHT,
  BOLU_SHAPES,
  BOLU_VIEW_BOX,
  BOLU_WIDTH,
  charSVG,
  footPath,
} from "./characters.ts";
import { BOLU_EXPRESSION_NAMES, BOLU_SHAPE_NAMES, type BoluExpressionName, type BoluShapeName } from "./names.ts";

const SHAPE_GEOMETRY: Record<
  BoluShapeName,
  { sw: number; foot: number | null; face: [number, number, number] }
> = {
  kacang: { sw: 4, foot: 168, face: [100, 104, 1.1] },
  hantu: { sw: 6, foot: null, face: [98, 86, 1] },
  gumpal: { sw: 4, foot: 166, face: [100, 100, 1.15] },
  awan: { sw: 4, foot: 162, face: [102, 116, 1.05] },
  daun: { sw: 4, foot: null, face: [98, 98, 1.05] },
  percik: { sw: 4, foot: null, face: [104, 108, 1] },
  bintang: { sw: 36, foot: null, face: [100, 112, 0.95] },
  genangan: { sw: 4, foot: null, face: [104, 128, 0.9] },
  melambai: { sw: 4, foot: 172, face: [100, 96, 0.9] },
  tetes: { sw: 6, foot: 172, face: [100, 126, 1.05] },
  mochi: { sw: 4, foot: 172, face: [100, 124, 1.1] },
  bulat: { sw: 4, foot: 174, face: [100, 104, 1.15] },
  kotak: { sw: 4, foot: 172, face: [100, 106, 1.2] },
  segitiga: { sw: 40, foot: 170, face: [100, 124, 0.85] },
  hati: { sw: 6, foot: null, face: [100, 104, 0.95] },
  kucing: { sw: 6, foot: 176, face: [100, 114, 1.05] },
};

const EXPRESSION_META: Record<
  BoluExpressionName,
  { anim: string; blink: boolean; look?: [number, number]; blush?: number }
> = {
  senang: { anim: "a-bounce", blink: false, blush: 0.75 },
  iseng: { anim: "a-wiggle", blink: false },
  bosan: { anim: "a-sway", blink: true, look: [-3, 1.5] },
  mikir: { anim: "a-calm", blink: true, look: [3, -3.5] },
  marah: { anim: "a-shake", blink: false },
  capek: { anim: "a-sag", blink: true, look: [0, 2] },
  kaget: { anim: "a-jump", blink: false },
  sedih: { anim: "a-calm", blink: true, look: [0, 2.5] },
  gemas: { anim: "a-wiggle", blink: false, blush: 0.95 },
  cinta: { anim: "a-love", blink: false, blush: 0.85 },
};

test("the registry covers every name, in the reference's order", () => {
  assert.deepEqual(Object.keys(BOLU_SHAPES), BOLU_SHAPE_NAMES);
  assert.deepEqual(Object.keys(BOLU_EXPRESSIONS), BOLU_EXPRESSION_NAMES);
});

test("every shape keeps its stroke width, feet baseline and face origin", () => {
  for (const [shape, expected] of Object.entries(SHAPE_GEOMETRY)) {
    const actual = BOLU_SHAPES[shape as BoluShapeName];
    assert.equal(actual.sw, expected.sw, shape);
    assert.equal(actual.foot, expected.foot, shape);
    assert.deepEqual(actual.face, expected.face, shape);
    assert.ok(actual.body.length > 0, shape);
  }
});

test("every expression keeps its animation, blink, gaze and blush", () => {
  for (const [expression, expected] of Object.entries(EXPRESSION_META)) {
    const actual = BOLU_EXPRESSIONS[expression as BoluExpressionName];
    assert.equal(actual.anim, expected.anim, expression);
    assert.equal(actual.blink, expected.blink, expression);
    assert.deepEqual(actual.look, expected.look, expression);
    assert.equal(actual.blush, expected.blush, expression);
  }
});

test("every shape × expression yields a deterministic description", () => {
  for (const shape of BOLU_SHAPE_NAMES) {
    for (const expression of BOLU_EXPRESSION_NAMES) {
      const first = charSVG({ shape, expression, color: "#F28C4E", idPrefix: "t" });
      const second = charSVG({ shape, expression, color: "#F28C4E", idPrefix: "t" });
      const label = `${shape}/${expression}`;

      assert.deepEqual(first, second, label);
      assert.equal(first.viewBox, "0 0 200 200", label);
      assert.equal(first.width, 200, label);
      assert.equal(first.height, 200, label);
      assert.ok(first.body.elements.length > 0, label);
      assert.ok(first.face.elements.length > 0, label);
      assert.equal(first.blush.id, "tg", label);
    }
  }
});

test("the viewBox and dimensions are the constants the web app rendered", () => {
  assert.equal(BOLU_VIEW_BOX, "0 0 200 200");
  assert.equal(BOLU_WIDTH, 200);
  assert.equal(BOLU_HEIGHT, 200);

  const description = charSVG({ shape: "kacang", expression: "senang", color: "#F28C4E" });
  assert.equal(description.viewBox, "0 0 200 200");
  assert.equal(description.width, 200);
  assert.equal(description.height, 200);
});

test("feet sit on the shape's baseline, and disappear when asked", () => {
  for (const shape of BOLU_SHAPE_NAMES) {
    const baseline = BOLU_SHAPES[shape].foot;
    const withFeet = charSVG({ shape, expression: "senang", color: "#F28C4E" });
    if (baseline === null) {
      assert.equal(withFeet.feet, null, shape);
    } else {
      assert.equal(withFeet.feet?.d, footPath(baseline), shape);
    }

    const withoutFeet = charSVG({ shape, expression: "senang", color: "#F28C4E", feet: false });
    assert.equal(withoutFeet.feet, null, shape);
  }
});

test("a frozen character has no animation class, a live one does", () => {
  const live = charSVG({ shape: "kacang", expression: "mikir", color: "#F28C4E" });
  assert.equal(live.animation, "bolu-anim a-calm");

  const frozen = charSVG({ shape: "kacang", expression: "mikir", color: "#F28C4E", still: true });
  assert.equal(frozen.animation, null);
});

test("clip ids are per instance and follow the prefix", () => {
  const first = charSVG({ shape: "bulat", expression: "marah", color: "#F28C4E", idPrefix: "a" });
  const second = charSVG({ shape: "bulat", expression: "marah", color: "#F28C4E", idPrefix: "b" });

  assert.deepEqual(
    first.clipPaths.map((clip) => clip.id),
    ["ac0", "ac1"],
  );
  assert.deepEqual(
    second.clipPaths.map((clip) => clip.id),
    ["bc0", "bc1"],
  );
  assert.equal(first.blush.id, "ag");
  assert.equal(second.blush.id, "bg");
});

test("the core imports no React and renders no JSX", () => {
  const core = ["characters.ts", "names.ts", "palette.ts"];
  for (const file of core) {
    const source = readFileSync(new URL(`./${file}`, import.meta.url), "utf8");
    assert.doesNotMatch(source, /from\s+["']react(-dom)?["']/, file);
    assert.doesNotMatch(source, /require\(\s*["']react/, file);
    assert.doesNotMatch(source, /React\.createElement/, file);
    assert.doesNotMatch(source, /<\/[A-Za-z]/, file);
  }
});
