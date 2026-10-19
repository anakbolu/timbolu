/**
 * Application errors.
 *
 * The Go backend signals failures two ways: `fiber.NewError(status, message)`
 * for plain HTTP failures, and `response.Error(c, status, message, code, ...)`
 * when the client needs a machine-readable code. Both collapse into AppError
 * here, and the global error handler renders the envelope.
 */

export class AppError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: unknown;

  constructor(status: number, message: string, code = "http_error", details?: unknown) {
    super(message);
    this.name = "AppError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

/** 400 — the request body or query is not usable. */
export function badRequest(message: string, code = "bad_request", details?: unknown): AppError {
  return new AppError(400, message, code, details);
}

/** 401 — no identity, or an identity that no longer holds. */
export function unauthorized(message = "authentication required", code = "unauthorized"): AppError {
  return new AppError(401, message, code);
}

/** 403 — an identity that may not do this. */
export function forbidden(message = "forbidden", code = "forbidden"): AppError {
  return new AppError(403, message, code);
}

/** 404 — the record does not exist in this scope. */
export function notFound(message = "not found", code = "not_found"): AppError {
  return new AppError(404, message, code);
}

/** 409 — the request conflicts with the current state. */
export function conflict(message: string, code = "conflict"): AppError {
  return new AppError(409, message, code);
}

/** 422 — syntactically fine, semantically refused. */
export function unprocessable(message: string, code = "unprocessable", details?: unknown): AppError {
  return new AppError(422, message, code, details);
}

/** 429 — a bound was reached. */
export function tooManyRequests(message: string, code = "rate_limited"): AppError {
  return new AppError(429, message, code);
}

/** 503 — a dependency this deployment does not have. */
export function notConfigured(what: string, code = "not_configured"): AppError {
  return new AppError(503, `${what} is not configured in this deployment`, code);
}

/** 500 — nothing better to say. */
export function internal(message = "internal server error", code = "internal_error"): AppError {
  return new AppError(500, message, code);
}

/** True for an AppError, so the error handler can read its status. */
export function isAppError(err: unknown): err is AppError {
  return err instanceof AppError;
}
