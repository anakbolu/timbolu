/**
 * The bridges between the platform layer and each module's own ports.
 *
 * Every module declares the interfaces it needs (`domain/ports.ts`) rather than
 * importing a concrete platform type — that is the Go architecture rule, and it
 * is why the container has an adapters file. Each adapter below is one Go
 * `type xAdapter struct` from `apps/backend/internal/container/adapters.go`.
 */

import type { Sender as MailSender } from "../platform/mail/mail.ts";
import type { Limiter } from "../platform/ratelimit/ratelimit.ts";
import type { Codec as OAuthStateCodec } from "../platform/oauthstate/state.ts";
import type { Client as GoogleClient } from "../platform/oauth/google/google.ts";
import { generateToken, hashPassword, verifyPassword } from "../platform/authn/index.ts";

// --- auth module ports ------------------------------------------------------

export interface AuthMailer {
  send(to: string, subject: string, body: string): Promise<void>;
}

export function authMailer(sender: MailSender | null, from: string): AuthMailer {
  return {
    async send(to, subject, body) {
      if (!sender) throw new Error("mail is not configured");
      await sender.send({ to, subject, text: body });
    },
  };
}

export interface AuthStateSigner {
  sign(): Promise<string>;
  verify(state: string): Promise<void>;
}

/**
 * Signs the `state` of a Google sign-in. The platform codec signs a structured
 * OAuth state; the auth module's port only needs an opaque value it can verify
 * later, so the payload is minted here with no tenant (the sign-in happens before
 * a workspace is chosen).
 */
export function authStateSigner(codec: OAuthStateCodec | null, ttlMinutes: number): AuthStateSigner | null {
  if (!codec) return null;
  return {
    async sign() {
      return codec.sign({
        purpose: "oauth-state",
        workspaceId: "",
        userId: "",
        app: "google",
        nonce: crypto.randomUUID(),
        returnTo: "",
        expiresAt: Math.floor(Date.now() / 1000) + ttlMinutes * 60,
      } as any);
    },
    async verify(state) {
      codec.verify(state);
    },
  };
}

export interface AuthThrottle {
  allow(key: string, capacity: number, refill: number): Promise<{ allowed: boolean; retryAfter: number }>;
  reset(key: string): Promise<void>;
}

export function authThrottle(limiter: Limiter | null): AuthThrottle | null {
  if (!limiter) return null;
  return {
    async allow(key, capacity, refill) {
      const result = await limiter.allow(undefined, key, capacity, refill);
      return { allowed: result.allowed, retryAfter: result.retryAfter ?? 0 };
    },
    async reset(key) {
      await limiter.reset(undefined, key);
    },
  };
}

export interface AuthGoogle {
  authCodeURL(state: string): string;
  exchange(code: string): Promise<{ subject: string; email: string; emailVerified: boolean }>;
}

export function authGoogle(client: GoogleClient | null): AuthGoogle | null {
  if (!client) return null;
  return {
    authCodeURL(state) {
      return client.authCodeURL(state);
    },
    async exchange(code) {
      const info = await client.exchange(code);
      return { subject: info.sub, email: info.email, emailVerified: info.email_verified };
    },
  };
}

export interface AuthPasswords {
  hash(password: string): Promise<string>;
  verify(hash: string, password: string): Promise<boolean>;
  validate(password: string): Promise<void>;
}

/** The argon2id implementation, with the Go password policy. */
export const authPasswords: AuthPasswords = {
  hash: hashPassword,
  verify: verifyPassword,
  async validate(password) {
    // Mirrors the Go policy: a password must be long enough to be worth hashing.
    if (typeof password !== "string" || password.length < 8) {
      throw new Error("password must be at least 8 characters");
    }
  },
};

export interface AuthTokens {
  generate(): { raw: string; hash: string };
}

export const authTokens: AuthTokens = { generate: generateToken };

// --- workspace module ports -------------------------------------------------

export interface WorkspaceMailer {
  send(ctx: unknown, to: string, subject: string, body: string): Promise<void>;
}

export function workspaceMailer(sender: MailSender | null, from: string): WorkspaceMailer {
  return {
    async send(_ctx, to, subject, body) {
      if (!sender) throw new Error("mail is not configured");
      await sender.send({ to, subject, text: body });
    },
  };
}

export interface WorkspaceTokens {
  generate(): { raw: string; hash: string };
}

export const workspaceTokens: WorkspaceTokens = { generate: generateToken };

export interface WorkspacePlanLimits {
  maxMembers(ctx: unknown, workspaceId: string): Promise<number>;
}

/**
 * The member allowance. Until the billing module is wired this reports the plan's
 * default, which is what a workspace without a subscription is created under.
 */
export function workspacePlanLimits(maxMembers: number): WorkspacePlanLimits {
  return {
    async maxMembers() {
      return maxMembers;
    },
  };
}

// --- agent module ports -----------------------------------------------------

export interface AgentPlanLimits {
  maxAgents(workspaceId: string): Promise<number>;
}

export function agentPlanLimits(maxAgents: number): AgentPlanLimits {
  return {
    async maxAgents() {
      return maxAgents;
    },
  };
}
