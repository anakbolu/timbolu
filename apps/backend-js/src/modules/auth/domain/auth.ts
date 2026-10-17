/**
 * The authentication contracts: the entities, the repository and service
 * interfaces, and the request/response shapes.
 *
 * Ported 1:1 from internal/modules/auth/domain/auth.go. The Go sentinels are
 * `AppError`s carrying the machine-readable `code` the container guards branch
 * on, so a wrapped error keeps both the code and the Go message prefix.
 */

import { AppError } from "../../../platform/errors.ts";

/**
 * Errors returned by the service. Handlers map them to status codes, so the
 * mapping lives in one place instead of being guessed per endpoint.
 *
 * Statuses mirror the Go handler's `fail` switch; the container only reads
 * `code`.
 */
export const ErrEmailTaken = new AppError(409, "auth: email is already registered", "email_taken");
export const ErrInvalidCredentials = new AppError(401, "auth: email or password is incorrect", "invalid_credentials");
export const ErrEmailNotVerified = new AppError(403, "auth: email is not verified", "email_not_verified");
export const ErrInvalidToken = new AppError(400, "auth: token is invalid, expired, or already used", "invalid_token");
export const ErrTooManyAttempts = new AppError(429, "auth: too many attempts", "too_many_attempts");
export const ErrUserNotFound = new AppError(404, "auth: user not found", "user_not_found");
export const ErrWeakPassword = new AppError(400, "auth: password does not meet the policy", "weak_password");
export const ErrGoogleNotConfigured = new AppError(503, "auth: google sign-in is not configured", "google_not_configured");
/** The request itself is wrong (a malformed address). Handlers answer 400. */
export const ErrInvalidInput = new AppError(400, "auth: invalid input", "invalid_input");
export const ErrEmailMismatch = new AppError(
  400,
  "auth: invitation email does not match the signed-in account",
  "email_mismatch",
);
/**
 * The provider did not assert the address, so the account must not be linked or
 * created from it.
 */
export const ErrGoogleEmailUnverified = new AppError(
  400,
  "auth: google did not verify the email address",
  "google_email_unverified",
);

/**
 * Wraps a sentinel with Go's `fmt.Errorf("%w: %w", sentinel, cause)` message
 * shape while keeping the sentinel's status and code.
 */
export function wrapAuthError(sentinel: AppError, cause: unknown): AppError {
  const detail = cause instanceof Error ? cause.message : String(cause);
  return new AppError(sentinel.status, `${sentinel.message}: ${detail}`, sentinel.code);
}

/** True when `err` carries the sentinel's machine-readable code. */
export function isAuthError(err: unknown, sentinel: AppError): boolean {
  return err instanceof AppError && err.code === sentinel.code;
}

/**
 * The platform role of an account, matching the check constraint on
 * `users.role`.
 *
 * It is not the same thing as a workspace role, which is stored on `members`
 * and answers "what may this person do inside this workspace". This one answers
 * "may this person see the deployment", and only the operator surface at /admin
 * reads it.
 */
export const RoleUser = "user";
export const RoleAdmin = "admin";

/**
 * Reports whether a platform role may reach the operator surface. An
 * unrecognised or empty role is not one, so a value that never went through the
 * column's check constraint still fails closed.
 */
export function isAdmin(role: string): boolean {
  return role === RoleAdmin;
}

/** An account. The password hash never leaves the backend. */
export interface User {
  id: string;
  email: string;
  emailVerified: boolean;
  hasGoogle: boolean;
  hasPassword: boolean;
  onboarded: boolean;
  /**
   * The platform role. It travels with the account rather than with a session,
   * so a promotion takes effect on the next request instead of on the next
   * sign-in.
   */
  role: string;
  createdAt: Date;
}

/**
 * What a successful sign-in returns: an access token for API calls and a
 * refresh token for a new access token.
 */
export interface Session {
  accessToken: string;
  accessTokenExp: Date;
  refreshToken: string;
  refreshTokenExp: Date;
  user: User;
}

/** A stored refresh session. */
export interface SessionRecord {
  id: string;
  userId: string;
  expiresAt: Date;
  revokedAt: Date | null;
}

/** The sign-up payload. */
export interface RegisterRequest {
  email: string;
  password: string;
}

/** Carries the credentials and the client context recorded on the session. */
export interface LoginRequest {
  email: string;
  password: string;
  userAgent: string;
  ip: string;
}

/** Carries the code and the state returned by Google. */
export interface GoogleCallbackRequest {
  code: string;
  state: string;
  userAgent: string;
  ip: string;
}

/** The password reset payload. */
export interface ResetPasswordRequest {
  token: string;
  newPassword: string;
}

/** A session to store. */
export interface NewSession {
  userId: string;
  refreshTokenHash: string;
  userAgent: string;
  ip: string;
  expiresAt: Date;
  rotatedFrom: string | null;
}

/**
 * The persistence contract. The auth tables are not tenant scoped, so the
 * implementation needs no scope; every write is identified by a user id, a
 * session id, or a token hash.
 *
 * Go returns `domain.ErrUserNotFound` from the repository when a row is absent;
 * the TS implementation throws the same sentinel.
 */
export interface Repository {
  // Users.
  createUser(email: string, passwordHash: string): Promise<User>;
  createGoogleUser(email: string, subject: string): Promise<User>;
  userByEmail(email: string): Promise<User>;
  userByID(id: string): Promise<User>;
  userByGoogleSubject(subject: string): Promise<User>;
  linkGoogleSubject(userID: string, subject: string): Promise<User>;
  markEmailVerified(userID: string): Promise<User>;
  updatePasswordHash(userID: string, hash: string): Promise<User>;
  markOnboarded(userID: string): Promise<User>;
  /** Returns the stored hash; kept out of User so it is never carried around. */
  passwordHash(userID: string): Promise<string>;

  // Sessions.
  createSession(session: NewSession): Promise<SessionRecord>;
  sessionByTokenHash(tokenHash: string): Promise<SessionRecord>;
  sessionByID(id: string): Promise<SessionRecord>;
  revokeSession(id: string): Promise<void>;
  revokeSessionByTokenHash(tokenHash: string): Promise<void>;
  revokeSessionsForUser(userID: string): Promise<number>;

  // One-time tokens.
  createEmailVerificationToken(userID: string, tokenHash: string, expiresAt: Date): Promise<void>;
  consumeEmailVerificationToken(tokenHash: string): Promise<string>;
  createPasswordResetToken(userID: string, tokenHash: string, expiresAt: Date): Promise<void>;
  consumePasswordResetToken(tokenHash: string): Promise<string>;
}

/** The authentication use case contract. */
export interface Service {
  register(req: RegisterRequest): Promise<User>;
  verifyEmail(token: string): Promise<User>;
  resendVerification(email: string): Promise<void>;
  login(req: LoginRequest): Promise<Session>;
  refresh(refreshToken: string): Promise<Session>;
  logout(refreshToken: string): Promise<void>;
  requestPasswordReset(email: string): Promise<void>;
  resetPassword(req: ResetPasswordRequest): Promise<void>;
  googleAuthURL(): Promise<string>;
  googleCallback(req: GoogleCallbackRequest): Promise<Session>;
  currentUser(userID: string): Promise<User>;
  /**
   * Verifies an access token and returns the account behind it. It also checks
   * that the session the token was issued for is still alive, so signing out or
   * resetting a password invalidates outstanding tokens.
   */
  authenticate(accessToken: string): Promise<User>;
  /**
   * Rejects an account that has not confirmed its email. The workspace
   * endpoints call it, so an unverified account cannot use the features that
   * require verification.
   */
  assertVerified(userID: string): Promise<void>;
}
