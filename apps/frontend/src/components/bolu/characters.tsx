/**
 * The Bolu character API the web app has always imported.
 *
 * The generator moved to `packages/bolu-character` (#387): `characters.ts` holds
 * the shape and expression data plus `charSVG()`, and `dom.tsx` renders that
 * description as web SVG. This file stays as the frontend's stable import path,
 * so no page or component had to change.
 */

export type { BoluExpressionName, BoluShapeName } from "@timbolu/bolu-character/names";
export { BOLU_EXPRESSION_NAMES, BOLU_SHAPE_NAMES } from "@timbolu/bolu-character/names";
export { BOLU_SWATCHES, feetColorFor, hexToHsl, inkFor } from "@timbolu/bolu-character/palette";
export type {
  BoluExpression,
  BoluEyeOptions,
  BoluFaceContext,
  BoluShape,
} from "@timbolu/bolu-character/characters";
export { BOLU_EXPRESSIONS, BOLU_SHAPES } from "@timbolu/bolu-character/characters";
