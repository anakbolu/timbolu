// @ts-ignore
import { describe, it, expect } from "bun:test";
import { CLAIMED_PREFIXES, parseIncomingLink } from "./deep-link";

type AppJson = {
  expo: {
    android: {
      intentFilters: {
        data: { pathPrefix?: string }[];
      }[];
    };
  };
};
// @ts-ignore
const appJson = (await Bun.file(new URL("../../app.json", import.meta.url)).json()) as AppJson;

describe("Universal Links Configuration", () => {
  it("should resolve claimed prefixes to non-fallback kinds", () => {
    const testPaths = [
      `https://timbolu.com/chat?conversation=${crypto.randomUUID()}`,
      `https://timbolu.com/tasks/${crypto.randomUUID()}`,
      `https://timbolu.com/approval/${crypto.randomUUID()}`,
      `https://timbolu.com/routines/${crypto.randomUUID()}`,
    ];

    testPaths.forEach((path) => {
      const result = parseIncomingLink(path);
      expect(result.kind).not.toBe("fallback");
    });
  });

  it("should have app.json pathPrefix matching CLAIMED_PREFIXES", () => {
    const appJsonPrefixes = appJson.expo.android.intentFilters.flatMap((f) => 
      f.data.filter((d) => d.pathPrefix).map((d) => d.pathPrefix!)
    );
    
    expect([...CLAIMED_PREFIXES].sort()).toEqual(appJsonPrefixes.sort());
  });

  it("should not claim unlisted paths", () => {
    const unlistedPaths = [
      "https://timbolu.com/dashboard",
      "https://timbolu.com/dashboard/integrations",
      "https://timbolu.com/routines",
      "https://timbolu.com/groups",
      "https://timbolu.com/bolu/new",
      "https://timbolu.com/settings",
      "https://timbolu.com/settings/usage",
    ];

    unlistedPaths.forEach((path) => {
      const isClaimed = CLAIMED_PREFIXES.some((prefix: string) => new URL(path).pathname.startsWith(prefix));
      expect(isClaimed).toBe(false);
    });
  });
});