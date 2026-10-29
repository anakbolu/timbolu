import type { NextConfig } from "next";
import path from "node:path";

const nextConfig: NextConfig = {
  /* `standalone` keeps the production image slim: server.js + traced deps only. */
  output: "standalone",
  cacheComponents: true,
  partialPrefetching: true,
  turbopack: {
    // `packages/bolu-character` is imported as source from outside the app
    // directory, so Turbopack's root has to be the repository: a file outside
    // the root is not resolved at all. Anchored to this file rather than the
    // working directory, so it holds wherever `next` is started from.
    root: path.join(__dirname, "../.."),
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
};

export default nextConfig;
