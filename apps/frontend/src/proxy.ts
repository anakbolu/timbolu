import { NextResponse, type NextRequest } from "next/server";

import { SESSION_HINT_COOKIE } from "@/lib/session-hint";

/**
 * The gate on the workspace routes, which is the one question worth answering
 * before any JavaScript runs: is there a session to load this screen against?
 *
 * Without a hint the answer is no, and a visitor with no session is sent to the
 * door instead of being handed the dashboard shell, its sidebar, and a round trip
 * of 401s that ends in an error card on a screen they were never allowed to read.
 *
 * The hint is not the session and this is not the authorisation. The access token
 * is the only thing the API accepts, and the workspace layout redirects the moment
 * a refresh fails — so a forged hint buys the shell and nothing else. That is what
 * makes this decision safe to take here at all.
 */
export default function proxy(request: NextRequest) {
  if (request.cookies.has(SESSION_HINT_COOKIE)) {
    return NextResponse.next();
  }

  const door = request.nextUrl.clone();
  door.pathname = "/login";
  door.search = "";
  return NextResponse.redirect(door);
}

/**
 * The routes that *are* a workspace rather than a page about one. The landing
 * page, pricing, and the auth screens are deliberately absent: they are what
 * somebody without a session is here to read.
 */
export const config = {
  matcher: [
    "/dashboard/:path*",
    "/chat/:path*",
    "/office/:path*",
    "/groups/:path*",
    "/settings/:path*",
    "/tasks/:path*",
  ],
};
