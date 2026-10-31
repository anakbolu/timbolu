/**
 * The one translation from an incoming `bolu://` URL to a screen.
 *
 * Kept pure and free of React so it can be read (and checked) on its own: every
 * branch below is a rule from `docs/deep-links.md` § Skema `bolu://`, not a
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

/**
 * Never throws: a URL that does not parse is a link this app does not know, and
 * the person who tapped it still needs a screen with a way out (rule 4 §
 * Keadaan saat tautan tidak bisa dibuka).
 *
 * Query and fragment are read but deliberately ignored: the contract forbids
 * tokens in links, and they must not change which screen opens. Extra path
 * segments are not ignored — `bolu://routine/<id>/extra` is not a link this app
 * defined, so it falls back instead of dropping the tail and guessing.
 */
export function parseIncomingLink(url: string | null | undefined): LinkTarget {
  if (!url) return fallback("not-a-link");

  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return fallback("not-a-link");
  }

  // Universal links are not wired yet (issue #499 scope), so anything that is
  // not the app's own scheme is not a link this build can open.
  if (parsed.protocol !== "bolu:") return fallback("not-a-link");

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
