/**
 * The auth data access with postgres.js.
 *
 * Ported from internal/modules/auth/repository/postgres.go and the sqlc queries
 * in repository/sqlcgen (auth.sql), folded in here: in TS the query is the
 * function, and each keeps its sqlc query name in a comment.
 *
 * The auth tables (users, sessions, one-time tokens) are not tenant-scoped and
 * carry no Row Level Security: access is controlled by knowing the token or the
 * credentials. Tenant isolation starts at members/workspaces.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import type {
  NewSession,
  Repository as RepositoryPort,
  SessionRecord,
  User,
} from "../domain/auth.ts";
import { ErrEmailTaken, ErrUserNotFound } from "../domain/auth.ts";

/** The PostgreSQL error code for a unique constraint breach. */
const UNIQUE_VIOLATION = "23505";

/** The row shape of `users`, as the sqlc queries select it. */
export interface UserRow {
  id: string;
  email: string;
  password_hash: string;
  email_verified_at: Date | null;
  google_subject: string | null;
  created_at: Date;
  updated_at: Date;
  onboarded_at: Date | null;
  role: string;
}

/** The row shape of `sessions`. */
export interface SessionRow {
  id: string;
  user_id: string;
  refresh_token_hash: string;
  rotated_from: string | null;
  user_agent: string;
  ip: string;
  expires_at: Date;
  revoked_at: Date | null;
  created_at: Date;
}

/** The row shape of `email_verification_tokens` and `password_reset_tokens`. */
export interface OneTimeTokenRow {
  id: string;
  user_id: string;
  token_hash: string;
  expires_at: Date;
  used_at: Date | null;
  created_at: Date;
}

/** Postgres-backed implementation of the domain repository. */
export class Repository implements RepositoryPort {
  constructor(private readonly db: Database) {}

  /** Stores a password account. */
  async createUser(email: string, passwordHash: string): Promise<User> {
    try {
      // name: CreateUser
      const rows = await this.db.sql<UserRow[]>`
        INSERT INTO users (email, password_hash)
        VALUES (${email}, ${passwordHash})
        RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
      `;
      return toUser(rows[0]!);
    } catch (err) {
      if (isUniqueViolation(err)) throw ErrEmailTaken;
      throw new Error(`auth: create user: ${errorMessage(err)}`);
    }
  }

  /** Stores a Google account, already verified. */
  async createGoogleUser(email: string, subject: string): Promise<User> {
    try {
      // name: CreateGoogleUser
      const rows = await this.db.sql<UserRow[]>`
        INSERT INTO users (email, google_subject, email_verified_at)
        VALUES (${email}, ${subject}, now())
        RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
      `;
      return toUser(rows[0]!);
    } catch (err) {
      if (isUniqueViolation(err)) throw ErrEmailTaken;
      throw new Error(`auth: create google user: ${errorMessage(err)}`);
    }
  }

  /** Looks up an account by address, case insensitively. */
  async userByEmail(email: string): Promise<User> {
    // name: GetUserByEmail
    const rows = await this.db.sql<UserRow[]>`
      SELECT id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role FROM users WHERE lower(email) = lower(${email})
    `;
    return toUser(one(rows, "user by email"));
  }

  /** Looks up an account by id. */
  async userByID(id: string): Promise<User> {
    // name: GetUserByID
    const rows = await this.db.sql<UserRow[]>`
      SELECT id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role FROM users WHERE id = ${id}
    `;
    return toUser(one(rows, "user by id"));
  }

  /** Looks up an account by its Google identity. */
  async userByGoogleSubject(subject: string): Promise<User> {
    // name: GetUserByGoogleSubject
    const rows = await this.db.sql<UserRow[]>`
      SELECT id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role FROM users WHERE google_subject = ${subject}
    `;
    return toUser(one(rows, "user by google subject"));
  }

  /** Attaches a Google identity to an existing account. */
  async linkGoogleSubject(userID: string, subject: string): Promise<User> {
    try {
      // name: LinkGoogleSubject
      const rows = await this.db.sql<UserRow[]>`
        UPDATE users
        SET google_subject = ${subject},
            email_verified_at = COALESCE(email_verified_at, now()),
            updated_at = now()
        WHERE id = ${userID}
        RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
      `;
      return toUser(one(rows, "link google subject"));
    } catch (err) {
      if (isUniqueViolation(err)) throw ErrEmailTaken;
      throw new Error(`auth: link google subject: ${errorMessage(err)}`);
    }
  }

  /** Records that the address was confirmed. */
  async markEmailVerified(userID: string): Promise<User> {
    // name: MarkEmailVerified
    const rows = await this.db.sql<UserRow[]>`
      UPDATE users
      SET email_verified_at = COALESCE(email_verified_at, now()),
          updated_at = now()
      WHERE id = ${userID}
      RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
    `;
    return toUser(one(rows, "mark email verified"));
  }

  /** Replaces the stored password hash. */
  async updatePasswordHash(userID: string, hash: string): Promise<User> {
    // name: UpdatePasswordHash
    const rows = await this.db.sql<UserRow[]>`
      UPDATE users
      SET password_hash = ${hash}, updated_at = now()
      WHERE id = ${userID}
      RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
    `;
    return toUser(one(rows, "update password hash"));
  }

  /** Records that the account finished onboarding. */
  async markOnboarded(userID: string): Promise<User> {
    // name: MarkOnboarded
    const rows = await this.db.sql<UserRow[]>`
      UPDATE users
      SET onboarded_at = COALESCE(onboarded_at, now()), updated_at = now()
      WHERE id = ${userID}
      RETURNING id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role
    `;
    return toUser(one(rows, "mark onboarded"));
  }

  /**
   * Records the same fact inside a transaction the caller owns, which is how
   * onboarding writes it together with the workspace it just created. A crash
   * between the two would otherwise leave an account that owns a workspace its
   * session does not believe in.
   */
  async markOnboardedInTx(tx: Tx, userID: string): Promise<void> {
    if (!tx) throw new Error("auth: marking onboarded needs a transaction");

    // name: MarkOnboarded (in the caller's transaction)
    const rows = await tx<Pick<UserRow, "id">[]>`
      UPDATE users
      SET onboarded_at = COALESCE(onboarded_at, now()), updated_at = now()
      WHERE id = ${userID}
      RETURNING id
    `;
    if (rows.length === 0) throw ErrUserNotFound;
  }

  /** Returns the stored hash, or an empty string for a Google-only account. */
  async passwordHash(userID: string): Promise<string> {
    // name: GetUserByID (PasswordHash)
    const rows = await this.db.sql<Pick<UserRow, "password_hash">[]>`
      SELECT id, email, password_hash, email_verified_at, google_subject, created_at, updated_at, onboarded_at, role FROM users WHERE id = ${userID}
    `;
    return one(rows, "password hash").password_hash;
  }

  /** Stores a refresh session. */
  async createSession(session: NewSession): Promise<SessionRecord> {
    try {
      // name: CreateSession
      const rows = await this.db.sql<SessionRow[]>`
        INSERT INTO sessions (user_id, refresh_token_hash, user_agent, ip, expires_at, rotated_from)
        VALUES (${session.userId}, ${session.refreshTokenHash}, ${session.userAgent}, ${session.ip}, ${session.expiresAt}, ${session.rotatedFrom})
        RETURNING id, user_id, refresh_token_hash, rotated_from, user_agent, ip, expires_at, revoked_at, created_at
      `;
      return toSession(rows[0]!);
    } catch (err) {
      throw new Error(`auth: create session: ${errorMessage(err)}`);
    }
  }

  /** Finds a session by the hash of its refresh token. */
  async sessionByTokenHash(tokenHash: string): Promise<SessionRecord> {
    // name: GetSessionByTokenHash
    const rows = await this.db.sql<SessionRow[]>`
      SELECT id, user_id, refresh_token_hash, rotated_from, user_agent, ip, expires_at, revoked_at, created_at FROM sessions WHERE refresh_token_hash = ${tokenHash}
    `;
    return toSession(one(rows, "session by token hash"));
  }

  /** Finds a session by id, used to validate an access token. */
  async sessionByID(id: string): Promise<SessionRecord> {
    // name: GetSessionByID
    const rows = await this.db.sql<SessionRow[]>`
      SELECT id, user_id, refresh_token_hash, rotated_from, user_agent, ip, expires_at, revoked_at, created_at FROM sessions WHERE id = ${id}
    `;
    return toSession(one(rows, "session by id"));
  }

  /** Marks one session as revoked. */
  async revokeSession(id: string): Promise<void> {
    // name: RevokeSession
    try {
      await this.db.sql`
        UPDATE sessions
        SET revoked_at = now()
        WHERE id = ${id} AND revoked_at IS NULL
      `;
    } catch (err) {
      throw new Error(`auth: revoke session: ${errorMessage(err)}`);
    }
  }

  /** Revokes the session a refresh token belongs to. */
  async revokeSessionByTokenHash(tokenHash: string): Promise<void> {
    // name: RevokeSessionByTokenHash
    try {
      await this.db.sql`
        UPDATE sessions
        SET revoked_at = now()
        WHERE refresh_token_hash = ${tokenHash} AND revoked_at IS NULL
      `;
    } catch (err) {
      throw new Error(`auth: revoke session by token: ${errorMessage(err)}`);
    }
  }

  /** Revokes every live session of a user, used after a password reset. */
  async revokeSessionsForUser(userID: string): Promise<number> {
    // name: RevokeSessionsForUser
    try {
      const result = await this.db.sql`
        UPDATE sessions
        SET revoked_at = now()
        WHERE user_id = ${userID} AND revoked_at IS NULL
      `;
      return result.count;
    } catch (err) {
      throw new Error(`auth: revoke sessions for user: ${errorMessage(err)}`);
    }
  }

  /** Stores a one-time verification token hash. */
  async createEmailVerificationToken(userID: string, tokenHash: string, expiresAt: Date): Promise<void> {
    // name: CreateEmailVerificationToken
    try {
      await this.db.sql`
        INSERT INTO email_verification_tokens (user_id, token_hash, expires_at)
        VALUES (${userID}, ${tokenHash}, ${expiresAt})
      `;
    } catch (err) {
      throw new Error(`auth: create verification token: ${errorMessage(err)}`);
    }
  }

  /**
   * Marks the token used and returns its owner. A second call finds nothing,
   * which is what enforces single use.
   */
  async consumeEmailVerificationToken(tokenHash: string): Promise<string> {
    // name: ConsumeEmailVerificationToken
    const rows = await this.db.sql<OneTimeTokenRow[]>`
      UPDATE email_verification_tokens
      SET used_at = now()
      WHERE token_hash = ${tokenHash}
        AND used_at IS NULL
        AND expires_at > now()
      RETURNING id, user_id, token_hash, expires_at, used_at, created_at
    `;
    return one(rows, "consume verification token").user_id;
  }

  /** Stores a one-time reset token hash. */
  async createPasswordResetToken(userID: string, tokenHash: string, expiresAt: Date): Promise<void> {
    // name: CreatePasswordResetToken
    try {
      await this.db.sql`
        INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
        VALUES (${userID}, ${tokenHash}, ${expiresAt})
      `;
    } catch (err) {
      throw new Error(`auth: create reset token: ${errorMessage(err)}`);
    }
  }

  /** Marks the token used and returns its owner. */
  async consumePasswordResetToken(tokenHash: string): Promise<string> {
    // name: ConsumePasswordResetToken
    const rows = await this.db.sql<OneTimeTokenRow[]>`
      UPDATE password_reset_tokens
      SET used_at = now()
      WHERE token_hash = ${tokenHash}
        AND used_at IS NULL
        AND expires_at > now()
      RETURNING id, user_id, token_hash, expires_at, used_at, created_at
    `;
    return one(rows, "consume reset token").user_id;
  }
}

/** Maps a `users` row to the domain account. */
export function toUser(row: UserRow): User {
  return {
    id: row.id,
    email: row.email,
    emailVerified: row.email_verified_at !== null,
    hasGoogle: row.google_subject !== null && row.google_subject !== "",
    hasPassword: row.password_hash !== "",
    onboarded: row.onboarded_at !== null,
    role: row.role,
    createdAt: row.created_at,
  };
}

/** Maps a `sessions` row to the domain record. */
export function toSession(row: SessionRow): SessionRecord {
  return {
    id: row.id,
    userId: row.user_id,
    expiresAt: row.expires_at,
    revokedAt: row.revoked_at,
  };
}

/**
 * Turns "no rows" into the domain error the service expects, mirroring Go's
 * `mapNotFound`.
 */
function one<T>(rows: T[], operation: string): T {
  if (rows.length === 0) throw ErrUserNotFound;
  return rows[0]!;
}

/** True when the driver reported a unique constraint breach. */
function isUniqueViolation(err: unknown): boolean {
  return (err as { code?: string } | null)?.code === UNIQUE_VIOLATION;
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
