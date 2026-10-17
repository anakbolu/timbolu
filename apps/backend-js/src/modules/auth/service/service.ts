/**
 * The authentication use cases. It depends on the domain contracts and on the
 * platform primitives (password hashing, access tokens, mail, rate limiting,
 * Google), never on HTTP or SQL.
 *
 * Ported 1:1 from internal/modules/auth/service/service.go.
 */

import type {
  GoogleCallbackRequest,
  LoginRequest,
  NewSession,
  RegisterRequest,
  Repository,
  ResetPasswordRequest,
  Service as ServicePortContract,
  Session,
  User,
} from "../domain/auth.ts";
import {
  ErrEmailNotVerified,
  ErrGoogleEmailUnverified,
  ErrGoogleNotConfigured,
  ErrInvalidCredentials,
  ErrInvalidInput,
  ErrInvalidToken,
  ErrTooManyAttempts,
  ErrUserNotFound,
  ErrWeakPassword,
  isAuthError,
  wrapAuthError,
} from "../domain/auth.ts";
import type {
  GoogleProvider,
  LoginThrottle,
  Mailer,
  Passwords,
  StateSigner,
  Tokens,
  TokenIssuer,
} from "../domain/ports.ts";
import { hashToken } from "../domain/ports.ts";

/** Holds the authentication lifetimes and the login throttle. */
export interface Config {
  /** How long a refresh session stays valid. Milliseconds. */
  refreshTTL: number;
  /** How long an email verification link stays valid. Milliseconds. */
  verificationTTL: number;
  /** How long a password reset link stays valid. Milliseconds. */
  resetTTL: number;
  /**
   * The base URL of the web app, used to build the links that are emailed to
   * the user.
   */
  frontendURL: string;
  /**
   * Configure the per-email and per-IP login buckets.
   */
  loginThrottleCapacity: number;
  loginThrottleRefill: number;
  /**
   * Makes a new account confirm its address before it may use the product. When
   * false, register marks the address verified at once and sends no link:
   * signing up is the whole of the gate.
   */
  requireEmailVerification: boolean;
  /** Injectable so tests can control expiry. */
  clock?: () => Date;
}

/**
 * The service dependencies. Everything except the repository is optional, so a
 * deployment without Redis or Google still serves password authentication.
 */
export interface Deps {
  repository: Repository;
  /** Signs access tokens. */
  issuer: TokenIssuer;
  /** Signs the OAuth state value. */
  states: StateSigner | null;
  /** Delivers the verification and reset links. Optional. */
  mailer: Mailer | null;
  /** Limits repeated login attempts. Optional: the credentials check still runs. */
  throttle: LoginThrottle | null;
  /** The OIDC client. Optional: without it Google sign-in reports not configured. */
  google: GoogleProvider | null;
  /** Hashes and verifies account passwords. */
  passwords: Passwords;
  /** Generates the one-time tokens emailed to users. */
  tokens: Tokens;
  /**
   * Reports a failure that must not fail the request it belongs to, such as a
   * verification link that could not be delivered. Optional.
   */
  logf?: (format: string, ...args: unknown[]) => void;
  config: Config;
}

/**
 * What the handler calls. The concrete class is the implementation; the port is
 * re-exported under the name the module contract uses.
 */
export type ServicePort = ServicePortContract;

/** The subject of every verification email. */
const verificationSubject = "Verifikasi email Bolu";

/** Builds the auth service. */
export class Service implements ServicePort {
  private readonly deps: Deps;
  private readonly clock: () => Date;

  constructor(deps: Deps) {
    // Go takes `Deps` by value, so its defaults never reach the caller. Copy
    // the config for the same reason: the container may share the object.
    const config: Config = { ...deps.config };
    if (!config.clock) config.clock = () => new Date();
    if (!(config.refreshTTL > 0)) config.refreshTTL = 30 * 24 * 60 * 60 * 1000;
    if (!(config.verificationTTL > 0)) config.verificationTTL = 24 * 60 * 60 * 1000;
    if (!(config.resetTTL > 0)) config.resetTTL = 60 * 60 * 1000;
    if (!(config.loginThrottleCapacity > 0)) config.loginThrottleCapacity = 5;
    if (!(config.loginThrottleRefill > 0)) {
      // One attempt every five seconds, so a burst is absorbed but a legitimate
      // user who mistyped can retry shortly.
      config.loginThrottleRefill = 0.2;
    }

    this.deps = { ...deps, config, logf: deps.logf ?? (() => {}) };
    this.clock = config.clock;
  }

  /** Creates a password account and emails a verification link. */
  async register(req: RegisterRequest): Promise<User> {
    const email = normalizeEmail(req.email);

    try {
      await this.deps.passwords.validate(req.password);
    } catch (err) {
      throw wrapAuthError(ErrWeakPassword, err);
    }

    const hash = await this.deps.passwords.hash(req.password);
    const user = await this.deps.repository.createUser(email, hash);

    if (!this.deps.config.requireEmailVerification) {
      // Verification is off in this deployment: the account is usable the moment
      // it exists, so the address is marked verified rather than left pending.
      return this.deps.repository.markEmailVerified(user.id);
    }

    const body = await this.issueVerificationToken(user);
    try {
      await this.send(user.email, verificationSubject, body);
    } catch (err) {
      // The account and its token exist, so a mail server having a bad day must
      // not turn a successful sign-up into a failure the user cannot see past.
      // The link stays valid: resend-verification issues a fresh one.
      this.deps.logf!(
        "auth: verification email to %s failed: %v",
        user.email,
        err instanceof Error ? err.message : err,
      );
    }

    return user;
  }

  /**
   * Sends a fresh verification link. It succeeds even when the account does not
   * exist, so the endpoint cannot be used to discover addresses.
   */
  async resendVerification(email: string): Promise<void> {
    if (!this.deps.config.requireEmailVerification) {
      // There is nothing to verify in this deployment, and answering as if a
      // link were sent would be a lie the caller cannot see through.
      return;
    }

    let normalized: string;
    try {
      normalized = normalizeEmail(email);
    } catch {
      // A malformed address is answered like an unknown one, so the response
      // never depends on what the caller typed.
      return;
    }

    let user: User;
    try {
      user = await this.deps.repository.userByEmail(normalized);
    } catch (err) {
      // An unknown address is answered exactly like a known one, so the endpoint
      // cannot be used to discover accounts.
      if (isAuthError(err, ErrUserNotFound)) return;
      throw err;
    }
    if (user.emailVerified) return;

    await this.sendVerification(user);
  }

  /** Consumes a verification token and marks the account verified. */
  async verifyEmail(token: string): Promise<User> {
    if (token.trim() === "") throw ErrInvalidToken;

    let userID: string;
    try {
      userID = await this.deps.repository.consumeEmailVerificationToken(hashToken(token));
    } catch (err) {
      if (isAuthError(err, ErrUserNotFound)) {
        // No live token matched: already used, expired, or unknown. The
        // distinction is not useful to the caller.
        throw ErrInvalidToken;
      }
      throw err;
    }

    return this.deps.repository.markEmailVerified(userID);
  }

  /** Verifies credentials and opens a session. */
  async login(req: LoginRequest): Promise<Session> {
    let email: string;
    try {
      email = normalizeEmail(req.email);
    } catch {
      throw ErrInvalidCredentials;
    }

    await this.allowLoginAttempt(email, req.ip);

    let user: User;
    try {
      user = await this.deps.repository.userByEmail(email);
    } catch (err) {
      if (isAuthError(err, ErrUserNotFound)) throw ErrInvalidCredentials;
      throw err;
    }

    const hash = await this.deps.repository.passwordHash(user.id);
    if (hash === "") {
      // Google-only account: there is no password to check.
      throw ErrInvalidCredentials;
    }

    let ok: boolean;
    try {
      ok = await this.deps.passwords.verify(hash, req.password);
    } catch {
      throw ErrInvalidCredentials;
    }
    if (!ok) throw ErrInvalidCredentials;

    if (!this.deps.config.requireEmailVerification && !user.emailVerified) {
      // This deployment does not ask for a confirmed address, so an account that
      // predates the switch is healed at sign-in rather than left able to
      // authenticate but refused everywhere afterwards.
      user = await this.deps.repository.markEmailVerified(user.id);
    }

    const session = await this.openSession(user, req.userAgent, req.ip, null);

    // A successful sign-in clears the throttle for this email and IP.
    await this.resetLoginAttempts(email, req.ip);

    return session;
  }

  /**
   * Rotates a refresh session: the presented token is revoked and a new one is
   * issued, so a stolen token is usable at most once.
   */
  async refresh(refreshToken: string): Promise<Session> {
    if (refreshToken.trim() === "") throw ErrInvalidToken;

    let record;
    try {
      record = await this.deps.repository.sessionByTokenHash(hashToken(refreshToken));
    } catch (err) {
      if (isAuthError(err, ErrUserNotFound)) throw ErrInvalidToken;
      throw err;
    }
    if (!this.sessionUsable(record)) throw ErrInvalidToken;

    const user = await this.deps.repository.userByID(record.userId);
    await this.deps.repository.revokeSession(record.id);

    return this.openSession(user, "", "", record.id);
  }

  /**
   * Revokes the session behind a refresh token. It is idempotent: logging out
   * twice, or with an unknown token, is not an error.
   */
  async logout(refreshToken: string): Promise<void> {
    if (refreshToken.trim() === "") return;
    await this.deps.repository.revokeSessionByTokenHash(hashToken(refreshToken));
  }

  /**
   * Emails a reset link. Like resend-verification, it always reports success so
   * the endpoint cannot enumerate accounts.
   */
  async requestPasswordReset(email: string): Promise<void> {
    let normalized: string;
    try {
      normalized = normalizeEmail(email);
    } catch {
      return;
    }

    let user: User;
    try {
      user = await this.deps.repository.userByEmail(normalized);
    } catch (err) {
      if (isAuthError(err, ErrUserNotFound)) return;
      throw err;
    }

    const { raw, hash } = this.deps.tokens.generate();
    const expiresAt = new Date(this.clock().getTime() + this.deps.config.resetTTL);
    await this.deps.repository.createPasswordResetToken(user.id, hash, expiresAt);

    await this.send(
      user.email,
      "Atur ulang kata sandi Bolu",
      `Buka tautan ini untuk mengatur ulang kata sandi:\n\n${this.deps.config.frontendURL.replace(/\/+$/, "")}/forgot-password?token=${raw}\n\nTautan berlaku ${humanize(this.deps.config.resetTTL)}.`,
    );
  }

  /**
   * Consumes a reset token, stores the new password, and revokes every existing
   * session so a compromised session cannot outlive the reset.
   */
  async resetPassword(req: ResetPasswordRequest): Promise<void> {
    if (req.token.trim() === "") throw ErrInvalidToken;

    try {
      await this.deps.passwords.validate(req.newPassword);
    } catch (err) {
      throw wrapAuthError(ErrWeakPassword, err);
    }

    let userID: string;
    try {
      userID = await this.deps.repository.consumePasswordResetToken(hashToken(req.token));
    } catch (err) {
      if (isAuthError(err, ErrUserNotFound)) throw ErrInvalidToken;
      throw err;
    }

    const hash = await this.deps.passwords.hash(req.newPassword);
    await this.deps.repository.updatePasswordHash(userID, hash);
    await this.deps.repository.revokeSessionsForUser(userID);
  }

  /** Returns the consent URL with a signed state. */
  async googleAuthURL(): Promise<string> {
    if (!this.deps.google || !this.deps.states) throw ErrGoogleNotConfigured;

    const state = await this.deps.states.sign();
    return this.deps.google.authCodeURL(state);
  }

  /**
   * Completes the Google flow: it verifies the state, exchanges the code, and
   * either signs in, links to an existing account with the same email, or
   * creates a new account.
   */
  async googleCallback(req: GoogleCallbackRequest): Promise<Session> {
    if (!this.deps.google || !this.deps.states) throw ErrGoogleNotConfigured;

    try {
      await this.deps.states.verify(req.state);
    } catch (err) {
      throw wrapAuthError(ErrInvalidToken, err);
    }

    let info;
    try {
      info = await this.deps.google.exchange(req.code);
    } catch (err) {
      if (isAuthError(err, ErrGoogleEmailUnverified)) {
        throw wrapAuthError(ErrInvalidToken, "provider did not verify the email");
      }
      throw err;
    }

    const email = normalizeEmail(info.email);

    let user: User;
    try {
      user = await this.deps.repository.userByGoogleSubject(info.subject);
    } catch (err) {
      if (!isAuthError(err, ErrUserNotFound)) throw err;

      // Not linked yet. An account with the same email is linked rather than
      // duplicated.
      let existing: User | null = null;
      try {
        existing = await this.deps.repository.userByEmail(email);
      } catch (lookupErr) {
        if (!isAuthError(lookupErr, ErrUserNotFound)) throw lookupErr;
      }

      user = existing
        ? await this.deps.repository.linkGoogleSubject(existing.id, info.subject)
        : await this.deps.repository.createGoogleUser(email, info.subject);
    }

    return this.openSession(user, req.userAgent, req.ip, null);
  }

  /**
   * Verifies an access token and confirms its session is still usable, then
   * returns the account.
   */
  async authenticate(accessToken: string): Promise<User> {
    let claims;
    try {
      claims = await this.deps.issuer.verify(accessToken);
    } catch (err) {
      throw wrapAuthError(ErrInvalidToken, err);
    }

    let record;
    try {
      record = await this.deps.repository.sessionByID(claims.sessionId);
    } catch (err) {
      if (isAuthError(err, ErrUserNotFound)) throw ErrInvalidToken;
      throw err;
    }
    if (!this.sessionUsable(record) || record.userId !== claims.userId) {
      throw ErrInvalidToken;
    }

    return this.deps.repository.userByID(claims.userId);
  }

  /** Returns the account behind an access token. */
  async currentUser(userID: string): Promise<User> {
    if (!userID) throw ErrUserNotFound;
    return this.deps.repository.userByID(userID);
  }

  /** Rejects an account whose email is not verified yet. */
  async assertVerified(userID: string): Promise<void> {
    const user = await this.currentUser(userID);
    if (!user.emailVerified) throw ErrEmailNotVerified;
  }

  /** Creates the refresh session and issues the matching access token. */
  private async openSession(
    user: User,
    userAgent: string,
    ip: string,
    rotatedFrom: string | null,
  ): Promise<Session> {
    const { raw, hash } = this.deps.tokens.generate();

    const expiresAt = new Date(this.clock().getTime() + this.deps.config.refreshTTL);
    const record = await this.deps.repository.createSession({
      userId: user.id,
      refreshTokenHash: hash,
      userAgent,
      ip,
      expiresAt,
      rotatedFrom,
    } satisfies NewSession);

    const { token, expiresAt: accessExpiry } = await this.deps.issuer.issue(user.id, record.id);

    return {
      accessToken: token,
      accessTokenExp: accessExpiry,
      refreshToken: raw,
      refreshTokenExp: record.expiresAt,
      user,
    };
  }

  /** Reports whether a stored session may still be exchanged. */
  private sessionUsable(record: { revokedAt: Date | null; expiresAt: Date }): boolean {
    if (record.revokedAt !== null) return false;
    return this.clock().getTime() < record.expiresAt.getTime();
  }

  /**
   * Spends a token from the per-email and per-IP buckets. When Redis is not
   * configured the throttle is skipped rather than blocking every sign-in.
   */
  private async allowLoginAttempt(email: string, ip: string): Promise<void> {
    if (!this.deps.throttle) return;

    const keys = ["auth:login:email:" + email];
    if (ip.trim() !== "") keys.push("auth:login:ip:" + ip);

    for (const key of keys) {
      let result;
      try {
        result = await this.deps.throttle.allow(
          key,
          this.deps.config.loginThrottleCapacity,
          this.deps.config.loginThrottleRefill,
        );
      } catch {
        // A Redis failure degrades to "no throttling" rather than "nobody can
        // sign in": the credentials check still runs below.
        return;
      }
      if (!result.allowed) {
        throw wrapAuthError(ErrTooManyAttempts, `retry in ${goDuration(result.retryAfter)}`);
      }
    }
  }

  private async resetLoginAttempts(email: string, ip: string): Promise<void> {
    if (!this.deps.throttle) return;
    await this.deps.throttle.reset("auth:login:email:" + email).catch(() => undefined);
    if (ip.trim() !== "") {
      await this.deps.throttle.reset("auth:login:ip:" + ip).catch(() => undefined);
    }
  }

  /** Issues a one-time link and emails it. */
  private async sendVerification(user: User): Promise<void> {
    const body = await this.issueVerificationToken(user);
    await this.send(user.email, verificationSubject, body);
  }

  /**
   * Stores a one-time verification link for the account and returns the body of
   * the email that carries it. It is separate from the delivery because the two
   * failures mean different things: a token that could not be written is a
   * broken request, a message that could not be sent is not.
   */
  private async issueVerificationToken(user: User): Promise<string> {
    const { raw, hash } = this.deps.tokens.generate();

    const expiresAt = new Date(this.clock().getTime() + this.deps.config.verificationTTL);
    await this.deps.repository.createEmailVerificationToken(user.id, hash, expiresAt);

    return `Buka tautan ini untuk memverifikasi email:\n\n${this.deps.config.frontendURL.replace(/\/+$/, "")}/verify?token=${raw}\n\nTautan berlaku ${humanize(this.deps.config.verificationTTL)}.`;
  }

  private async send(to: string, subject: string, body: string): Promise<void> {
    if (!this.deps.mailer) return;
    await this.deps.mailer.send(to, subject, body);
  }
}

/**
 * Validates and lowercases an address, mirroring Go's `mail.ParseAddress`
 * acceptance of a bare address or `Name <address>`.
 */
function normalizeEmail(email: string): string {
  const trimmed = email.trim();
  if (trimmed === "") throw wrapAuthError(ErrInvalidInput, "email is required");

  const address = parseAddress(trimmed);
  if (address === null) throw wrapAuthError(ErrInvalidInput, "email is not valid");
  return address.toLowerCase();
}

/**
 * Reads the address out of a plain or display-name form. Go's parser rejects
 * control characters, spaces inside the address, and anything without exactly
 * one `@` with a non-empty local part and domain.
 */
function parseAddress(input: string): string | null {
  let candidate = input;

  const angled = /^[^<>]*<([^<>]+)>$/.exec(input);
  if (angled) {
    candidate = angled[1]!.trim();
  }

  // Only printable ASCII, as RFC 5322 atoms allow in the unquoted form.
  if (!/^[\x20-\x7e]+$/.test(candidate)) return null;
  if (/\s/.test(candidate)) return null;

  const parts = candidate.split("@");
  if (parts.length !== 2) return null;

  const [local, domain] = parts as [string, string];
  if (local === "" || domain === "") return null;
  if (!/^[A-Za-z0-9!#$%&'*+/=?^_`{|}~.-]+$/.test(local)) return null;
  if (!/^[A-Za-z0-9.-]+$/.test(domain)) return null;
  if (domain.startsWith(".") || domain.endsWith(".") || domain.includes("..")) return null;

  return candidate;
}

/**
 * Renders a duration in milliseconds the way Go's `time.Duration.String()` does
 * after `Round(time.Second)`.
 */
function goDuration(ms: number): string {
  const total = Math.round(ms / 1000);
  const sign = total < 0 ? "-" : "";
  const seconds = Math.abs(total);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  if (hours > 0) return `${sign}${hours}h${minutes}m${rest}s`;
  if (minutes > 0) return `${sign}${minutes}m${rest}s`;
  return `${sign}${rest}s`;
}

/** Renders a duration the way a user reads it. */
function humanize(ms: number): string {
  const day = 24 * 60 * 60 * 1000;
  const hour = 60 * 60 * 1000;
  if (ms >= day) {
    const days = Math.floor(ms / day);
    return days === 1 ? "1 hari" : `${days} hari`;
  }
  if (ms >= hour) {
    const hours = Math.floor(ms / hour);
    return hours === 1 ? "1 jam" : `${hours} jam`;
  }
  return `${Math.floor(ms / 60000)} menit`;
}

