import type { MetadataRoute } from "next";
import { SITE_URL } from "@/lib/metadata";

/**
 * The pages a crawler may index: the marketing site only.
 *
 * The sign-in, sign-up, verification, password-reset, invitation and onboarding
 * pages are deliberately absent. They exist for a person arriving from a link or
 * an email, they render nothing useful without a session, and listing them in a
 * sitemap only invites a crawler to index a redirect or an empty form.
 */

/**
 * One fixed date for every entry, bumped by hand when the public copy changes.
 *
 * `lastModified: new Date()` would stamp the moment of each build, so every deploy
 * would tell every crawler that all five pages changed — a signal that is either
 * false or worthless, and it also makes the generated XML differ between builds of
 * the same commit. A date that is honestly stale is better than one that is
 * confidently wrong.
 */
const LAST_UPDATED = new Date("2026-10-31");

const PUBLIC_PAGES: ReadonlyArray<{
  path: string;
  changeFrequency: MetadataRoute.Sitemap[number]["changeFrequency"];
  priority: number;
}> = [
  // The landing is what a search for the product should return.
  { path: "/", changeFrequency: "weekly", priority: 1 },
  // Pricing changes with the product's offer, and is the page a buyer lands on.
  { path: "/pricing", changeFrequency: "monthly", priority: 0.9 },
  // Legal pages change only when the policy does.
  { path: "/privacy", changeFrequency: "yearly", priority: 0.5 },
  { path: "/terms", changeFrequency: "yearly", priority: 0.5 },
  // A support address and a form; nothing here refreshes.
  { path: "/contact", changeFrequency: "yearly", priority: 0.6 },
];

export default function sitemap(): MetadataRoute.Sitemap {
  return PUBLIC_PAGES.map(({ path, changeFrequency, priority }) => ({
    url: path === "/" ? SITE_URL : `${SITE_URL}${path}`,
    lastModified: LAST_UPDATED,
    changeFrequency,
    priority,
  }));
}
