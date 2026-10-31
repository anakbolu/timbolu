import path from "node:path";
import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /* `standalone` keeps the production image slim: server.js + traced deps only. */
  output: "standalone",
  /* The app imports `packages/contracts` (#343), which sits above `apps/frontend`,
   * so the build root is the repository root rather than the app directory.
   * Next assigns `turbopack.root` from this value (`next/dist/server/config.js`),
   * and Turbopack refuses to resolve anything above its root — without this the
   * `../../packages` import is unroutable and the root-context image fails to
   * build. It also nests the standalone output as `apps/frontend/...`, which is
   * what `Dockerfile.root` copies. */
  outputFileTracingRoot: path.join(__dirname, "../.."),
  cacheComponents: true,
  partialPrefetching: true,
  turbopack: {
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
  /* Apple and Google fetch these two files from the bare `/.well-known/...` URL
   * and do not follow a redirect. A `.well-known` folder under `app/` is a
   * private folder to Next, so the handlers live under `app/api/well-known` and
   * these rewrites put them at the URL the platforms actually ask for — the
   * response is the handler's own, so it is a 200 with `application/json`. */
  async rewrites() {
    return [
      {
        source: "/.well-known/apple-app-site-association",
        destination: "/api/well-known/apple-app-site-association",
      },
      {
        source: "/.well-known/assetlinks.json",
        destination: "/api/well-known/assetlinks.json",
      },
    ];
  },
};

export default nextConfig;
