import { connection, NextResponse } from "next/server";

import { assetLinks } from "@/lib/app-links";

/**
 * The file Google fetches to believe that this domain belongs to the Android app.
 *
 * Served at `/.well-known/assetlinks.json` by the rewrite in `next.config.ts`; the
 * rewrite exists because a `.well-known` folder under `app/` is treated as private
 * by Next and would build quietly and 404 in production.
 *
 * Like the Apple file, the identity is read per request — see `app-links.ts` for
 * why an unset identity answers with an empty list instead of a placeholder.
 */
export async function GET() {
  await connection();

  return NextResponse.json(assetLinks());
}