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
};

export default nextConfig;
