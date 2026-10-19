/**
 * The health module contracts. Imports nothing but the platform types, so the
 * module can be tested without a database or an HTTP server.
 *
 * Ported from internal/modules/health/domain/health.go.
 */

/** Component states. */
export const StatusOK = "ok";
export const StatusUnavailable = "unavailable";
export const StatusNotConfigured = "not_configured";

/** Component is the health state of one dependency. */
export interface Component {
  name: string;
  status: string;
  /** Omitted when the dependency answered. */
  error?: string;
}

/** Report is the answer of a probe. */
export interface Report {
  status: string;
  /** Omitted by liveness, which never inspects a dependency. */
  components?: Component[];
}

/** Reports whether the database is reachable. Implemented by the Postgres repository. */
export interface Repository {
  ping(): Promise<void>;
}

/** The minimal contract of any optional dependency (Redis). */
export interface Checker {
  ping(): Promise<void>;
}

/** Service answers liveness and readiness probes. */
export interface Service {
  /** Liveness never touches a dependency: it answers "is this process alive". */
  liveness(): Report;
  /** Readiness checks every configured dependency. */
  readiness(): Promise<Report>;
}
