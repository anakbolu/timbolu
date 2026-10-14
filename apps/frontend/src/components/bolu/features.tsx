import { AlignLeft, ClipboardCheck, Clock } from "lucide-react";

import { FEATURES } from "@/lib/crew";

function FeatureIcon({ kind }: { kind: (typeof FEATURES)[number]["icon"] }) {
  const shared = {
    width: 34,
    height: 34,
    stroke: "#1E1B2E",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
  };

  if (kind === "check") {
    return <ClipboardCheck {...shared} />;
  }

  if (kind === "clock") {
    return <Clock {...shared} />;
  }

  return <AlignLeft {...shared} />;
}

export function Features() {
  return (
    <section
      aria-labelledby="kendali-title"
      className="mx-auto flex w-full max-w-[1248px] flex-col gap-8 px-6 pt-28"
    >
      <h2
        id="kendali-title"
        className="max-w-[720px] font-display text-[clamp(34px,4.6vw,52px)] font-bold leading-[1.08]"
      >
        Bolu yang siapkan, kamu yang ketok palu
      </h2>

      <ul className="grid list-none grid-cols-[repeat(auto-fit,minmax(260px,1fr))] gap-[18px] p-0">
        {FEATURES.map((feature) => (
          <li
            key={feature.title}
            className="flex flex-col gap-2.5 rounded-md border border-bolu-border bg-white p-[26px]"
          >
            <FeatureIcon kind={feature.icon} />
            <div className="font-display text-[22px] font-semibold">{feature.title}</div>
            <p className="text-bolu-muted">{feature.text}</p>
          </li>
        ))}
      </ul>
    </section>
  );
}
