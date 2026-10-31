/**
 * The Open Graph card for one public page.
 *
 * `layout.tsx` declares the landing's card once, and Next inherits it into every
 * route that does not declare its own, so a page exporting only `title` and
 * `description` still previews as the landing — `/pricing` arrived in chat as
 * "pekerjaan berulang, beres sampai tuntas" instead of "Harga". Each public page
 * therefore builds its card here from the title and description it already
 * renders.
 *
 * `og:url` has to be absolute: a relative value is useless to every crawler that
 * reads the card. The app sets no `metadataBase`, so the origin lives here and is
 * exported for `app/robots.ts` and `app/sitemap.ts`, which need the same absolute
 * URLs rather than a second copy of the origin that could drift.
 *
 * `openGraph.title` is written verbatim — Next's `title.template` only decorates
 * `<title>`, so the page title is passed through without the "· Timbolu" suffix.
 */

import type { Metadata } from "next";

export const SITE_URL = "https://timbolu.com";
const SITE_NAME = "Timbolu";

type CardInput = {
  /** The page's own title, the same value its `title` export carries. */
  title: string;
  /** The sentence the card shows; usually the page's `description` too. */
  description: string;
  /** Route path, e.g. `/pricing`. The root is `/` and gets no trailing slash. */
  path: string;
};

export function openGraphCard({
  title,
  description,
  path,
}: CardInput): Pick<Metadata, "openGraph" | "twitter"> {
  return {
    openGraph: {
      title,
      description,
      url: path === "/" ? SITE_URL : `${SITE_URL}${path}`,
      siteName: SITE_NAME,
      locale: "id_ID",
      type: "website",
    },
    twitter: {
      // There is no `og:image` yet, so `summary_large_image` would render an
      // empty card; `summary` falls back to the title and description.
      card: "summary",
      title,
      description,
    },
  };
}

export function pageMetadata({ title, description, path }: CardInput): Metadata {
  return { title, description, ...openGraphCard({ title, description, path }) };
}