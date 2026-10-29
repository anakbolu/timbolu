/**
 * The refresh endpoint serves two kinds of client, and the rotated token has to
 * reach each through the only channel it can read. A browser reads the httpOnly
 * cookie, so the token stays out of the body. A native client has no cookie jar:
 * if the rotated token did not come back in the body it would be lost, and the
 * client could refresh exactly once (#363). These cases pin both shapes.
 */

import { describe, expect, test } from "bun:test";
import { Hono } from "hono";

import type { AppEnv } from "../../../platform/context.ts";
import { nopLogger } from "../../../platform/logger.ts";
import type { Service, Session, User } from "../domain/auth.ts";
import { CookieName, Handler, routes } from "./handler.ts";

const user: User = {
  id: "user-1",
  email: "owner@example.com",
  emailVerified: true,
  hasGoogle: false,
  hasPassword: true,
  onboarded: true,
  role: "user",
  createdAt: new Date(0),
};

/** The session every refresh returns, with a token that differs from the input. */
const rotated: Session = {
  accessToken: "access-rotated",
  accessTokenExp: new Date("2026-10-29T12:15:00Z"),
  refreshToken: "refresh-rotated",
  refreshTokenExp: new Date("2026-11-29T12:00:00Z"),
  user,
};

/** Records the token it was handed; every other method fails loudly. */
class FakeService implements Service {
  presented: string | null = null;

  async refresh(token: string): Promise<Session> {
    this.presented = token;
    return rotated;
  }

  register(): Promise<User> {
    throw new Error("register is not under test");
  }
  verifyEmail(): Promise<User> {
    throw new Error("verifyEmail is not under test");
  }
  resendVerification(): Promise<void> {
    throw new Error("resendVerification is not under test");
  }
  login(): Promise<Session> {
    throw new Error("login is not under test");
  }
  logout(): Promise<void> {
    throw new Error("logout is not under test");
  }
  requestPasswordReset(): Promise<void> {
    throw new Error("requestPasswordReset is not under test");
  }
  resetPassword(): Promise<void> {
    throw new Error("resetPassword is not under test");
  }
  googleAuthURL(): Promise<string> {
    throw new Error("googleAuthURL is not under test");
  }
  googleCallback(): Promise<Session> {
    throw new Error("googleCallback is not under test");
  }
  currentUser(): Promise<User> {
    throw new Error("currentUser is not under test");
  }
  authenticate(): Promise<User> {
    throw new Error("authenticate is not under test");
  }
  assertVerified(): Promise<void> {
    throw new Error("assertVerified is not under test");
  }
}

function newApp(service: Service): Hono<AppEnv> {
  const app = new Hono<AppEnv>();
  routes(
    app,
    new Handler(service, nopLogger, {
      cookie: { path: "/api/auth", domain: "", secure: false, sameSite: "lax" },
      frontendURL: "http://localhost:3000",
    }),
  );
  return app;
}

describe("POST /auth/refresh", () => {
  test("returns the rotated token in the body when the token came from the body", async () => {
    const service = new FakeService();
    const res = await newApp(service).request("/auth/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: "refresh-presented" }),
    });

    expect(res.status).toBe(200);
    expect(service.presented).toBe("refresh-presented");

    const body = (await res.json()) as { data: { access_token: string; refresh_token?: string } };
    expect(body.data.refresh_token).toBe("refresh-rotated");
    expect(body.data.refresh_token).not.toBe("refresh-presented");
    expect(body.data.access_token).toBe("access-rotated");
  });

  test("keeps the rotated token out of the body when the token came from the cookie", async () => {
    const service = new FakeService();
    const res = await newApp(service).request("/auth/refresh", {
      method: "POST",
      headers: { Cookie: `${CookieName}=refresh-presented` },
    });

    expect(res.status).toBe(200);
    expect(service.presented).toBe("refresh-presented");

    const body = (await res.json()) as { data: Record<string, unknown> };
    expect("refresh_token" in body.data).toBe(false);
    expect(body.data.access_token).toBe("access-rotated");

    const cookie = res.headers.get("set-cookie") ?? "";
    expect(cookie).toContain(`${CookieName}=refresh-rotated`);
    expect(cookie).toContain("HttpOnly");
  });
});
