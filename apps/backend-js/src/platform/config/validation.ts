/**
 * Required-field validation.
 *
 * Ported from packages/platform/config/validation.go. Only `Http`, `Database`,
 * and `JWT` are checked, and fields tagged `validate:"optional"` (or of type
 * bool) are skipped. The error text matches the Go side.
 */

import type { Config } from "./config.ts";

const OPTIONAL_HTTP: Record<string, true> = {
  OpsAddress: true,
  InternalAddress: true,
};

const OPTIONAL_DATABASE: Record<string, true> = {
  URL: true,
  MaxOpenConns: true,
  MaxIdleConns: true,
  ConnMaxLifetime: true,
};

const OPTIONAL_JWT: Record<string, true> = {
  ExpireHours: true,
};

/**
 * Checks that every required config field is populated.
 *
 * Go's reflection walks the struct in declaration order and reports the first
 * zero field; the same order is kept here.
 */
export function validate(c: Config): void {
  validateStruct("http", OPTIONAL_HTTP, [
    ["OpsAddress", c.http.opsAddress],
    ["InternalAddress", c.http.internalAddress],
    ["Address", c.http.address],
    ["ReadTimeout", c.http.readTimeout],
    ["WriteTimeout", c.http.writeTimeout],
    ["ApiPrefix", c.http.apiPrefix],
  ]);

  validateStruct("database", OPTIONAL_DATABASE, [
    ["URL", c.database.url],
    ["MaxOpenConns", c.database.maxOpenConns],
    ["MaxIdleConns", c.database.maxIdleConns],
    ["ConnMaxLifetime", c.database.connMaxLifetime],
  ]);

  validateStruct("jwt", OPTIONAL_JWT, [
    ["Secret", c.jwt.secret],
    ["ExpireHours", c.jwt.expireHours],
  ]);
}

/** Mirrors Go `validateStruct`: first zero non-optional field wins. */
function validateStruct(
  name: string,
  optional: Record<string, true>,
  fields: Array<[string, string | number]>,
): void {
  for (const [fieldName, value] of fields) {
    if (optional[fieldName]) continue;
    if (value === "" || value === 0) {
      throw new Error(`err ${name} config: field ${fieldName} is required`);
    }
  }
}
