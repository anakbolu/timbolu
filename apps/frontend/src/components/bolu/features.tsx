import { FEATURES } from "@/lib/crew";

function FeatureIcon({ kind }: { kind: (typeof FEATURES)[number]["icon"] }) {
  const shared = {
    width: 34,
    height: 34,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "#1E1B2E",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
  };

  if (kind === "check") {
    return (
      <svg {...shared}>
        <rect x="4" y="3" width="16" height="18" rx="3" />
        <path d="M8.5 12.5l2.5 2.5 4.5-5" />
      </svg>
    );
  }

  if (kind === "clock") {
    return (
      <svg {...shared}>
        <circle cx="12" cy="12" r="9" />
        <path d="M12 7v5l3 2" />
      </svg>
    );
  }

  return (
    <svg {...shared}>
      <path d="M4 6h16M4 12h16M4 18h10" />
    </svg>
  );
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
            className="flex flex-col gap-2.5 rounded border border-bolu-border bg-white p-[26px]"
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
