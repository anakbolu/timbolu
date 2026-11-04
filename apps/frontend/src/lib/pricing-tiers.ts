export type Tier = {
  name: string;
  price: string;
  period?: string;
  tagline: string;
  features: string[];
  cta: { label: string; href?: string };
  featured?: boolean;
};

export const TIERS: Tier[] = [
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
