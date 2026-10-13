import type { Metadata } from "next";
import Link from "next/link";
import { SiteFooter } from "@/components/bolu/site-footer";
import { SiteNav } from "@/components/bolu/site-nav";

type Tier = {
  name: string;
  price: string;
  period?: string;
  tagline: string;
  features: string[];
  cta: { label: string; href?: string };
  featured?: boolean;
};

const TIERS: Tier[] = [
  {
    name: "Gratis",
    price: "Rp 0",
    period: "/ bulan",
    tagline: "Untuk mencoba dengan tim kecil.",
    features: [
      "3 anggota tim",
      "200 ribu token per periode",
      "Enam Bolu siap pakai",
      "Invoice dan balasan sebagai draf",
    ],
    cta: { label: "Coba gratis", href: "/dashboard" },
  },
  {
    name: "Keluarga",
    price: "Rp 149.000",
    period: "/ bulan",
    tagline: "Untuk bisnis yang sudah jalan.",
    features: [
      "6 anggota tim dan 6 Bolu",
      "2 juta token per bulan",
      "Tiga aplikasi terhubung",
      "Paket tambahan token saat menipis",
    ],
    cta: { label: "Mulai", href: "/signup" },
    featured: true,
  },
  {
    name: "Rombongan",
    price: "Rp 449.000",
    period: "/ bulan",
    tagline: "Untuk tim dan agensi.",
    features: [
      "25 anggota tim dan 20 Bolu",
      "8 juta token per bulan",
      "Sepuluh aplikasi terhubung",
      "Catatan aktivitas lengkap",
    ],
    cta: { label: "Mulai", href: "/signup" },
  },
];

export const metadata: Metadata = {
  title: "Harga",
  description:
    "Harga Keluarga Bolu: mulai gratis dengan 200 ribu token, naikkan paket saat bisnismu siap. Bayar lewat transfer bank atau virtual account Indonesia.",
};

export default function PricingPage() {
  return (
    <>
      <SiteNav />
      <main className="flex-1">
        <div className="mx-auto w-full max-w-[1248px] px-6 py-20">
          <div className="mx-auto max-w-[680px] text-center">
            <h1 className="font-display text-[clamp(38px,5vw,56px)] font-bold leading-[1.05]">
              Harga
            </h1>
            <p className="mt-5 text-[19px] text-bolu-muted">
              Mulai gratis dengan 3 anggota tim. Naikkan paket saat bisnismu siap —
              bayar dengan transfer bank atau virtual account Indonesia.
            </p>
          </div>

          <ul className="mt-14 grid list-none grid-cols-[repeat(auto-fit,minmax(280px,1fr))] gap-[18px] p-0">
            {TIERS.map((tier) => (
              <li
                key={tier.name}
                className={`flex flex-col rounded border bg-white p-7 ${
                  tier.featured ? "border-bolu-ink" : "border-bolu-border"
                }`}
              >
                <div className="flex items-center justify-between gap-3">
                  <h2 className="font-display text-[22px] font-semibold">{tier.name}</h2>
                  {tier.featured ? (
                    <span className="rounded-full border border-bolu-border px-2.5 py-1 text-[10px] uppercase tracking-wide text-bolu-muted">
                      Segera
                    </span>
                  ) : null}
                </div>

                <p className="mt-6 flex items-baseline gap-1.5">
                  <span className="font-display text-3xl font-bold">{tier.price}</span>
                  {tier.period ? (
                    <span className="text-sm text-bolu-muted">{tier.period}</span>
                  ) : null}
                </p>

                <p className="mt-3 text-sm text-bolu-muted">{tier.tagline}</p>

                <ul className="mt-7 flex-1 list-none space-y-3 p-0 text-[15px] text-bolu-body">
                  {tier.features.map((feature) => (
                    <li key={feature} className="flex gap-3">
                      <svg
                        viewBox="0 0 20 20"
                        aria-hidden="true"
                        className="mt-0.5 size-4 shrink-0 text-bolu-muted"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="1.8"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      >
                        <path d="M4.5 10.5l3.5 3.5 7.5-8" />
                      </svg>
                      {feature}
                    </li>
                  ))}
                </ul>

                <div className="mt-8">
                  {tier.cta.href ? (
                    <Link
                      href={tier.cta.href}
                      className={`block w-full rounded-full px-5 py-3 text-center font-semibold no-underline ${
                        tier.featured
                          ? "bg-bolu-accent text-white hover:text-white"
                          : "border-2 border-bolu-ink text-bolu-ink hover:text-bolu-ink"
                      }`}
                    >
                      {tier.cta.label}
                    </Link>
                  ) : (
                    <span
                      aria-disabled="true"
                      className="block w-full cursor-not-allowed rounded-full border border-bolu-border px-5 py-3 text-center font-semibold text-bolu-muted"
                    >
                      {tier.cta.label}
                    </span>
                  )}
                </div>
              </li>
            ))}
          </ul>

          <div className="mx-auto mt-10 max-w-[760px] rounded border border-bolu-border bg-white p-6">
            <h2 className="font-display text-[20px] font-semibold">Pembayaran lokal</h2>
            <p className="mt-2 text-bolu-muted">
              Tagihan dan checkout belum aktif — harga di halaman ini masih placeholder.
              Nanti pembayaran dilakukan lewat transfer bank atau virtual account
              Indonesia, bukan Stripe.
            </p>
          </div>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
