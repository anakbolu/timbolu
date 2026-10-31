import type { MetadataRoute } from "next";
import { SITE_URL } from "@/lib/metadata";

/**
 * `/robots.txt` for the web app.
 *
 * Everything behind a session is closed to crawlers: the workspace screens, the
 * operator console, onboarding, and the API. Only the marketing pages are public,
 * and `sitemap.xml` is where their absolute URLs come from, so the crawler does
 * not have to discover them by following links.
 *
 * `/approval` already covers every path beneath it, so `/approval/` adds nothing;
 * it is listed because the two spellings are what people search for when a page
 * leaks into an index, and an explicit line is easier to audit than an inference
 * about prefix matching.
 */
export default function robots(): MetadataRoute.Robots {
  return {
    rules: {
      userAgent: "*",
      allow: "/",
      disallow: [
        "/dashboard",
        "/approval",
        "/approval/",
        "/chat",
        "/groups",
        "/routines",
        "/tasks",
        "/settings",
        "/onboarding",
        "/bolu",
        "/admin",
        "/api",
      ],
    },
    sitemap: `${SITE_URL}/sitemap.xml`,
  };
}
