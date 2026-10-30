"use client";

/**
 * The bolu component: resolves a crew member's body and colour, then hands the
 * drawing to the package's web adapter.
 *
 * The generator itself — 16 shapes, 10 expressions, the feet, the viewBox —
 * lives in `packages/bolu-character` as data (`characters.ts`) plus the SVG DOM
 * adapter (`dom.tsx`), so the Expo app can render the same character later
 * (#302). This file only knows the frontend's `CrewMember`.
 *
 * `<Bolu />`                 a default bolu: kacang, senang, Oren orange
 * `<Bolu expression="mikir" />`
 * `<Bolu shape="bintang" color="#2E9BEF" expression="kaget" />`
 * `<Bolu member={bot("Ijo")} expression="mikir" />`
 */

import {
  BOLU_DEFAULT_COLOR,
  BOLU_DEFAULT_EXPRESSION,
  BOLU_DEFAULT_SHAPE,
} from "@timbolu/bolu-character/characters";
import { BoluCharacter } from "@timbolu/bolu-character/dom";
import type { BoluExpressionName, BoluShapeName } from "@timbolu/bolu-character/names";

import type { CrewMember } from "@/lib/crew";

type BoluProps = {
  /** Body shape. Defaults to the member's own, or `kacang`. */
  shape?: BoluShapeName;
  /** Expression. Defaults to `senang`. */
  expression?: BoluExpressionName;
  /** Body colour; feet and ink are derived from it. */
  color?: string;
  /**
   * Draw a crew member: its body shape, colour and animation phase. Explicit
   * props still win, so `member` plus `expression` poses the member.
   */
  member?: CrewMember;
  /** Draw the little feet. Defaults to true. */
  feet?: boolean;
  /** Square size in pixels, or `"fill"` to fill the parent. Defaults to 120. */
  size?: number | "fill";
  /** Extra classes for the wrapper, e.g. layout utilities. */
  className?: string;
  /** Accessible name. Omitted → decorative (hidden from screen readers). */
  title?: string;
  /** Freeze the character's animation. */
  still?: boolean;
};

export function Bolu({
  shape,
  expression = BOLU_DEFAULT_EXPRESSION,
  color,
  member,
  feet = true,
  size = 120,
  className,
  title,
  still,
}: BoluProps) {
  const shapeName = (shape ?? member?.shape ?? BOLU_DEFAULT_SHAPE) as BoluShapeName;
  const bodyColor = color ?? member?.color ?? BOLU_DEFAULT_COLOR;

  return (
    <BoluCharacter
      shape={shapeName}
      expression={expression}
      color={bodyColor}
      feet={feet}
      size={size}
      className={className}
      title={title}
      still={still}
    />
  );
}
