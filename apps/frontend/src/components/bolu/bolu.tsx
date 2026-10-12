"use client";

/**
 * The bolu generator: draws a character from the "Bolu — karakter blob
 * ekspresif" reference — 16 body shapes, 10 expressions and a colour, with
 * optional feet. Everything here is a faithful port of that reference's
 * `charSVG()`: the same viewBox, the same face origin, the same palette.
 *
 * `<Bolu />`                 a default bolu: kacang, senang, Oren orange
 * `<Bolu expression="mikir" />`
 * `<Bolu shape="bintang" color="#2E9BEF" expression="kaget" />`
 * `<Bolu member={bot("Ijo")} expression="mikir" />`
 */

import { useId, type CSSProperties, type ReactNode } from "react";

import {
  BOLU_EXPRESSIONS,
  BOLU_SHAPES,
  feetColorFor,
  hexToHsl,
  inkFor,
  type BoluExpressionName,
  type BoluEyeOptions,
  type BoluFaceContext,
  type BoluShapeName,
} from "@/components/bolu/characters";
import type { CrewMember } from "@/lib/crew";

/** Colours that read as a dark body, and so use light ink. */
const DEFAULT_COLOR = "#F28C4E";
const DEFAULT_SHAPE: BoluShapeName = "kacang";
const DEFAULT_EXPRESSION: BoluExpressionName = "senang";

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

/**
 * The two flat ellipse feet, drawn just above the shape's baseline.
 * A port of the reference's `footPath()`.
 */
function footPath(baseline: number) {
  const y = baseline - 3;
  return `M70 ${y} a12 8 0 1 0 24 0 a12 8 0 1 0 -24 0 Z M106 ${y} a12 8 0 1 0 24 0 a12 8 0 1 0 -24 0 Z`;
}

export function Bolu({
  shape,
  expression = DEFAULT_EXPRESSION,
  color,
  member,
  feet = true,
  size = 120,
  className,
  title,
  still,
}: BoluProps) {
  const expressionBody = BOLU_EXPRESSIONS[expression];
  const shapeName = (shape ?? member?.shape ?? DEFAULT_SHAPE) as BoluShapeName;
  const body = BOLU_SHAPES[shapeName];
  if (!body) throw new Error(`Unknown bolu shape: ${shapeName}`);

  const bodyColor = color ?? member?.color ?? DEFAULT_COLOR;
  const lightness = hexToHsl(bodyColor)[2];
  const restingLook = expressionBody.look ?? [0, 0];

  // Clip paths and gradients are per-instance, so their ids must be too.
  const clipPrefix = useId().replace(/[^a-zA-Z0-9]/g, "");
  const clips: ReactNode[] = [];
  let clipSeq = 0;

  const eye: BoluFaceContext["eye"] = (
    cx: number,
    { rx = 10.5, ry = 11.5, lid = null, pr = 7 }: BoluEyeOptions = {},
  ) => {
    let clip = "";
    let lidLine: ReactNode = null;
    if (lid) {
      // The lid clips the eye from the outer corner down to the inner one.
      const side = Math.sign(cx);
      const outer = cx + side * (rx + 2);
      const inner = cx - side * (rx + 2);
      const id = `${clipPrefix}c${clipSeq++}`;
      clips.push(
        <clipPath id={id} key={id}>
          <polygon points={`${outer},${lid[0]} ${inner},${lid[1]} ${inner},20 ${outer},20`} />
        </clipPath>,
      );
      clip = `url(#${id})`;
      lidLine = (
        <path
          d={`M${outer} ${lid[0]} L${inner} ${lid[1]}`}
          className="bolu-ln"
          strokeWidth={4}
        />
      );
    }

    const eyeClass = expressionBody.blink && !still ? "bolu-eye bolu-blink" : "bolu-eye";
    return (
      <g className={eyeClass} key={cx}>
        <g clipPath={clip || undefined}>
          <ellipse cx={cx} cy={-8} rx={rx} ry={ry} fill="#fff" />
          <g className="bolu-look" transform={`translate(${restingLook[0]} ${restingLook[1]})`}>
            <circle cx={cx} cy={-7} r={pr} fill="var(--bolu-ink)" />
            <circle cx={cx + pr * 0.4} cy={-7 - pr * 0.45} r={pr * 0.42} fill="#fff" />
            <circle cx={cx - pr * 0.42} cy={-7 + pr * 0.4} r={pr * 0.2} fill="#fff" />
          </g>
        </g>
        {lidLine}
      </g>
    );
  };

  const heart: BoluFaceContext["heart"] = (cx) => (
    <g className="bolu-eye" key={cx}>
      <g className="bolu-look">
        <g className="bolu-pulse">
          <path
            fill="#ff3d6e"
            d={`M${cx} 0 C${cx - 11} -7 ${cx - 10} -17 ${cx - 4} -16 C${cx - 1.5} -15 ${cx} -13 ${cx} -11.5 C${cx} -13 ${cx + 1.5} -15 ${cx + 4} -16 C${cx + 10} -17 ${cx + 11} -7 ${cx} 0 Z`}
          />
        </g>
      </g>
    </g>
  );

  const tear: BoluFaceContext["tear"] = (cx, delay) => (
    <path
      key={`${cx}${delay ?? ""}`}
      className={`bolu-tear ${delay === "t2" ? "bolu-t2" : ""}`}
      fill="#9ad8ff"
      d={`M${cx} 4 Q${cx + 4} 10 ${cx + 4} 13 A4 4 0 0 1 ${cx - 4} 13 Q${cx - 4} 10 ${cx} 4 Z`}
    />
  );

  const face = expressionBody.face({ eye, heart, tear });

  const blushOpacity = expressionBody.blush ?? 0.55;
  const blushId = `${clipPrefix}g`;
  const [faceX, faceY, faceScale] = body.face;

  return (
    <div
      className={className}
      style={size === "fill" ? { aspectRatio: "1 / 1" } : { width: size, height: size }}
    >
      <svg
        viewBox="0 0 200 200"
        className="block size-full overflow-visible"
        role={title ? "img" : undefined}
        aria-label={title}
        aria-hidden={title ? undefined : true}
        style={{ "--bolu-ink": inkFor(lightness) } as CSSProperties}
      >
        {title ? <title>{title}</title> : null}
        <defs>
          {clips}
          <radialGradient id={blushId}>
            <stop offset="0" stopColor="#ff5a86" stopOpacity={blushOpacity} />
            <stop offset="1" stopColor="#ff5a86" stopOpacity={0} />
          </radialGradient>
        </defs>

        <g className={still ? undefined : `bolu-anim ${expressionBody.anim}`}>
          {feet && body.foot ? (
            <g className="bolu-feet" fill={feetColorFor(bodyColor)}>
              <path d={footPath(body.foot)} />
            </g>
          ) : null}

          <g
            className="bolu-body"
            fill={bodyColor}
            stroke={bodyColor}
            strokeWidth={body.sw}
            strokeLinejoin="round"
          >
            {body.body}
          </g>

          <g
            className="bolu-face"
            transform={`translate(${faceX} ${faceY + 3}) scale(${faceScale})`}
          >
            <ellipse cx="-31" cy="9" rx="11" ry="7" fill={`url(#${blushId})`} />
            <ellipse cx="31" cy="9" rx="11" ry="7" fill={`url(#${blushId})`} />
            {face}
          </g>
        </g>
      </svg>
    </div>
  );
}
