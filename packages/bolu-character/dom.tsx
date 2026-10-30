"use client";

/**
 * The web adapter: maps a `BoluDescription` to SVG DOM.
 *
 * Every element here is a one-to-one mapping of the descriptor — the adapter
 * never computes a coordinate or a colour. The Expo app will write the same
 * mapping against `react-native-svg`; the descriptor is the contract (#302).
 */

import { useId, type CSSProperties, type ReactNode } from "react";

import {
  BOLU_DEFAULT_COLOR,
  BOLU_DEFAULT_EXPRESSION,
  BOLU_DEFAULT_SHAPE,
  charSVG,
  type BoluElement,
} from "./characters.ts";
import type { BoluExpressionName, BoluShapeName } from "./names.ts";

export type BoluCharacterProps = {
  /** Body shape. Defaults to `kacang`. */
  shape?: BoluShapeName;
  /** Expression. Defaults to `senang`. */
  expression?: BoluExpressionName;
  /** Body colour; feet and ink are derived from it. */
  color?: string;
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

function renderElement(element: BoluElement, key: number): ReactNode {
  switch (element.kind) {
    case "path":
      return (
        <path
          key={key}
          d={element.d}
          className={element.className}
          fill={element.fill}
          stroke={element.stroke}
          strokeWidth={element.strokeWidth}
          strokeLinecap={element.strokeLinecap}
        />
      );
    case "circle":
      return (
        <circle
          key={key}
          cx={element.cx}
          cy={element.cy}
          r={element.r}
          className={element.className}
          fill={element.fill}
        />
      );
    case "ellipse":
      return (
        <ellipse key={key} cx={element.cx} cy={element.cy} rx={element.rx} ry={element.ry} fill={element.fill} />
      );
    case "rect":
      return (
        <rect
          key={key}
          x={element.x}
          y={element.y}
          width={element.width}
          height={element.height}
          rx={element.rx}
          fill={element.fill}
          stroke={element.stroke}
          strokeWidth={element.strokeWidth}
        />
      );
    case "polygon":
      return <polygon key={key} points={element.points} />;
    case "group":
      return (
        <g
          key={key}
          className={element.className}
          transform={element.transform}
          fill={element.fill}
          clipPath={element.clipPath}
        >
          {element.children.map(renderElement)}
        </g>
      );
  }
}

/** Draw a Bolu character as web SVG. */
export function BoluCharacter({
  shape = BOLU_DEFAULT_SHAPE,
  expression = BOLU_DEFAULT_EXPRESSION,
  color = BOLU_DEFAULT_COLOR,
  feet = true,
  size = 120,
  className,
  title,
  still,
}: BoluCharacterProps) {
  // Clip paths and gradients are per-instance, so their ids must be too.
  const idPrefix = useId().replace(/[^a-zA-Z0-9]/g, "");
  const description = charSVG({ shape, expression, color, feet, still, idPrefix });
  const blushFill = `url(#${description.blush.id})`;

  return (
    <div
      className={className}
      style={size === "fill" ? { aspectRatio: "1 / 1" } : { width: size, height: size }}
    >
      <svg
        viewBox={description.viewBox}
        className="block size-full overflow-visible"
        role={title ? "img" : undefined}
        aria-label={title}
        aria-hidden={title ? undefined : true}
        style={{ "--bolu-ink": description.ink } as CSSProperties}
      >
        {title ? <title>{title}</title> : null}
        <defs>
          {description.clipPaths.map((clip) => (
            <clipPath id={clip.id} key={clip.id}>
              <polygon points={clip.points} />
            </clipPath>
          ))}
          <radialGradient id={description.blush.id}>
            <stop offset="0" stopColor={description.blush.color} stopOpacity={description.blush.opacity} />
            <stop offset="1" stopColor={description.blush.color} stopOpacity={0} />
          </radialGradient>
        </defs>

        <g className={description.animation ?? undefined}>
          {description.feet ? (
            <g className={description.feet.className} fill={description.feet.color}>
              <path d={description.feet.d} />
            </g>
          ) : null}

          <g
            className={description.body.className}
            fill={description.body.color}
            stroke={description.body.color}
            strokeWidth={description.body.strokeWidth}
            strokeLinejoin="round"
          >
            {description.body.elements.map(renderElement)}
          </g>

          <g className={description.face.className} transform={description.face.transform}>
            <ellipse cx="-31" cy="9" rx="11" ry="7" fill={blushFill} />
            <ellipse cx="31" cy="9" rx="11" ry="7" fill={blushFill} />
            {description.face.elements.map(renderElement)}
          </g>
        </g>
      </svg>
    </div>
  );
}
