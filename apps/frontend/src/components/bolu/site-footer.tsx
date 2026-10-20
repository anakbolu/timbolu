import Link from "next/link";

const LINKS = [
  { href: "/privacy", label: "Privasi" },
  { href: "/terms", label: "Syarat" },
  { href: "/contact", label: "Kontak" },
];

export function SiteFooter() {
  return (
    <footer className="mx-auto flex w-full max-w-[1248px] flex-wrap justify-between gap-4 px-6 pb-14 pt-12 text-[15px] text-bolu-muted">
      <span>© 2026 Bolu</span>
      <nav aria-label="Tautan footer" className="flex flex-wrap gap-5">
        {LINKS.map((link) => (
          <Link key={link.label} href={link.href} className="text-bolu-muted">
            {link.label}
          </Link>
        ))}
      </nav>
    </footer>
  );
}
