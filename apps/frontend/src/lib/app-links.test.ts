/**
 * What the two well-known documents claim, and what they refuse to claim.
 *
 * The lists are the whole risk here: a path claimed for the app that has no app
 * screen opens the app on nothing, and an identity claim that names the wrong
 * app makes Apple and Google reject the domain silently. So the exclusions are
 * pinned as firmly as the inclusions, and both documents are checked with the
 * environment empty and filled.
 *
 * Run it with Bun, like the other frontend tests:
 *
 *   cd apps/frontend && bun test src/lib/app-links.test.ts
 */

import assert from "node:assert/strict";
import test from "node:test";

import {
  ANDROID_PATH_PREFIXES,
  APPLE_APP_LINK_COMPONENTS,
  appleAppSiteAssociation,
  assetLinks,
  UNIVERSAL_LINK_PATHS,
} from "./app-links";

const EXCLUDED = [
  "/login",
  "/signup",
  "/verify",
  "/forgot-password",
  "/onboarding",
  "/join",
  "/pricing",
  "/contact",
  "/privacy",
  "/terms",
  "/",
  "/admin",
];

function withEnvironment<T>(values: Record<string, string | undefined>, run: () => T): T {
  const previous = Object.fromEntries(Object.keys(values).map((key) => [key, process.env[key]]));
  for (const [key, value] of Object.entries(values)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  try {
    return run();
  } finally {
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
}

test("the claimed paths are the universal-link table, with the excluded pages absent", () => {
  for (const excluded of EXCLUDED) {
    assert.ok(
      !UNIVERSAL_LINK_PATHS.some((path) => path === excluded || path.startsWith(`${excluded}/`)),
      `${excluded} must not be claimed`,
    );
  }
  // `/tasks` alone is only a redirect; the detail screen is what the app opens.
  assert.ok(!UNIVERSAL_LINK_PATHS.includes("/tasks"));
  assert.ok(UNIVERSAL_LINK_PATHS.includes("/tasks/*"));
  // A draft is the object a notification points at, and it has its own screen.
  assert.ok(UNIVERSAL_LINK_PATHS.includes("/approval/*"));
});

test("Apple components restate the claimed paths one for one", () => {
  assert.deepEqual(
    APPLE_APP_LINK_COMPONENTS.map((component) => component["/"]),
    [...UNIVERSAL_LINK_PATHS],
  );
});

test("Android prefixes drop the id glob and an ancestor covers its children", () => {
  assert.deepEqual([...ANDROID_PATH_PREFIXES], [
    "/dashboard",
    "/chat",
    "/tasks",
    "/approval",
    "/routines",
    "/groups",
    "/bolu/new",
    "/settings",
  ]);
  for (const prefix of ANDROID_PATH_PREFIXES) assert.ok(!prefix.includes("*"));
});

test("without an app identity the documents claim nothing", () => {
  withEnvironment({ APPLE_APP_ID: undefined, ANDROID_PACKAGE_NAME: undefined, ANDROID_CERT_SHA256: undefined }, () => {
    assert.deepEqual(appleAppSiteAssociation(), { applinks: { details: [] } });
    assert.deepEqual(assetLinks(), []);
  });
  // Half an Android identity is not a smaller claim, it is an unverifiable one.
  withEnvironment({ ANDROID_PACKAGE_NAME: "com.timbolu.app", ANDROID_CERT_SHA256: undefined }, () => {
    assert.deepEqual(assetLinks(), []);
  });
  withEnvironment({ ANDROID_PACKAGE_NAME: undefined, ANDROID_CERT_SHA256: "AA:BB:CC" }, () => {
    assert.deepEqual(assetLinks(), []);
  });
});

test("a configured identity is used as given, fingerprints separated", () => {
  withEnvironment(
    {
      APPLE_APP_ID: "ABCDE12345.com.timbolu.app",
      ANDROID_PACKAGE_NAME: "com.timbolu.app",
      ANDROID_CERT_SHA256: "AA:BB:CC, DD:EE:FF ,",
    },
    () => {
      const aasa = appleAppSiteAssociation();
      assert.deepEqual(aasa.applinks.details[0]?.appIDs, ["ABCDE12345.com.timbolu.app"]);
      assert.deepEqual(aasa.applinks.details[0]?.components, APPLE_APP_LINK_COMPONENTS.map((component) => ({ ...component })));

      assert.deepEqual(assetLinks(), [
        {
          relation: ["delegate_permission/common.handle_all_urls"],
          target: {
            namespace: "android_app",
            package_name: "com.timbolu.app",
            sha256_cert_fingerprints: ["AA:BB:CC", "DD:EE:FF"],
          },
        },
      ]);
    },
  );
});