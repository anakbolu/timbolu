/**
 * The Keluarga Bolu brand mark: the crew crowded together in front of the
 * wordmark. The auth screens stack it above the card; the landing header sets it
 * inline beside the nav. Both draw the same five characters, in the same poses
 * and the same order, so the brand reads the same wherever it appears.
 */

import { Bolu } from "@/components/bolu/bolu";
import type { BoluExpressionName } from "@/components/bolu/characters";
import { bot } from "@/lib/crew";

/**
 * The crew, in the order they crowd together. `scale` is relative to the tallest
 * member (Oren, in the middle), so a caller picks one height and the rest follow.
 */
const CREW: { name: string; expression: BoluExpressionName; scale: number }[] = [
  { name: "Ijo", expression: "senang", scale: 0.625 },
  { name: "Pinky", expression: "gemas", scale: 0.75 },
  { name: "Oren", expression: "mikir", scale: 1 },
  { name: "Lila", expression: "kaget", scale: 0.75 },
  { name: "Biru", expression: "cinta", scale: 0.625 },
];

type KeluargaBoluMarkProps = {
  /** Height in px of the tallest character; the others keep their ratio. */
  size?: number;
  /** `stacked` centres the crew above the wordmark; `inline` sets it beside. */
  layout?: "stacked" | "inline";
  /** The visible wordmark. The landing header writes it lower-case. */
  wordmark?: string;
  /** Type scale for the wordmark, e.g. `text-[26px]`. */
  wordmarkClassName?: string;
  className?: string;
};

export function KeluargaBoluMark({
  size = 48,
  layout = "stacked",
  wordmark = "Keluarga Bolu",
  wordmarkClassName = "text-[26px]",
  className,
}: KeluargaBoluMarkProps) {
  return (
    <span
      className={`flex ${
        layout === "inline" ? "items-center gap-2 sm:gap-2.5" : "flex-col items-center gap-2"
      } ${className ?? ""}`}
    >
      <span aria-hidden="true" className="flex items-end -space-x-3">
        {CREW.map((member) => (
          <Bolu
            key={member.name}
            member={bot(member.name)}
            expression={member.expression}
            size={Math.round(size * member.scale)}
            className={member.scale === 1 ? "relative z-10" : undefined}
          />
        ))}
      </span>
      <span className={`font-display font-bold ${wordmarkClassName}`}>{wordmark}</span>
    </span>
  );
}
