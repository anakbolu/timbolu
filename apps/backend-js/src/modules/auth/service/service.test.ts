/**
 * Regression tests for the authentication login rate limiter (#713).
 *
 * Verifies that successful logins clear only the per-email throttle bucket,
 * leaving the per-IP bucket to drain exclusively via its configured refill rate.
 * This prevents password-spraying attacks where an attacker alternates between
 * guessing victim passwords and logging into an attacker-controlled account
 * to reset their IP throttle.
 */

import { describe, expect, test } from "bun:test";

import {
  ErrInvalidCredentials,
  ErrTooManyAttempts,
  isAuthError,
  type Repository,
  type SessionRecord,
  type User,
} from "../domain/auth.ts";
import type { LoginThrottle, Passwords, TokenIssuer, Tokens } from "../domain/ports.ts";
import { Service, type Config, type Deps } from "./service.ts";

/** In-memory bucket counter implementing LoginThrottle. */
class MemoryThrottle implements LoginThrottle {
  readonly counts = new Map<string, number>();
  readonly resets: string[] = [];

  async allow(
    key: string,
    capacity: number,
    _refill: number,
  ): Promise<{ allowed: boolean; retryAfter: number }> {
    const current = this.counts.get(key) ?? 0;
    if (current >= capacity) {
      return { allowed: false, retryAfter: 5000 };
    }
    this.counts.set(key, current + 1);
    return { allowed: true, retryAfter: 0 };
  }

  async reset(key: string): Promise<void> {
    this.resets.push(key);
    this.counts.delete(key);
  }
}

/** Minimal in-memory auth repository for login flow tests. */
class MemoryRepository implements Repository {
  private readonly usersByEmail = new Map<string, User>();
  private readonly passwordsByUserId = new Map<string, string>();
  private sessionCounter = 0;

  addUser(user: User, passwordHash: string): void {
    this.usersByEmail.set(user.email, user);
    this.passwordsByUserId.set(user.id, passwordHash);
  }

  async userByEmail(email: string): Promise<User> {
    const user = this.usersByEmail.get(email);
    if (!user) throw new Error("user not found");
    return user;
  }

  async passwordHash(userID: string): Promise<string> {
    return this.passwordsByUserId.get(userID) ?? "";
  }

  async createSession(session: {
    userId: string;
    refreshTokenHash: string;
    userAgent: string;
    ip: string;
    expiresAt: Date;
    rotatedFrom: string | null;
  }): Promise<SessionRecord> {
    this.sessionCounter++;
    return {
      id: `session-${this.sessionCounter}`,
      userId: session.userId,
      expiresAt: session.expiresAt,
      revokedAt: null,
    };
  }

  async createUser(): Promise<User> {
    throw new Error("not implemented");
  }
  async createGoogleUser(): Promise<User> {
    throw new Error("not implemented");
  }
  async userByID(): Promise<User> {
    throw new Error("not implemented");
  }
  async userByGoogleSubject(): Promise<User> {
    throw new Error("not implemented");
  }
  async linkGoogleSubject(): Promise<User> {
    throw new Error("not implemented");
  }
  async markEmailVerified(): Promise<User> {
    throw new Error("not implemented");
  }
  async updatePasswordHash(): Promise<User> {
    throw new Error("not implemented");
  }
  async markOnboarded(): Promise<User> {
    throw new Error("not implemented");
  }
  async sessionByTokenHash(): Promise<SessionRecord> {
    throw new Error("not implemented");
  }
  async sessionByID(): Promise<SessionRecord> {
    throw new Error("not implemented");
  }
  async revokeSession(): Promise<void> {
    throw new Error("not implemented");
  }
  async revokeAllUserSessions(): Promise<void> {
    throw new Error("not implemented");
  }
  async revokeSessionByTokenHash(): Promise<void> {
    throw new Error("not implemented");
  }
  async revokeSessionsForUser(): Promise<number> {
    throw new Error("not implemented");
  }
  async createEmailVerificationToken(): Promise<void> {
    throw new Error("not implemented");
  }
  async consumeEmailVerificationToken(): Promise<string> {
    throw new Error("not implemented");
  }
  async createPasswordResetToken(): Promise<void> {
    throw new Error("not implemented");
  }
  async consumePasswordResetToken(): Promise<string> {
    throw new Error("not implemented");
  }
}

function makeHarness(capacity: number = 5) {
  const repository = new MemoryRepository();
  const throttle = new MemoryThrottle();

  const dummyIssuer: TokenIssuer = {
    async issue(userID: string, sessionID: string) {
      return { token: `access-${userID}-${sessionID}`, expiresAt: new Date(Date.now() + 3600000) };
    },
    async verify() {
      throw new Error("not implemented");
    },
  };

  const dummyPasswords: Passwords = {
    async hash(p: string) {
      return `hash:${p}`;
    },
    async verify(hash: string, password: string) {
      return hash === `hash:${password}`;
    },
    async validate() {},
  };

  const dummyTokens: Tokens = {
    generate() {
      return { raw: "raw-token", hash: "hash-token" };
    },
  };

  const config: Config = {
    refreshTTL: 3600000,
    verificationTTL: 3600000,
    resetTTL: 3600000,
    frontendURL: "http://localhost:3000",
    loginThrottleCapacity: capacity,
    loginThrottleRefill: 0.2,
    requireEmailVerification: false,
  };

  const deps: Deps = {
    repository,
    issuer: dummyIssuer,
    states: null,
    mailer: null,
    throttle,
    google: null,
    passwords: dummyPasswords,
    tokens: dummyTokens,
    config,
  };

  const service = new Service(deps);
  return { service, repository, throttle };
}

describe("login throttling per-IP and per-email (#713)", () => {
  test("per-IP bucket is NOT reset on successful login, blocking password spraying", async () => {
    // Capacity 5 allows 5 total attempts across the same IP.
    const { service, repository, throttle } = makeHarness(5);
    const attackerIp = "198.51.100.1";

    // Setup an attacker account with valid credentials.
    const attackerUser: User = {
      id: "attacker-user-id",
      email: "attacker@example.com",
      emailVerified: true,
      hasGoogle: false,
      hasPassword: true,
      onboarded: true,
      role: "user",
      createdAt: new Date(),
    };
    repository.addUser(attackerUser, "hash:correct-attacker-password");

    // Also register victim accounts with known credentials.
    for (let i = 1; i <= 4; i++) {
      const victim: User = {
        id: `victim-user-${i}`,
        email: `victim${i}@example.com`,
        emailVerified: true,
        hasGoogle: false,
        hasPassword: true,
        onboarded: true,
        role: "user",
        createdAt: new Date(),
      };
      repository.addUser(victim, `hash:correct-victim-${i}-password`);
    }

    // 4 failed login attempts for 4 different victim accounts from the attacker's IP.
    for (let i = 1; i <= 4; i++) {
      await expect(
        service.login({
          email: `victim${i}@example.com`,
          password: "wrong-guess-password",
          userAgent: "test-agent",
          ip: attackerIp,
        }),
      ).rejects.toThrow(ErrInvalidCredentials);
    }

    // At this point, the IP bucket has spent 4 tokens out of 5 capacity.
    expect(throttle.counts.get(`auth:login:ip:${attackerIp}`)).toBe(4);

    // 5th attempt: 1 SUCCESSFUL login for the attacker's own account from the same IP.
    const session = await service.login({
      email: "attacker@example.com",
      password: "correct-attacker-password",
      userAgent: "test-agent",
      ip: attackerIp,
    });
    expect(session.user.id).toBe("attacker-user-id");

    // The IP bucket spent its 5th token and MUST NOT have been reset.
    expect(throttle.counts.get(`auth:login:ip:${attackerIp}`)).toBe(5);
    expect(throttle.resets).not.toContain(`auth:login:ip:${attackerIp}`);
    expect(throttle.resets).toContain("auth:login:email:attacker@example.com");

    // 6th attempt: Next attempt from this IP (even for another account) MUST fail with ErrTooManyAttempts.
    let caughtErr: unknown = null;
    try {
      await service.login({
        email: "victim5@example.com",
        password: "any-password",
        userAgent: "test-agent",
        ip: attackerIp,
      });
    } catch (err) {
      caughtErr = err;
    }
    expect(isAuthError(caughtErr, ErrTooManyAttempts)).toBe(true);
    expect((caughtErr as Error).message).toContain("auth: too many attempts");
  });

  test("per-email bucket IS reset after successful login so typos do not lock out user", async () => {
    // Capacity 5 allows 5 attempts.
    const { service, repository, throttle } = makeHarness(5);
    const ip = "203.0.113.42";
    const userEmail = "honest.user@example.com";

    const user: User = {
      id: "user-id-123",
      email: userEmail,
      emailVerified: true,
      hasGoogle: false,
      hasPassword: true,
      onboarded: true,
      role: "user",
      createdAt: new Date(),
    };
    repository.addUser(user, "hash:correct-password");

    // User makes 3 typo attempts.
    for (let i = 0; i < 3; i++) {
      await expect(
        service.login({
          email: userEmail,
          password: "typo-password",
          userAgent: "test-agent",
          ip,
        }),
      ).rejects.toThrow(ErrInvalidCredentials);
    }

    expect(throttle.counts.get(`auth:login:email:${userEmail}`)).toBe(3);

    // User enters correct password and signs in successfully.
    const session = await service.login({
      email: userEmail,
      password: "correct-password",
      userAgent: "test-agent",
      ip,
    });
    expect(session.user.id).toBe("user-id-123");

    // Per-email bucket was reset.
    expect(throttle.resets).toContain(`auth:login:email:${userEmail}`);
    expect(throttle.counts.get(`auth:login:email:${userEmail}`)).toBeUndefined();
  });
});
