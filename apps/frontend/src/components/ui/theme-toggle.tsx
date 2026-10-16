"use client";

/**
 * ThemeToggle: the control that switches the theme.
 *
 * Three states rather than two, because "follow the system" is a real answer and
 * a two-state switch cannot express it — a person who never chose anything is
 * not a person who chose light. The control is a segmented group so all three
 * are visible at once: a cycling button would hide two of the answers behind
 * repeated clicks, and the current one would be the only one legible.
 *
 * The state is read through `useTheme` rather than mirrored into `useState`, so
 * two toggles on one page — the sidebar's and the settings card's — cannot
 * disagree.
 */

import { Monitor, Moon, Sun } from "lucide-react";

import { useTheme } from "@/hooks/use-theme";
import { setTheme, type Theme } from "@/lib/theme";

const OPTIONS: { value: Theme; label: string; icon: typeof Sun }[] = [
  { value: "light", label: "Terang", icon: Sun },
  { value: "dark", label: "Gelap", icon: Moon },
  { value: "system", label: "Ikut sistem", icon: Monitor },
];

/** ThemeToggle renders the three answers as one row of icon buttons. */
export function ThemeToggle({ className = "" }: { className?: string }) {
  const theme = useTheme();

  return (
    <div
      role="radiogroup"
      aria-label="Tema tampilan"
      className={`inline-flex items-center gap-0.5 rounded-full border border-bolu-border bg-bolu-surface p-0.5 ${className}`}
    >
      {OPTIONS.map((option) => {
        const on = option.value === theme;
        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={on}
            aria-label={option.label}
            title={option.label}
            onClick={() => setTheme(option.value)}
            className={`flex size-8 cursor-pointer items-center justify-center rounded-full border-0 transition-colors ${
              on ? "bg-bolu-solid text-bolu-solid-ink" : "bg-transparent text-bolu-muted hover:text-bolu-ink"
            }`}
          >
            <option.icon size={16} strokeWidth={2.25} aria-hidden="true" />
          </button>
        );
      })}
    </div>
  );
}
