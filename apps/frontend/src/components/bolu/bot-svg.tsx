import type { CSSProperties } from "react";
import type { BotShape } from "@/lib/crew";

type BotSvgProps = {
  bot: BotShape;
  /** Wrapper sizing/animation classes, e.g. `bob size-26`. */
  className?: string;
  /** Extra wrapper styles; `animationDelay` drives bob/blink/glance. */
  style?: CSSProperties;
  /**
   * Accessible name. Give one when the bolu carries meaning on its own;
   * without it the character is decorative and hidden from screen readers.
   */
  title?: string;
};

/**
 * The Keluarga Bolu character: body path + feet + face (cheeks, blinking eyes,
 * glancing pupils, mouth). Ported 1:1 from the design's `mk()` renderer.
 * Decorative by default — the surrounding copy carries the meaning.
 */
export function BotSvg({ bot, className, style, title }: BotSvgProps) {
  return (
    <div className={className} style={style}>
      <svg
        viewBox="0 0 200 200"
        role={title ? "img" : undefined}
        aria-label={title}
        aria-hidden={title ? undefined : true}
        className="block size-full overflow-visible"
      >
        {title ? <title>{title}</title> : null}
        {bot.hasFeet ? <path d={bot.feet} fill={bot.feetColor} /> : null}
        <path
          d={bot.d}
          fill={bot.color}
          stroke={bot.color}
          strokeWidth={bot.sw}
          strokeLinejoin="round"
        />
        <g transform={bot.face}>
          <ellipse cx="-31" cy="9" rx="11" ry="6.5" fill="#FF5A86" opacity="0.3" />
          <ellipse cx="31" cy="9" rx="11" ry="6.5" fill="#FF5A86" opacity="0.3" />
          {bot.eyesClosed ? (
            <path
              d="M-26 -6 Q-17 1 -8 -6 M8 -6 Q17 1 26 -6"
              fill="none"
              stroke="#1E1B2E"
              strokeWidth={4.5}
              strokeLinecap="round"
            />
          ) : bot.eyesLazy ? (
            <>
              <path
                d="M-27.5 -7 A10.5 10 0 0 0 -6.5 -7 Z M6.5 -7 A10.5 10 0 0 0 27.5 -7 Z"
                fill="#FFFFFF"
              />
              <path d="M-22 -7 A6 6 0 0 0 -10 -7 Z M12 -7 A6 6 0 0 0 24 -7 Z" fill="#1E1B2E" />
              <circle cx="-13.5" cy="-4.5" r="1.7" fill="#FFFFFF" />
              <circle cx="20.5" cy="-4.5" r="1.7" fill="#FFFFFF" />
              <path
                d="M-29 -8 Q-17 -11 -5 -8 M5 -8 Q17 -11 29 -8"
                fill="none"
                stroke="#1E1B2E"
                strokeWidth={4.5}
                strokeLinecap="round"
              />
            </>
          ) : (
            <g className="blink" style={{ animationDelay: bot.delay }}>
              <ellipse cx="-17" cy="-8" rx="10.5" ry="11.5" fill="#FFFFFF" />
              <ellipse cx="17" cy="-8" rx="10.5" ry="11.5" fill="#FFFFFF" />
              <g className="glance" style={{ animationDelay: bot.delay }}>
                <g transform={bot.look}>
                  <circle cx="-17" cy="-7" r="7" fill="#1E1B2E" />
                  <circle cx="17" cy="-7" r="7" fill="#1E1B2E" />
                  <circle cx="-14.2" cy="-10.2" r="2.9" fill="#FFFFFF" />
                  <circle cx="19.8" cy="-10.2" r="2.9" fill="#FFFFFF" />
                  <circle cx="-19.9" cy="-4.2" r="1.4" fill="#FFFFFF" />
                  <circle cx="14.1" cy="-4.2" r="1.4" fill="#FFFFFF" />
                </g>
              </g>
            </g>
          )}
          <path
            d={bot.mouth}
            fill={bot.mouthFill}
            stroke="#1E1B2E"
            strokeWidth={bot.mouthSw}
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          {bot.chillFx ? (
            <>
              <g className="steam">
                <path
                  d="M38 6 Q34 0 38 -6 Q42 -12 38 -18 M47 8 Q43 2 47 -4"
                  fill="none"
                  stroke="#B9BDD3"
                  strokeWidth={3}
                  strokeLinecap="round"
                />
              </g>
              <path
                d="M30 12 L56 12 L53 36 Q52 40 48 40 L38 40 Q34 40 33 36 Z"
                fill="#FFFFFF"
                stroke="#1E1B2E"
                strokeWidth={3.5}
                strokeLinejoin="round"
              />
              <path
                d="M55 17 Q65 18 63 26 Q61 32 53 31"
                fill="none"
                stroke="#1E1B2E"
                strokeWidth={3.5}
                strokeLinecap="round"
              />
              <path
                d="M33 19 L53 19"
                stroke="#C98A5B"
                strokeWidth={5}
                strokeLinecap="round"
              />
              <g className="note">
                <path
                  d="M-52 -46 L-52 -66 L-40 -70 L-40 -52"
                  fill="none"
                  stroke="#1E1B2E"
                  strokeWidth={3.5}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
                <ellipse cx="-55" cy="-46" rx="5" ry="4" fill="#1E1B2E" />
                <ellipse cx="-43" cy="-52" rx="5" ry="4" fill="#1E1B2E" />
              </g>
            </>
          ) : null}
        </g>
      </svg>
    </div>
  );
}
