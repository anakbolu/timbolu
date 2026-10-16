/**
 * The theme: which of the three answers the product is drawn in.
 *
 * The choice lives in `localStorage` rather than in a cookie. A cookie is the
 * other option, and it would let the server render the right theme into the HTML
 * — but the root layout has to stay statically prerendered, and reading a cookie
 * in a layout is exactly what stops that. The price of keeping the layout static
 * is a frame of the wrong theme, and the price is paid off by `THEME_INIT_SCRIPT`
 * below: it runs while the browser parses the document, before the first paint,
 * and reaches the same answer this module does.
 *
 * The class is the switch, not the media query. Tailwind's `dark:` variant is
 * bound to `.dark` in globals.css, so a person who wants the dark theme on a
 * machine that reports light can have it — and a person who follows the system
 * still follows it when the system changes, which is what `watchSystem` is for:
 * under `system` no CSS rule would notice, because nothing about the document
 * changed.
 *
 * "system" is the absence of a choice, not a third colour, which is why it is
 * stored as itself and resolved at every read rather than being written down as
 * the theme it happened to mean on the day it was picked.
 */

export const THEME_KEY = "bolu_theme";

/** What the person picked. */
export type Theme = "light" | "dark" | "system";

/** What that means right now. */
export type ResolvedTheme = "light" | "dark";

const DARK_QUERY = "(prefers-color-scheme: dark)";

/**
 * The colour the browser paints its own chrome — the address bar, the status bar
 * on a phone. It mirrors `--color-bolu-bg` in globals.css; the two are written in
 * two places because the browser reads the meta tag before any stylesheet is
 * applied, so it cannot be a `var()`.
 */
const CHROME: Record<ResolvedTheme, string> = {
  light: "#f5f6fb",
  dark: "#100e19",
};

/** isTheme narrows a stored string, which may be anything a user typed into devtools. */
function isTheme(value: string | null): value is Theme {
  return value === "light" || value === "dark" || value === "system";
}

/**
 * readTheme is the store's current value.
 *
 * `localStorage` throws in a browser that has storage switched off, and the
 * default has to be something: following the system is the answer that needs no
 * choice, so it is what a person who never chose gets.
 */
export function readTheme(): Theme {
  try {
    const stored = window.localStorage.getItem(THEME_KEY);
    return isTheme(stored) ? stored : "system";
  } catch {
    return "system";
  }
}

/** resolveTheme turns a choice into the theme it means. */
export function resolveTheme(theme: Theme): ResolvedTheme {
  if (theme !== "system") return theme;
  return window.matchMedia(DARK_QUERY).matches ? "dark" : "light";
}

/**
 * readResolvedTheme is what the page is actually drawn in right now.
 *
 * Only the two renderers that paint outside the cascade need it — ECharts and
 * Mermaid write into a canvas or an SVG of their own and cannot resolve a CSS
 * variable — so this is the exception, and every component that can use a token
 * does.
 */
export function readResolvedTheme(): ResolvedTheme {
  return resolveTheme(readTheme());
}

/**
 * paintChrome tells the browser what colour to paint its own chrome — the address
 * bar, the status bar on a phone.
 *
 * The tag is created and owned here rather than declared in the viewport export.
 * A declared one is rendered by React, which means React re-inserts it on every
 * navigation — after this module has already set the colour — and a
 * `media="(prefers-color-scheme: light)"` tag that survives into a dark page is
 * worse than no tag at all. With nothing declared, nothing re-inserts, and the
 * one tag that exists says what the page actually is.
 */
function paintChrome(resolved: ResolvedTheme): void {
  // A snapshot rather than the live list: the append below would otherwise land
  // inside the same collection the loop at the end clears.
  const existing = [...document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')];
  const meta = existing[0] ?? document.head.appendChild(document.createElement("meta"));
  meta.name = "theme-color";
  meta.content = CHROME[resolved];

  // A duplicate is a leftover, and the browser reads the first one.
  for (const extra of existing.slice(1)) extra.remove();
}

/**
 * apply paints a theme: the class Tailwind's `dark:` variant is bound to, the
 * attribute the toggle reads its own state from, and the colour the browser
 * paints its chrome with.
 */
export function apply(theme: Theme): void {
  const resolved = resolveTheme(theme);
  const root = document.documentElement;

  root.classList.toggle("dark", resolved === "dark");
  root.dataset.theme = theme;
  paintChrome(resolved);
}

/** setTheme records a choice and paints it. */
export function setTheme(theme: Theme): void {
  try {
    window.localStorage.setItem(THEME_KEY, theme);
  } catch {
    // A browser that refuses storage still gets the theme for this session; only
    // the memory of it is lost, which is the lesser of the two failures.
  }
  apply(theme);
  for (const listener of listeners) listener();
}

/** The set of mounted readers. Dynamic membership, so a `Set` rather than a record. */
const listeners = new Set<() => void>();
let watching = false;

/**
 * watchSystem keeps "system" honest.
 *
 * The class on `<html>` is a fixed decision, so a machine that goes dark at
 * sunset would leave the page light until something else re-rendered it. Nothing
 * else will: the media query is the only thing that changed.
 *
 * It is installed once, on the first subscriber, and never removed — the answer
 * it maintains is on the document, not on the component that happened to be
 * mounted first.
 */
function watchSystem(): void {
  if (watching) return;
  watching = true;

  window.matchMedia(DARK_QUERY).addEventListener("change", () => {
    if (readTheme() !== "system") return;
    apply("system");
    for (const listener of listeners) listener();
  });

  // Two tabs, one choice: the other tab's `storage` event is the only notice
  // this one gets, and without it the two disagree until one is reloaded.
  window.addEventListener("storage", (event) => {
    if (event.key !== null && event.key !== THEME_KEY) return;
    apply(readTheme());
    for (const listener of listeners) listener();
  });
}

/** subscribeTheme is the external store `useTheme` reads through. */
export function subscribeTheme(listener: () => void): () => void {
  watchSystem();
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * THEME_INIT_SCRIPT is `readTheme` + `apply` written for the parser.
 *
 * It is the one piece of this module that cannot be a function call: it has to
 * run while the document is being parsed, before the bundle exists, so it is
 * shipped as text and evaluated inline. It is deliberately the same steps in the
 * same order as `apply`, and it fails to the default rather than throwing — a
 * theme is not worth a blank page.
 *
 * The colours are written out rather than imported, for the same reason `CHROME`
 * is written out: at this point there is no stylesheet to read them from, and the
 * bundle that could import the module has not been fetched yet.
 */
export const THEME_INIT_SCRIPT = `(function(){try{
var t=localStorage.getItem(${JSON.stringify(THEME_KEY)});
if(t!=="light"&&t!=="dark")t="system";
var d=t==="dark"||(t==="system"&&window.matchMedia(${JSON.stringify(DARK_QUERY)}).matches);
var r=document.documentElement;
r.classList.toggle("dark",d);
r.dataset.theme=t;
var old=document.querySelectorAll('meta[name="theme-color"]'),m=old[0];
if(!m){m=document.createElement("meta");m.name="theme-color";document.head.appendChild(m)}
m.content=d?"${CHROME.dark}":"${CHROME.light}";
for(var i=1;i<old.length;i++)old[i].remove();
}catch(e){}})();`;
