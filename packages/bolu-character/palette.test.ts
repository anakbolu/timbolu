/**
 * Locks the palette helpers to the values the web app rendered before they
 * moved into this package (#302). The expected values were computed from the
 * old implementation in `apps/frontend/src/components/bolu/characters.tsx`,
 * not chosen by hand: a rounded or "tidied" copy would change the SVG.
 *
 *   node --test packages/bolu-character/palette.test.ts
 */
import assert from "node:assert/strict";
import test from "node:test";

import { BOLU_SWATCHES, feetColorFor, hexToHsl, inkFor } from "./palette.ts";

test("hexToHsl keeps the reference's unrounded HSL", () => {
  assert.deepEqual(hexToHsl("#F4A259"), [28.25806451612903, 87.57062146892657, 65.29411764705883]);
  assert.deepEqual(hexToHsl("#000000"), [0, 0, 0]);
  assert.deepEqual(hexToHsl("#FFFFFF"), [0, 0, 100]);
});

test("inkFor flips to light ink below 32% lightness", () => {
  assert.equal(inkFor(20), "#FFF6EA");
  assert.equal(inkFor(80), "#1E1B2E");
  assert.equal(inkFor(31.999), "#FFF6EA");
  assert.equal(inkFor(32), "#1E1B2E");
});

test("feetColorFor drops lightness by 12 and never below 8", () => {
  assert.equal(feetColorFor("#F4A259"), "hsl(28.25806451612903 87.57062146892657% 53.294117647058826%)");
  assert.equal(feetColorFor("#1E1B2E"), "hsl(249.47368421052633 26.027397260273972% 8%)");
  assert.equal(feetColorFor("#000000"), "hsl(0 0% 8%)");
});

test("BOLU_SWATCHES is the reference palette, in order", () => {
  assert.deepEqual(BOLU_SWATCHES, [
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
  ]);
});
