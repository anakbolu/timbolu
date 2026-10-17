/**
 * The operator surface's contract: the accounts a deployment has, and the one
 * decision an admin makes about them.
 *
 * Ported from internal/modules/admin/domain/admin.go.
 *
 * The Go file imports `modules/auth/domain` for the platform role constants.
 * That module has not been ported yet, so the two constants and the check are
 * defined here — the same values, and the same fail-closed check. When the auth
 * domain lands, this file should import it and drop the local copies.
 */

/**
 * The platform role of an account, matching the check constraint on
 * `users.role`. Not the workspace role stored on `members`.
 */
export const RoleUser = "user";
export const RoleAdmin = "admin";

/** An unrecognised or empty role is not an admin, so the check fails closed. */
export function isAdminRole(role: string): boolean {
  return role === RoleAdmin;
}

/** The page bounds a list read is clamped to, so one request cannot ask for the whole table. */
export const DefaultPageSize = 25;
export const MaxPageSize = 100;

/**
 * Errors this module returns. Handlers map them onto status codes, so the
 * mapping lives in one place instead of being guessed per endpoint.
 */
export type AdminErrorKind =
  | "not_admin"
  | "account_not_found"
  | "last_admin"
  | "self_demotion"
  | "invalid_input"
  | "not_configured";

export class AdminError extends Error {
  readonly kind: AdminErrorKind;

  constructor(kind: AdminErrorKind, message: string) {
    super(message);
    this.name = "AdminError";
    this.kind = kind;
  }
}

/** The caller may not reach the operator surface at all. */
export const ErrNotAdmin = new AdminError("not_admin", "admin: the caller is not a platform admin");
/** No account has that id. */
export const ErrAccountNotFound = new AdminError("account_not_found", "admin: account not found");
/** The change would leave the deployment with no admin. */
export const ErrLastAdmin = new AdminError("last_admin", "admin: the last platform admin cannot be demoted");
/** The caller tried to demote their own account. */
export const ErrSelfDemotion = new AdminError("self_demotion", "admin: an admin cannot demote their own account");
/** The request itself is wrong: a role that is not one of the two, or a page outside its bounds. */
export const ErrInvalidInput = new AdminError("invalid_input", "admin: invalid input");
/** The deployment has no database, so the surface answers 503 rather than failing the query. */
export const ErrNotConfigured = new AdminError("not_configured", "admin: database is not configured");

/** True for an AdminError of the given kind, so a wrapped error still maps. */
export function isAdminError(err: unknown, kind: AdminErrorKind): boolean {
  return err instanceof AdminError && err.kind === kind;
}

/** One row of the account list: what an operator reads about an account. */
export interface Account {
  id: string;
  email: string;
  role: string;
  emailVerified: boolean;
  onboarded: boolean;
  hasGoogle: boolean;
  hasPassword: boolean;
  createdAt: Date;
  /**
   * How many workspaces the account is a member of. A count rather than a list:
   * which workspaces it is in is the customer's business.
   */
  workspaces: number;
}

/** Reports whether this account may reach the operator surface. */
export function accountIsAdmin(account: Account): boolean {
  return isAdminRole(account.role);
}

/** The zero uuid, what an anonymous request carries as its caller id. */
export const ZeroUUID = "00000000-0000-0000-0000-000000000000";

/** What the accounts add up to: the overview's numbers, counted over the whole table. */
export interface Stats {
  total: number;
  admins: number;
  users: number;
  verified: number;
  onboarded: number;
  googleAccounts: number;
  newLast7Days: number;
}

/** Narrows a list read: an address search, a role, and the page. */
export interface Filter {
  search: string;
  role: string;
  limit: number;
  offset: number;
}

/** Returns the filter with its bounds applied, so every caller reads the same page. */
export function normaliseFilter(filter: Filter): Filter {
  let limit = filter.limit;
  if (limit <= 0) limit = DefaultPageSize;
  if (limit > MaxPageSize) limit = MaxPageSize;
  return {
    search: filter.search,
    role: filter.role,
    limit,
    offset: filter.offset < 0 ? 0 : filter.offset,
  };
}

/** True when the filter asks for something that exists. Only the role can be wrong. */
export function filterIsValid(filter: Filter): boolean {
  return filter.role === "" || filter.role === RoleUser || filter.role === RoleAdmin;
}

/** The persistence contract for the account list and the one decision about an account. */
export interface Repository {
  users(filter: Filter): Promise<Account[]>;
  countUsers(filter: Filter): Promise<number>;
  stats(): Promise<Stats>;
  /** Reads one account. A missing id is ErrAccountNotFound. */
  accountByID(id: string): Promise<Account>;
  /** Makes an account a platform admin and answers with it. */
  promote(id: string): Promise<Account>;
  /** Takes the platform role away; answers ErrLastAdmin when it is the only one. */
  demote(id: string): Promise<Account>;
  /** Per account id, how many workspaces it belongs to. Ids with no membership are absent. */
  workspaceCounts(ids: string[]): Promise<Record<string, number>>;
}

/** A promotion or a demotion. */
export interface ChangeRoleRequest {
  /** The admin making the change, taken from the session rather than from the body. */
  actorID: string;
  targetID: string;
  role: string;
}

/** The operator surface's use case contract. */
export interface Service {
  /** Answers what the deployment's accounts add up to. */
  overview(): Promise<Stats>;
  /** Answers a page of accounts, with the total the filter matches. */
  list(filter: Filter): Promise<[Account[], number]>;
  /** Promotes or demotes an account. */
  changeRole(req: ChangeRoleRequest): Promise<Account>;
}
