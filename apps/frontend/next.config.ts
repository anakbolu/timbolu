import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /* `standalone` keeps the production image slim: server.js + traced deps only. */
  output: "standalone",
  cacheComponents: true,
  partialPrefetching: true,
  turbopack: {
    // The contracts package lives at `packages/contracts` and is imported as
    // source, so Turbopack's root has to be the repository, not `apps/frontend`.
    // Without this it refuses an import from outside the app directory.
    root: "../..",
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
};

export default nextConfig;
