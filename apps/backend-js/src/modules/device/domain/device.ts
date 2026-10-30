/**
 * The device contract: the Expo push address a phone registered, and the two
 * writes a client makes about it.
 *
 * A device token is an address rather than a session. It is what the push sender
 * looks up when a task finishes or a draft waits for a person, and the rules are
 * deliberately small so the service can be tested without a database:
 *
 *   - **One row per account and token.** A phone re-registers on every launch;
 *     the second registration updates the row it already has, because a table
 *     that grew per launch would be a table of duplicates nobody can
 *     deduplicate later.
 *   - **The token is the account's.** A workspace is recorded with it, but
 *     removing that workspace must not remove the address, and another account's
 *     token is invisible rather than forbidden. That invisibility is what lets
 *     the delete answer 404 without a second query.
 *
 * Ported 1:1 from internal/modules/device/domain/device.go.
 */

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

// Platforms a device may register as, matching the check constraint on
// `device_tokens.platform`.
export const PlatformIOS = "ios";
export const PlatformAndroid = "android";

/** Platforms lists every platform a registration may name, in the order the check constraint reads. */
export const Platforms: string[] = [PlatformIOS, PlatformAndroid];

/**
 * TokenMaxLength bounds one push token. Expo's tokens are far shorter, so the
 * ceiling exists to refuse a body that is not a token at all rather than to
 * describe a real one.
 */
export const TOKEN_MAX_LENGTH = 512;

// Errors the service and the handler map onto responses.
/** A registration that cannot be honoured: no token, a token longer than a push address can be, or a platform the app does not run on. */
export const ErrInvalidInput = new Error("device: invalid input");
/** A token that is not this account's, or not registered at all. The two are deliberately one answer. */
export const ErrNotFound = new Error("device: not found");

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string, options?: ErrorOptions): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`, options);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) {
      return true;
    }
    if (!(current instanceof Error)) {
      return false;
    }
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * True for an absent id, which Go expresses as the zero uuid. Named because the
 * comparison is against a magic constant, and because `device_tokens` stores an
 * absent workspace as NULL rather than as all zeroes.
 */
export function isNilID(id: string | null | undefined): boolean {
  return !id || id === NilUUID;
}

/**
 * Scope is the tenant a call runs as. The user owns the token; the workspace is
 * where it was last registered from.
 */
export interface Scope {
  userId?: string;
  workspaceId: string;
}

/**
 * Device is one registered push address.
 */
export interface Device {
  id: string;
  /**
   * The account the address belongs to, which is also the only account that can
   * read or delete it.
   */
  userId: string;
  /**
   * Where the registration came from. Empty when the workspace was removed; the
   * address survives it.
   */
  workspaceId: string;
  platform: string;
  token: string;
  /** What the phone reported. Empty when it did not say. */
  appVersion: string;
  /**
   * Moves on every registration, which is what tells a sender the address is
   * still live.
   */
  lastSeenAt: Date;
  createdAt: Date;
}

/** RegisterRequest is one registration from a phone. */
export interface RegisterRequest {
  token: string;
  platform: string;
  appVersion: string;
}

/** Repository persists device tokens. */
export interface Repository {
  /**
   * Stores a token, or updates the row the same account already has for it. The
   * row it answers with is the stored one, so `last_seen_at` is the moment of
   * this registration.
   */
  register(scope: Scope, request: RegisterRequest): Promise<Device>;
  /**
   * Removes one of the caller's tokens. A token that is not the caller's is
   * reported as ErrNotFound rather than as a refusal.
   */
  remove(scope: Scope, token: string): Promise<void>;
  /**
   * Every address the caller registered, newest first. The policy on
   * `device_tokens` is keyed on the user, so the scope's user is the whole of
   * the filter; an account with no phones answers an empty list, not an error.
   */
  listTokens(scope: Scope): Promise<Device[]>;
}

/** Service is what the HTTP handler calls. */
export interface Service {
  /** Validates a registration and stores it, updating the row a repeated token already has. */
  register(scope: Scope, request: RegisterRequest): Promise<Device>;
  /** Removes one of the caller's tokens. */
  remove(scope: Scope, token: string): Promise<void>;
}
