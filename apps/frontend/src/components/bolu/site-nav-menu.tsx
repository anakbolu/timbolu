"use client";

/**
 * The landing navigation for phones: the desktop link row is hidden below 761px
 * and this hamburger takes its place, so the five sections and the call to action
 * are one tap instead of a wrapped wall of links.
 */

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";

import { Menu, X } from "lucide-react";

import { StartLink } from "@/components/bolu/start-link";
import { ThemeToggle } from "@/components/ui/theme-toggle";

export function SiteNavMenu({
  links,
}: {
  links: readonly { href: string; label: string }[];
}) {
  const [open, setOpen] = useState(false);
  const pathname = usePathname();

  return (
    <>
      <button
        type="button"
        aria-label="Buka menu"
        aria-expanded={open}
        onClick={() => setOpen(true)}
        className="flex size-11 cursor-pointer items-center justify-center rounded-xl border border-bolu-border bg-bolu-surface text-bolu-ink min-[761px]:hidden"
      >
        <Menu size={22} strokeWidth={2} aria-hidden="true" />
      </button>

      {open ? (
        <div
          onClick={() => setOpen(false)}
          className="fixed inset-0 z-[35] bg-bolu-scrim min-[761px]:hidden"
        />
      ) : null}

      <nav
        aria-label="Navigasi utama"
        className={`fixed inset-y-0 right-0 z-40 flex w-[min(320px,86vw)] flex-col gap-1.5 bg-bolu-surface p-5 transition-transform duration-200 min-[761px]:hidden ${
          open ? "translate-x-0 shadow-[0_10px_40px_rgba(30,27,46,0.25)]" : "translate-x-full"
        }`}
      >
        <div className="flex items-center justify-between pb-2.5">
          <span className="font-display text-xl font-bold">Menu</span>
          <button
            type="button"
            aria-label="Tutup menu"
            onClick={() => setOpen(false)}
            className="flex size-11 cursor-pointer items-center justify-center rounded-full border-0 bg-bolu-bg"
          >
            <X size={20} strokeWidth={2.5} aria-hidden="true" />
          </button>
        </div>

        <div className="flex flex-col gap-1">
          {/* Same rule as the desktop row: the front page does not link to itself. */}
          {pathname === "/" ? null : (
            <Link
              href="/"
              onClick={() => setOpen(false)}
              className="min-h-[52px] rounded-xl px-3.5 text-[17px] font-bold text-bolu-ink no-underline hover:bg-bolu-hover/60"
            >
              Home
            </Link>
          )}
          {links.map((link) => (
            <Link
              key={link.href}
              href={link.href}
              onClick={() => setOpen(false)}
              aria-current={pathname === link.href ? "page" : undefined}
              className="min-h-[52px] rounded-xl px-3.5 text-[17px] font-bold text-bolu-ink no-underline hover:bg-bolu-hover/60"
            >
              {link.label}
            </Link>
          ))}
        </div>

        <StartLink
          onClick={() => setOpen(false)}
          className="mt-3 inline-flex min-h-11 items-center justify-center rounded-full bg-bolu-solid px-5 py-[11px] font-semibold text-bolu-solid-ink no-underline"
        >
          Coba gratis
        </StartLink>

        {/* The phone's nav row is hidden, so the theme control comes with it:
            the drawer is the only place the landing page offers on a phone. */}
        <div className="mt-4 flex items-center justify-between gap-2 border-t border-bolu-line pt-4">
          <span className="text-[15px] font-bold text-bolu-muted">Tampilan</span>
          <ThemeToggle />
        </div>
      </nav>
    </>
  );
}