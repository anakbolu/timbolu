import Link from "next/link";
import { BotSvg } from "@/components/bolu/bot-svg";
import { StartLink } from "@/components/bolu/start-link";
import { SiteNavMenu } from "@/components/bolu/site-nav-menu";
import { ThemeToggle } from "@/components/ui/theme-toggle";
import { LOGO } from "@/lib/crew";

const LINKS = [
  { href: "/#tim", label: "Timnya" },
  { href: "/#cara-kerja", label: "Cara kerja" },
  { href: "/#intip", label: "Intip kerjanya" },
  // The prices are their own page rather than a section here, because a person deciding what to pay
  // arrives with that question on its own — and until this link existed the page was reachable only by
  // typing its URL, which is a page nobody reads.
  { href: "/pricing", label: "Harga" },
  { href: "/#tanya", label: "Tanya jawab" },
];

export function SiteNav() {
  return (
    <div className="mx-auto flex w-full max-w-[1248px] flex-wrap items-center justify-between gap-4 px-6 py-5">
      <Link href="/#atas" className="flex items-center gap-2.5 text-bolu-ink no-underline">
        <BotSvg bot={LOGO} className="size-10" />
        <span className="font-display text-[26px] font-bold">bolu</span>
      </Link>

      <nav
        aria-label="Navigasi utama"
        className="flex flex-wrap items-center gap-x-7 gap-y-2 font-medium max-[760px]:hidden"
      >
        {LINKS.map((link) => (
          <Link key={link.href} href={link.href} className="text-bolu-ink no-underline">
            {link.label}
          </Link>
        ))}
        <StartLink className="inline-flex min-h-11 items-center rounded-full bg-bolu-solid px-5 py-[11px] font-semibold text-bolu-solid-ink no-underline">
          Coba gratis
        </StartLink>
        <ThemeToggle />
      </nav>

      <SiteNavMenu links={LINKS} />
    </div>
  );
}
