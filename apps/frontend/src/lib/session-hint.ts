/**
 * The cookie the workspace routes are gated on.
 *
 * It is named in a file of its own because two runtimes have to agree about it:
 * the edge gate below the app, and the API client that keeps it in step. The
 * gate cannot import the API client, which is why this module has no imports at
 * all.
 *
 * It is a *hint*, not the session. The session is the httpOnly refresh cookie,
 * which the backend deliberately scopes to the auth endpoints so it does not
 * travel with every request — and a browser only sends a cookie whose path is a
 * prefix of the request, so the edge never sees it. This cookie carries no secret
 * and authorises nothing: the access token is still the only thing the API
 * accepts. It decides one thing, and it is a thing worth deciding before any
 * JavaScript runs — whether an anonymous visitor is handed the dashboard shell
 * and a round trip of 401s, or the door.
 */
export const SESSION_HINT_COOKIE = "bolu_session";

/** Matches the refresh cookie's own thirty days. */
export const SESSION_HINT_MAX_AGE = 30 * 24 * 60 * 60;
