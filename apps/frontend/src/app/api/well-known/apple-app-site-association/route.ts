import { connection, NextResponse } from "next/server";

import { appleAppSiteAssociation } from "@/lib/app-links";

/**
 * The file Apple fetches to believe that this domain belongs to the app.
 *
 * It is served at `/.well-known/apple-app-site-association` by the rewrite in
 * `next.config.ts`, not from a `.well-known` folder in `app/`: Next reserves
 * dot-prefixed names for private folders, so a file placed there would build
 * without complaint and then never be served — a 404 nothing in the build log
 * points at.
 *
 * `connection()` keeps this out of the build-time render. The answer is read
 * from the environment at request time, so the same image serves the identity the
 * deployment actually has instead of one frozen from the environment of the
 * machine that built it.
 */
export async function GET() {
  await connection();

  // `NextResponse.json` is the point of this route: Apple rejects the file when
  // the content type is anything but `application/json`, and `public/` serves a
  // file without an extension as `application/octet-stream`.
  return NextResponse.json(appleAppSiteAssociation());
}