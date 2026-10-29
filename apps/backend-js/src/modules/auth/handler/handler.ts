/**
 * The auth module over HTTP. It talks to the domain service interface only, and
 * never to a repository.
 *
 * Ported 1:1 from internal/modules/auth/handler/handler.go. Every message and
 * error code is copied verbatim — the frontend branches on `error.code`.
 */

import type { Hono } from "hono";
import { getCookie, setCookie } from "hono/cookie";
import type { AppContext, AppEnv } from "../../../platform/context.ts";
import { currentUserId } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error as errorEnvelope, ok } from "../../../platform/response.ts";
import type { Service, Session, User } from "../domain/auth.ts";
import {
  ErrEmailNotVerified,
  ErrEmailTaken,
  ErrGoogleNotConfigured,
  ErrInvalidCredentials,
  ErrInvalidInput,
  ErrInvalidToken,
  ErrTooManyAttempts,
  ErrUserNotFound,
  ErrWeakPassword,
  isAuthError,
} from "../domain/auth.ts";

/**
 * The httpOnly cookie that carries the refresh token. Keeping the refresh token
 * out of JavaScript is what makes a stolen access token expire quickly instead
 * of being refreshable.
 */
export const CookieName = "bolu_refresh";

/** Describes where the refresh cookie is valid. */
export interface CookieConfig {
  /**
   * The API prefix plus /auth, so the cookie is not sent to every endpoint.
   */
  path: string;
  /** Optional; empty means the host that served the request. */
  domain: string;
  /** Must be true outside local development. */
  secure: boolean;
  /**
   * "lax" by default, which still allows the OAuth redirect back to the API.
   */
  sameSite: string;
}

/** The handler settings. */
export interface Config {
  cookie: CookieConfig;
  /** Where the Google callback sends the browser back to. */
  frontendURL: string;
}

/** The public shape of an account. */
export interface userPayload {
  id: string;
  email: string;
  email_verified: boolean;
  onboarded: boolean;
  has_password: boolean;
  has_google: boolean;
  /**
   * The platform role: `admin` reaches /admin, `user` is everybody else. The
   * screen reads it from /auth/me to decide whether to offer the link; the API
   * checks it again on every admin request.
   */
  role: string;
}

/**
 * What a sign-in returns. The refresh token travels in the httpOnly cookie, so
 * it is not repeated here.
 *
 * `refresh_token` is set only when the caller presented the token in the body
 * instead of the cookie: a native client has no cookie jar, so the cookie is not
 * a channel it can read, and the rotated token would otherwise be lost after a
 * single refresh.
 */
export interface sessionPayload {
  access_token: string;
  access_expires_at: string;
  refresh_expires_at: string;
  user: userPayload;
  refresh_token?: string;
}

/** Serves the auth endpoints. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
    private readonly cfg: Config,
  ) {
    if (!cfg.cookie.path) cfg.cookie.path = "/";
    if (!cfg.cookie.sameSite) cfg.cookie.sameSite = "Lax";
  }

  /** Creates a password account and emails a verification link. */
  register = async (c: AppContext): Promise<Response> => {
    const req = await readFields(c, ["email", "password"]);
    if (!req.ok || req.value === null) return errorEnvelope(c, 400, "invalid request body", "invalid_request");

    try {
      const user = await this.service.register({ email: req.value.email ?? "", password: req.value.password ?? "" });

      // The message follows the account's state: a deployment that has turned
      // verification off creates an account that is already usable, and telling
      // its owner to check their email would send them to a link that does not
      // exist.
      const message = user.emailVerified ? "account created" : "account created, check your email to verify it";
      return created(c, message, toUserPayload(user));
    } catch (err) {
      return this.fail(c, "register", err);
    }
  };

  /** Consumes the one-time verification token. */
  verifyEmail = async (c: AppContext): Promise<Response> => {
    const req = await readFields(c, ["token"]);
    if (!req.ok || req.value === null) return errorEnvelope(c, 400, "invalid request body", "invalid_request");

    try {
      const user = await this.service.verifyEmail(req.value.token ?? "");
      return ok(c, "email verified", toUserPayload(user));
    } catch (err) {
      return this.fail(c, "verify email", err);
    }
  };

  /** Sends a new verification link. */
  resendVerification = async (c: AppContext): Promise<Response> => {
    const req = await readFields(c, ["email"]);
    if (!req.ok || req.value === null) return errorEnvelope(c, 400, "invalid request body", "invalid_request");

    try {
      await this.service.resendVerification(req.value.email ?? "");
    } catch (err) {
      return this.fail(c, "resend verification", err);
    }

    // The answer is the same whether or not the address exists, so the endpoint
    // cannot be used to enumerate accounts.
    return ok(c, "if the address exists and is unverified, a new link has been sent");
  };

  /** Verifies credentials, opens a session, and sets the refresh cookie. */
  login = async (c: AppContext): Promise<Response> => {
    const req = await readFields(c, ["email", "password"]);
    if (!req.ok || req.value === null) return errorEnvelope(c, 400, "invalid request body", "invalid_request");

    try {
      const session = await this.service.login({
        email: req.value.email ?? "",
        password: req.value.password ?? "",
        userAgent: c.req.header("User-Agent") ?? "",
        ip: clientIp(c),
      });
      this.setRefreshCookie(c, session);
      return ok(c, "signed in", toSessionPayload(session));
    } catch (err) {
      return this.fail(c, "login", err);
    }
  };

  /**
   * Rotates the session using the refresh cookie, or the body token for
   * non-browser clients.
   */
  refresh = async (c: AppContext): Promise<Response> => {
    // A browser presents the refresh cookie, so the rotated token stays in the
    // cookie and JavaScript never sees it. A native client has no cookie jar:
    // the cookie is not a channel it can read at all, so when the token came
    // from the body the rotated one must go back in the body or the client can
    // refresh exactly once.
    let fromCookie = true;
    let token = getCookie(c, CookieName) ?? "";
    if (token === "") {
      fromCookie = false;
      const req = await readFields(c, ["token"]);
      if (!req.ok) return errorEnvelope(c, 400, "invalid request body", "invalid_request");
      token = req.value?.token ?? "";
    }

    try {
      const session = await this.service.refresh(token);
      this.setRefreshCookie(c, session);
      return ok(c, "session refreshed", toSessionPayload(session, !fromCookie));
    } catch (err) {
      this.clearRefreshCookie(c);
      return this.fail(c, "refresh session", err);
    }
  };

  /** Revokes the session and clears the cookie. */
  logout = async (c: AppContext): Promise<Response> => {
    let token = getCookie(c, CookieName) ?? "";
    if (token === "") {
      const req = await readFields(c, ["token"]);
      token = req.ok ? (req.value?.token ?? "") : "";
    }

    try {
      await this.service.logout(token);
    } catch (err) {
      return this.fail(c, "logout", err);
    }

    this.clearRefreshCookie(c);
    return ok(c, "signed out");
  };

  /** Emails a reset link. */
  forgotPassword = async (c: AppContext): Promise<Response> => {
    const req = await readFields(c, ["email"]);
    if (!req.ok || req.value === null) return errorEnvelope(c, 400, "invalid request body", "invalid_request");

    try {
      await this.service.requestPasswordReset(req.value.email ?? "");
    } catch (err) {
      return this.fail(c, "request password reset", err);
    }

    return ok(c, "if the address exists, a reset link has been sent");
  };

  /** Consumes the reset token and stores the new password. */
  resetPassword = async (c: AppContext): Promise<Response> => {
    const req = await readFields(c, ["token", "password"]);
    if (!req.ok || req.value === null) return errorEnvelope(c, 400, "invalid request body", "invalid_request");

    try {
      await this.service.resetPassword({ token: req.value.token ?? "", newPassword: req.value.password ?? "" });
    } catch (err) {
      return this.fail(c, "reset password", err);
    }

    this.clearRefreshCookie(c);
    return ok(c, "password updated, please sign in again");
  };

  /** Returns the authenticated account. */
  me = async (c: AppContext): Promise<Response> => {
    try {
      const user = await this.service.currentUser(currentUserId(c) ?? "");
      return ok(c, "ok", toUserPayload(user));
    } catch (err) {
      return this.fail(c, "current user", err);
    }
  };

  /** Redirects the browser to Google's consent screen. */
  googleStart = async (c: AppContext): Promise<Response> => {
    try {
      const url = await this.service.googleAuthURL();
      return c.redirect(url, 302);
    } catch (err) {
      return this.fail(c, "google start", err);
    }
  };

  /**
   * Completes the flow and returns the browser to the frontend with the refresh
   * cookie set. The access token is not put in the URL: the frontend calls
   * /auth/refresh, which reads the cookie.
   */
  googleCallback = async (c: AppContext): Promise<Response> => {
    let session: Session;
    try {
      session = await this.service.googleCallback({
        code: c.req.query("code") ?? "",
        state: c.req.query("state") ?? "",
        userAgent: c.req.header("User-Agent") ?? "",
        ip: clientIp(c),
      });
    } catch (err) {
      this.log.warn({ err }, "google sign-in failed");
      return c.redirect(this.frontendTarget("login", "google_error=1"), 302);
    }

    this.setRefreshCookie(c, session);

    const target = session.user.onboarded ? "login" : "onboarding";
    return c.redirect(this.frontendTarget(target, "google=ok"), 302);
  };

  private setRefreshCookie(c: AppContext, session: Session): void {
    setCookie(c, CookieName, session.refreshToken, {
      path: this.cfg.cookie.path,
      domain: this.cfg.cookie.domain || undefined,
      expires: session.refreshTokenExp,
      maxAge: Math.trunc((session.refreshTokenExp.getTime() - Date.now()) / 1000),
      secure: this.cfg.cookie.secure,
      httpOnly: true,
      sameSite: sameSiteValue(this.cfg.cookie.sameSite),
    });
  }

  private clearRefreshCookie(c: AppContext): void {
    setCookie(c, CookieName, "", {
      path: this.cfg.cookie.path,
      domain: this.cfg.cookie.domain || undefined,
      expires: new Date(Date.now() - 60 * 60 * 1000),
      maxAge: 0,
      secure: this.cfg.cookie.secure,
      httpOnly: true,
      sameSite: sameSiteValue(this.cfg.cookie.sameSite),
    });
  }

  private frontendTarget(path: string, query: string): string {
    const base = this.cfg.frontendURL.replace(/\/+$/, "");
    return base + "/" + path + "?" + query;
  }

  /**
   * Maps a domain error to a status code, so the mapping is defined once.
   * Mirrors Go's `fail` switch: the same statuses, messages and codes.
   */
  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (isAuthError(err, ErrEmailTaken)) {
      return errorEnvelope(c, 409, "email is already registered", "email_taken");
    }
    if (isAuthError(err, ErrInvalidCredentials)) {
      return errorEnvelope(c, 401, "email or password is incorrect", "invalid_credentials");
    }
    if (isAuthError(err, ErrEmailNotVerified)) {
      return errorEnvelope(c, 403, "verify your email first", "email_not_verified");
    }
    if (isAuthError(err, ErrInvalidToken)) {
      return errorEnvelope(c, 400, "token is invalid, expired, or already used", "invalid_token");
    }
    if (isAuthError(err, ErrTooManyAttempts)) {
      return errorEnvelope(c, 429, messageOf(err), "too_many_attempts");
    }
    if (isAuthError(err, ErrWeakPassword)) {
      return errorEnvelope(c, 400, messageOf(err), "weak_password");
    }
    if (isAuthError(err, ErrGoogleNotConfigured)) {
      return errorEnvelope(c, 503, "google sign-in is not configured", "google_not_configured");
    }
    if (isAuthError(err, ErrInvalidInput)) {
      return errorEnvelope(c, 400, messageOf(err), "invalid_input");
    }
    if (isAuthError(err, ErrUserNotFound)) {
      return errorEnvelope(c, 404, "account not found", "user_not_found");
    }

    this.log.error({ err, operation }, "auth request failed");
    return errorEnvelope(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the auth endpoints. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/auth/register", h.register);
  group.post("/auth/login", h.login);
  group.post("/auth/refresh", h.refresh);
  group.post("/auth/logout", h.logout);
  group.post("/auth/verify-email", h.verifyEmail);
  group.post("/auth/resend-verification", h.resendVerification);
  group.post("/auth/password/forgot", h.forgotPassword);
  group.post("/auth/password/reset", h.resetPassword);
  group.get("/auth/google/start", h.googleStart);
  group.get("/auth/google/callback", h.googleCallback);
}

/**
 * Registers the endpoints that require a signed-in user. The container applies
 * the auth middleware to this group.
 */
export function protectedRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/auth/me", h.me);
}

/** The public shape of an account. */
export function toUserPayload(user: User): userPayload {
  return {
    id: user.id,
    email: user.email,
    email_verified: user.emailVerified,
    onboarded: user.onboarded,
    has_password: user.hasPassword,
    has_google: user.hasGoogle,
    role: user.role,
  };
}

/**
 * What a sign-in returns. `includeRefreshToken` is off by default because the
 * cookie is the refresh channel for a browser; only a body-token refresh turns
 * it on, and then only because the client has no cookie to read.
 */
export function toSessionPayload(session: Session, includeRefreshToken = false): sessionPayload {
  const payload: sessionPayload = {
    access_token: session.accessToken,
    access_expires_at: session.accessTokenExp.getTime() ? session.accessTokenExp.toISOString() : "",
    refresh_expires_at: session.refreshTokenExp.getTime() ? session.refreshTokenExp.toISOString() : "",
    user: toUserPayload(session.user),
  };
  if (includeRefreshToken) payload.refresh_token = session.refreshToken;
  return payload;
}

/**
 * Reads the named string fields out of a JSON body, mirroring Go's
 * `c.BodyParser` for a struct of `string` fields: a body that is absent yields
 * `{ok: true, value: null}`, a body that will not decode or whose field is not
 * a string yields `{ok: false}`, and a missing field reads as the empty string.
 */
async function readFields(
  c: AppContext,
  keys: readonly string[],
): Promise<{ ok: true; value: Record<string, string> | null } | { ok: false }> {
  const raw = await c.req.text().catch(() => "");
  if (raw.trim() === "") return { ok: true, value: null };

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ok: false };
  }
  if (!isRecord(parsed)) return { ok: false };

  const fields: Record<string, string> = {};
  for (const key of keys) {
    const value = parsed[key];
    if (value === undefined || value === null) {
      fields[key] = "";
    } else if (typeof value === "string") {
      fields[key] = value;
    } else {
      return { ok: false };
    }
  }
  return { ok: true, value: fields };
}

/** Narrows a decoded JSON value to an object this handler can read fields from. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** The caller's address, preferring the proxy headers the Go server reads. */
function clientIp(c: AppContext): string {
  return (
    (c.req.header("CF-Connecting-IP") ?? "").trim() ||
    (c.req.header("X-Forwarded-For") ?? "").split(",")[0]?.trim() ||
    (c.req.header("X-Real-IP") ?? "").trim() ||
    ""
  );
}

/** Maps the configured SameSite string onto the casing the cookie writer needs. */
function sameSiteValue(value: string): "Lax" | "Strict" | "None" {
  switch (value.toLowerCase()) {
    case "strict":
      return "Strict";
    case "none":
      return "None";
    default:
      return "Lax";
  }
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
