/**
 * The names a Bolu character can be given, shared by web and app.
 *
 * Only the names live here for now. The geometry behind each name is still JSX
 * in `apps/frontend/src/components/bolu/characters.tsx`; #302 replaces it with
 * an element descriptor model plus a `react-native-svg` adapter, and only then
 * does a renderer stop needing the web tree.
 *
 * Order is part of the contract: both lists are what the shape and expression
 * pickers walk, so they stay in the reference's order.
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
