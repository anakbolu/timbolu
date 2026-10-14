import { FAQ } from "@/lib/crew";

export function Faq() {
  return (
    <section
      id="tanya"
      aria-labelledby="tanya-title"
      className="mx-auto flex w-full max-w-[908px] flex-col gap-5 px-6 pt-28"
    >
      <h2
        id="tanya-title"
        className="font-display text-[clamp(34px,4.6vw,52px)] font-bold leading-[1.08]"
      >
        Yang sering ditanyakan
      </h2>

      {FAQ.map((item) => (
        <details
          key={item.q}
          className="rounded-md border border-bolu-border bg-white px-6 py-5"
        >
          <summary className="font-display text-[20px] font-semibold">{item.q}</summary>
          <p className="mt-3 text-bolu-muted">{item.a}</p>
        </details>
      ))}
    </section>
  );
}
