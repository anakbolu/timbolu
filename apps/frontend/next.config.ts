import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /* `standalone` keeps the production image slim: server.js + traced deps only. */
  output: "standalone",
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
