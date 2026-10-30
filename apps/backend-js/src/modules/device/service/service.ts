/**
 * The device registry's use cases: validating a registration and deciding what
 * a delete of somebody else's token means.
 *
 * It depends only on the module's domain contract, so the rules are testable
 * without a database. The rules are small on purpose. What a registration
 * carries is an address, and the only things worth refusing are a body that is
 * not a token and a platform the app does not run on.
 *
 * Ported 1:1 from internal/modules/device/service/service.go.
 */

import {
  ErrInvalidInput,
  PlatformAndroid,
  PlatformIOS,
  Platforms,
  TOKEN_MAX_LENGTH,
  isNilID,
  wrap,
  type Device,
  type RegisterRequest,
  type Repository,
  type Scope,
  type Service as ServicePort,
} from "../domain/device.ts";

/** The service dependencies. */
export interface Deps {
  repository: Repository;
}

/** The device registry's implementation. */
export class Service implements ServicePort {
  private readonly repository: Repository;

  constructor(deps: Deps) {
    this.repository = deps.repository;
  }

  /**
   * Validates a registration and stores it.
   *
   * The repository is an upsert, so registering the same token twice is one row
   * with a newer `last_seen_at`. That is not an optimisation: a phone
   * re-registers on every launch, and a registry that grew per launch would hand
   * the future sender the same address several times.
   */
  async register(scope: Scope, request: RegisterRequest): Promise<Device> {
    requireScope(scope);

    return this.repository.register(scope, normalise(request));
  }

  /**
   * Removes one of the caller's tokens.
   *
   * The token is passed through exactly as the client sent it. Trimming it here
   * would turn a different token into this one, and the delete is already the
   * narrowest possible statement: the caller's own rows only.
   */
  async remove(scope: Scope, token: string): Promise<void> {
    requireScope(scope);

    return this.repository.remove(scope, token);
  }
}

// ----------------------------------------------------------------- validation

/**
 * Refuses a call that names no workspace. The route is tenant-scoped, and a
 * device row carries the workspace it was registered from; answering without one
 * would be a write nobody asked for.
 */
function requireScope(scope: Scope): void {
  if (!scope.userId || isNilID(scope.workspaceId)) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
}

/**
 * Trims the fields a client controls and refuses the ones that are not an
 * address.
 *
 * The platform is lowercased rather than compared case-sensitively: "iOS" from a
 * client is the same platform as "ios", and refusing it would be a validation
 * error with no product meaning.
 */
function normalise(request: RegisterRequest): RegisterRequest {
  const token = request.token.trim();
  if (token === "") {
    throw wrap(ErrInvalidInput, "token perangkat belum diisi");
  }
  if (token.length > TOKEN_MAX_LENGTH) {
    throw wrap(ErrInvalidInput, `token perangkat lebih dari ${TOKEN_MAX_LENGTH} karakter`);
  }

  const platform = request.platform.trim().toLowerCase();
  if (!Platforms.includes(platform)) {
    throw wrap(ErrInvalidInput, `platform harus ${PlatformIOS} atau ${PlatformAndroid}`);
  }

  return {
    token,
    platform,
    appVersion: request.appVersion.trim(),
  };
}
