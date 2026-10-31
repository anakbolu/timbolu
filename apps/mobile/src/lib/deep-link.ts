/**
 * The one translation from an incoming URL to a screen.
 *
 * Two sources, both from `docs/deep-links.md`: the app's own `bolu://` scheme
 * (§ Skema `bolu://`) and the universal link `https://timbolu.com/<path>`
 * (§ Universal link). Kept pure and free of React so it can be read (and
 * checked) on its own: every branch below is a rule from that document, not a
 * guess made at the call site.
 */

export type LinkTarget =
  | { kind: "chat"; conversationID: string }
  | { kind: "task"; taskID: string }
  | { kind: "approval"; draftID: string }
  | { kind: "routine"; routineID: string }
  | { kind: "fallback"; reason: "unknown-path" | "invalid-id" | "not-a-link" };

/** `docs/deep-links.md` rule 1: the id is a UUID, in either letter case. */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

const RESOURCES = ["chat", "task", "approval", "routine"] as const;
type Resource = (typeof RESOURCES)[number];

function isResource(value: string): value is Resource {
  return (RESOURCES as readonly string[]).includes(value);
}

function fallback(reason: Extract<LinkTarget, { kind: "fallback" }>["reason"]): LinkTarget {
  return { kind: "fallback", reason };
}

/** The one host whose universal links this app claims (§ Universal link). */
const UNIVERSAL_HOST = "timbolu.com";

/**
 * The paths this app claims under that host — the same list the Android intent
 * filter in `app.json` declares.
 *
 * `/tasks/` and `/approval/` keep their slash because those screens only exist
 * with an id (`/tasks` is a web redirect to `/dashboard`, not a screen); the
 * rest claim their whole subtree. The paths the contract leaves to the browser
 * — `/login`, `/signup`, `/pricing`, `/admin/**`, … — are deliberately absent.
 */
const CLAIMED_PREFIXES = [
  "/dashboard",
  "/chat",
  "/tasks/",
  "/approval/",
  "/routines",
  "/groups",
  "/bolu/new",
  "/settings",
] as const;

/**
 * `bolu://<resource>/<id>`, unchanged since #500.
 *
 * Query and fragment are read but deliberately ignored: the contract forbids
 * tokens in links, and they must not change which screen opens. Extra path
 * segments are not ignored — `bolu://routine/<id>/extra` is not a link this app
 * defined, so it falls back instead of dropping the tail and guessing.
 */
function parseBoluLink(parsed: URL): LinkTarget {
  // Non-special schemes keep the host's case; the contract writes the resource
  // in lower case, and `bolu://Chat/...` is the same destination.
  const resource = parsed.hostname.toLowerCase();
  if (!isResource(resource)) return fallback("unknown-path");

  // Exactly `<resource>/<id>`: an empty path, a trailing slash only, or a
  // second segment all mean the link was not built by the contract.
  const segments = parsed.pathname.split("/").filter((segment) => segment !== "");
  if (segments.length !== 1) return fallback("unknown-path");

  const id = segments[0];
  if (!UUID.test(id)) return fallback("invalid-id");

  switch (resource) {
    case "chat":
      return { kind: "chat", conversationID: id };
    case "task":
      return { kind: "task", taskID: id };
    case "approval":
      return { kind: "approval", draftID: id };
    case "routine":
      return { kind: "routine", routineID: id };
  }
}

/**
 * `https://timbolu.com/<path>` on the paths the app claims (§ Universal link).
 *
 * Only the forms that map onto a `LinkTarget` are honoured here. `/tasks` and
 * `/routines` without an id are list pages in the web app, and this build has
 * no list target to open; they fall back to the Bolu list with its notice
 * instead of a blank screen.
 */
function parseUniversalLink(parsed: URL): LinkTarget {
  // Empty segments dropped so `/tasks/<id>/` is the same URL as
  // `/tasks/<id>`; a second real segment still fails the length checks below.
  const segments = parsed.pathname.split("/").filter((segment) => segment !== "");

  if (segments.length === 2) {
    const [resource, id] = segments;
    // Only these three have a detail screen. `/settings/team`,
    // `/admin/dashboard`, … are real web paths but not screens here, so they
    // are an unknown path rather than a bad id.
    if (resource === "tasks" || resource === "approval" || resource === "routines") {
      if (!UUID.test(id)) return fallback("invalid-id");
      if (resource === "tasks") return { kind: "task", taskID: id };
      if (resource === "approval") return { kind: "approval", draftID: id };
      return { kind: "routine", routineID: id };
    }
    return fallback("unknown-path");
  }

  // `/chat` carries its id in the query on the web (`/chat?conversation=<id>`);
  // the path form `/chat/<id>` does not exist there, so it is not invented here.
  if (segments.length === 1 && segments[0] === "chat") {
    const conversationID = parsed.searchParams.get("conversation");
    // `?agent=<id>` names a Bolu: `chat/[agentID]` opens a thread from that id,
    // but a link target must say which conversation to open, and the contract
    // has no target for "the thread with this Bolu". Landing on the Bolu list
    // with its notice is honest; guessing a conversation id would not be.
    if (!conversationID) return fallback("unknown-path");
    return UUID.test(conversationID)
      ? { kind: "chat", conversationID }
      : fallback("invalid-id");
  }

  return fallback("unknown-path");
}

/**
 * Never throws: a URL that does not parse is a link this app does not know, and
 * the person who tapped it still needs a screen with a way out (rule 4 §
 * Keadaan saat tautan tidak bisa dibuka).
 */
export function parseIncomingLink(url: string | null | undefined): LinkTarget {
  if (!url) return fallback("not-a-link");

  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return fallback("not-a-link");
  }

  if (parsed.protocol === "bolu:") return parseBoluLink(parsed);

  // A universal link must be `https` on the default port: `http://` and a
  // non-default port are not what the association files vouch for, so the
  // browser keeps those. Any other host is somebody else's link.
  if (parsed.protocol !== "https:") return fallback("not-a-link");
  if (parsed.hostname.toLowerCase() !== UNIVERSAL_HOST) return fallback("not-a-link");
  if (parsed.port !== "") return fallback("not-a-link");

  return parseUniversalLink(parsed);
}

/**
 * Whether a URL belongs to this app's own URL space: the `bolu://` scheme, or
 * `https://timbolu.com` on a path the universal-link table claims.
 *
 * Both call sites need the same answer. The router must never see these URLs
 * (`app/+native-intent.tsx` returns `null` for them) because it would match the
 * path against file names — `bolu://chat/<conversation id>` onto
 * `chat/[agentID]`, an id of the wrong kind. And the root listener must hand
 * exactly these URLs to `parseIncomingLink`.
 *
 * Everything else stays with the platform and the router: a foreign host, and
 * the timbolu.com paths the contract leaves to the browser (`/login`,
 * `/pricing`, `/admin/**`, …), which the app has no screen for.
 */
export function isAppLink(url: string | null | undefined): boolean {
  if (!url) return false;

  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return false;
  }

  // Mirrors the scheme check in `+native-intent.tsx`: the scheme is compared
  // without case because the platform may deliver `BOLU://`.
  if (parsed.protocol.toLowerCase() === "bolu:") return true;

  // Path matching is case-sensitive on purpose: the web serves these paths in
  // lower case, so `/Tasks/<id>` is a different URL that does not exist there.
  return (
    parsed.protocol === "https:" &&
    parsed.hostname.toLowerCase() === UNIVERSAL_HOST &&
    parsed.port === "" &&
    CLAIMED_PREFIXES.some((prefix) =>
      prefix.endsWith("/")
        ? parsed.pathname.startsWith(prefix) && parsed.pathname.length > prefix.length
        : parsed.pathname === prefix || parsed.pathname.startsWith(`${prefix}/`),
    )
  );
}