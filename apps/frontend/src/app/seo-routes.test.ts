/**
 * What `/robots.txt` and `/sitemap.xml` say, and what they must never say.
 *
 * Both files are the only crawl contract the site has, so the risky half is the
 * omissions: a sitemap that lists a session page hands a crawler an empty form,
 * and a robots file that forgets a workspace route lets one be indexed. These
 * cases pin the public five, the closed routes, and the absolute origin.
 *
 * Run it with Bun, like the other frontend tests, because the routes resolve
 * `@/lib/metadata` through the app's alias:
 *
 *   cd apps/frontend && bun test src/app/seo-routes.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import robots from "./robots";
import sitemap from "./sitemap";

const SITE = "https://timbolu.com";

const PUBLIC_PATHS = ["/", "/pricing", "/privacy", "/terms", "/contact"];

const PRIVATE_PATHS = [
  "/dashboard",
  "/approval",
  "/chat",
  "/groups",
  "/routines",
  "/tasks",
  "/settings",
  "/onboarding",
  "/bolu",
  "/admin",
  "/api",
  "/login",
  "/signup",
  "/verify",
  "/forgot-password",
  "/join",
];

test("the sitemap lists exactly the public pages, as absolute URLs", () => {
  const entries = sitemap();
  const urls = entries.map((entry) => entry.url);

  assert.deepEqual(urls, PUBLIC_PATHS.map((path) => (path === "/" ? SITE : `${SITE}${path}`)));
  for (const entry of entries) {
    assert.ok(entry.url.startsWith(`${SITE}/`) || entry.url === SITE);
    assert.ok(entry.lastModified instanceof Date);
    assert.ok(entry.changeFrequency);
    assert.ok(entry.priority !== undefined);
  }
});

test("the sitemap never lists a page behind a session", () => {
  const urls = sitemap().map((entry) => entry.url);

  for (const path of PRIVATE_PATHS) {
    assert.ok(
      !urls.includes(`${SITE}${path}`) && !urls.some((url) => url.startsWith(`${SITE}${path}/`)),
      `${path} must not be in the sitemap`,
    );
  }
});

test("the sitemap's lastModified does not move between builds", () => {
  const first = sitemap();
  const second = sitemap();

  assert.deepEqual(
    first.map((entry) => entry.lastModified),
    second.map((entry) => entry.lastModified),
  );
});

test("robots.txt allows the site, closes the workspace and names the sitemap", () => {
  const { rules, sitemap: sitemapUrl } = robots();
  const rule = Array.isArray(rules) ? rules[0] : rules;

  assert.equal(rule.userAgent, "*");
  assert.equal(rule.allow, "/");
  assert.equal(sitemapUrl, `${SITE}/sitemap.xml`);

  for (const path of ["/dashboard", "/admin", "/api", "/settings", "/onboarding"]) {
    assert.ok(
      ([] as string[]).concat(rule.disallow ?? []).includes(path),
      `${path} must be disallowed`,
    );
  }
});
