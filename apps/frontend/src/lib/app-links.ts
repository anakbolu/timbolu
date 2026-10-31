/**
 * The `timbolu.com` paths the app owns, and the two files that tell the
 * platforms so.
 *
 * There is one list here on purpose. `/.well-known/apple-app-site-association`
 * and `/.well-known/assetlinks.json` are two spellings of the same decision, and
 * when that decision lives in two places one of them goes stale — the platform
 * then keeps a subset of the claims, and the links that fall out of it open a
 * browser without anyone noticing. `docs/deep-links.md` § Universal link is the
 * source: every entry below is a row of that table, and a path only appears if
 * the table names it as a universal link target.
 *
 * A claimed path the app cannot render opens the app on nothing, which is worse
 * than staying in the browser. So the paths the table excludes stay excluded:
 * `/login`, `/signup`, `/verify`, `/forgot-password`, `/onboarding`, `/join`,
 * `/pricing`, `/contact`, `/privacy`, `/terms`, `/` and all of `/admin/**` are
 * browser pages with no app screen. `/tasks` is absent on its own for a
 * different reason: it is only `redirect("/dashboard")`, so it has no screen of
 * its own — `/tasks/*` is claimed because the task detail does.
 */
const UNIVERSAL_LINK_PATHS = [
  "/dashboard",
  "/chat", // `/chat?agent=<id>` and `/chat?conversation=<id>`; a component matches the path, so both arrive
  "/tasks/*", // `/tasks/<task_id>`; the bare `/tasks` above is the redirect, not a screen
  "/approval/*", // `/approval/<draft_id>`
  "/routines", // the list, with history
  "/routines/*", // `/routines/<routine_id>`, the focused routine
  "/groups",
  "/bolu/new",
  "/dashboard/integrations",
  "/settings",
  "/settings/*", // `/settings/team`, `/model`, `/memory`, `/data`, `/usage`, `/subscription`
] as const;

/** A path from `docs/deep-links.md` § Universal link, with `*` standing for an id segment. */
export type UniversalLinkPath = (typeof UNIVERSAL_LINK_PATHS)[number];

export { UNIVERSAL_LINK_PATHS };

/**
 * The same list as Apple's `components`: one entry per claimed path, `*` and all.
 * Apple matches a component exactly unless it ends in `*`, so `/settings` and
 * `/settings/*` are both needed even though `/settings/*` covers the children.
 */
export const APPLE_APP_LINK_COMPONENTS: readonly { "/": UniversalLinkPath }[] = UNIVERSAL_LINK_PATHS.map(
  (path) => ({ "/": path }),
);

/**
 * The same list as Android `pathPrefix` values, for the intent filter in
 * `apps/mobile/app.json` to mirror — `assetlinks.json` carries no paths, only the
 * signing identity, so this is the only place the Android side of the same
 * decision is written down.
 *
 * The id glob is dropped (`/tasks/*` becomes `/tasks`), and an ancestor already
 * claims its descendants: Android compares a prefix one segment at a time, so
 * `/settings` covers `/settings/team` and repeating it would only restate the
 * claim.
 */
export const ANDROID_PATH_PREFIXES: readonly string[] = pathPrefixes(UNIVERSAL_LINK_PATHS);

export type AppleAppSiteAssociation = {
  applinks: {
    details: {
      appIDs: string[];
      components: { "/": string }[];
    }[];
  };
};

export type AssetLinkStatement = {
  relation: string[];
  target: {
    namespace: "android_app";
    package_name: string;
    sha256_cert_fingerprints: string[];
  };
};

/**
 * The AASA document, built from the identity the deployment knows.
 *
 * With `APPLE_APP_ID` unset this claims nothing — `{"applinks":{"details":[]}}`
 * — instead of shipping a placeholder app ID. A file that names the wrong app is
 * not a harmless placeholder: Apple reads it as the answer, finds no app with
 * that identity, and silently refuses every link for months (its CDN caches the
 * result), with nothing in the response saying why. An empty claim is a
 * verified-wrong answer that fixes itself the moment the identity exists; the
 * wrong app ID is a wrong answer nobody looks at again.
 */
export function appleAppSiteAssociation(): AppleAppSiteAssociation {
  const appID = process.env.APPLE_APP_ID?.trim();
  if (!appID) return { applinks: { details: [] } };

  return {
    applinks: {
      details: [{ appIDs: [appID], components: APPLE_APP_LINK_COMPONENTS.map((component) => ({ ...component })) }],
    },
  };
}

/**
 * The asset links document, built from the Android identity the deployment knows.
 *
 * An empty array when the package name or a fingerprint is missing, for the same
 * reason the AASA document claims nothing: an entry with a placeholder package
 * or a debug fingerprint is a statement Google checks and rejects, so the app
 * never becomes a verified handler of the domain — while an empty document makes
 * the missing identity visible in one `curl`.
 *
 * `ANDROID_CERT_SHA256` is comma separated because Play App Signing can rotate
 * the certificate while the old one still signs installed builds; both must be
 * listed until the rotation completes.
 */
export function assetLinks(): AssetLinkStatement[] {
  const packageName = process.env.ANDROID_PACKAGE_NAME?.trim();
  const fingerprints = splitList(process.env.ANDROID_CERT_SHA256);
  if (!packageName || fingerprints.length === 0) return [];

  return [
    {
      relation: ["delegate_permission/common.handle_all_urls"],
      target: {
        namespace: "android_app",
        package_name: packageName,
        sha256_cert_fingerprints: fingerprints,
      },
    },
  ];
}

function pathPrefixes(paths: readonly string[]): string[] {
  const prefixes: string[] = [];
  for (const path of paths) {
    const prefix = path.replace(/\/\*$/, "");
    if (prefixes.includes(prefix)) continue; // `/routines` and `/routines/*` are one prefix
    prefixes.push(prefix);
  }
  return prefixes.filter((prefix) => !prefixes.some((other) => prefix.startsWith(`${other}/`)));
}

function splitList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((item) => item.trim())
    .filter((item) => item.length > 0);
}