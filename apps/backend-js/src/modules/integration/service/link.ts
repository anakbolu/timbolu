/**
 * Linking is the other way an application becomes connected.
 *
 * `connect` stores what a provider authorised: an access token, a refresh
 * token, and the moment they expire. That is not every application. A linked
 * WhatsApp number is paired by a phone, and the session it produces lives with
 * the process that holds the connection open — so there is nothing for the
 * token columns to hold.
 *
 * Ported 1:1 from internal/modules/integration/service/link.go.
 */

import {
  ErrInvalidInput,
  validateLinkInput,
  wrap,
  type Integration,
  type LinkInput,
  type Scope,
} from "../domain/integration.ts";
import type { IntegrationService } from "./service.ts";

/** Records a connection whose credential this deployment holds itself. */
export async function linkConnection(
  service: IntegrationService,
  _ctx: unknown,
  scope: Scope,
  input: LinkInput,
): Promise<Integration> {
  const failure = validateLinkInput(input);
  if (failure) {
    throw wrap(ErrInvalidInput, failure.message);
  }

  // No cipher is needed and none is required: sealing an empty string to look
  // like a token would make the row read as a connection with a credential it
  // never had.
  const stored = await service.deps.repository.upsert(
    scope,
    {
      app: input.app,
      accountLabel: input.accountLabel,
      scopes: input.scopes,
      accessToken: "",
      refreshToken: "",
      expiresAt: null,
    },
    {
      accessTokenEncrypted: new Uint8Array(),
      refreshTokenEncrypted: new Uint8Array(),
      expiresAt: null,
      scopes: [],
    },
  );

  service.deps.logger.info({ app: stored.app, account: stored.accountLabel }, "integration linked");

  return stored;
}
