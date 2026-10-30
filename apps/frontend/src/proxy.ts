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
  if (!request.cookies.has(SESSION_HINT_COOKIE)) {
    const door = request.nextUrl.clone();
    door.pathname = "/login";
    door.search = "";
    return NextResponse.redirect(door);
  }

  // This answer depends on a cookie, so a shared cache must not store it. Next
  // marks these screens as static and sends `s-maxage=31536000`, and that header
  // knows nothing about the gate: if an edge cache ever stored the redirect above,
  // a signed-in visitor would be bounced to a sign-in page that then bounced them
  // straight back — a loop no amount of signing in can leave. Declaring it here is
  // what keeps the gate the only thing deciding who gets in.
  const response = NextResponse.next();
  response.headers.set("Cache-Control", "private, no-store");
  return response;
}

/**
 * The routes that *are* a workspace rather than a page about one. The landing
 * page, pricing, and the auth screens are deliberately absent: they are what
 * somebody without a session is here to read.
 *
 * `/admin` is here for the same reason the workspace routes are — it needs a
 * session before it can answer anything — and not for the platform role: a role
 * is a fact only the API knows, so the admin layout asks for it and redirects a
 * plain account. This gate only decides whether the shell is worth loading.
 *
 * `/approval` is the screen a notification lands on, and it reads a draft with
 * the session's token — so it is a workspace route like `/tasks`, not a page
 * about one.
 */
export const config = {
  matcher: [
    "/bolu/:path*",
    "/dashboard/:path*",
    "/routines/:path*",
    "/chat/:path*",
    "/groups/:path*",
    "/settings/:path*",
    "/tasks/:path*",
    "/approval/:path*",
    "/admin/:path*",
  ],
};
