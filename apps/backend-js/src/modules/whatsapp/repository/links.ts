/**
 * The statements the link flow and the retry path need, kept apart from the
 * main file because each is written once in a number's life rather than on a
 * path that runs per message.
 *
 * Ported 1:1 from internal/modules/whatsapp/repository/links.go. Each function
 * is the statement alone; the repository opens the tenant transaction around it.
 */

import type { Tx } from "../../../platform/database/index.ts";
import { ErrNotFound, wrap, type Scope } from "../domain/whatsapp.ts";

/** name: SetWhatsAppNumberIntegration — the connection the hub created. */
export async function setNumberIntegration(
  tx: Tx,
  scope: Scope,
  id: string,
  integrationID: string,
): Promise<void> {
  const rows = await tx`
    UPDATE whatsapp_numbers
    SET integration_id = ${integrationID}, updated_at = now()
    WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
  `;
  if (rows.count === 0) throw wrap(ErrNotFound, "set integration");
}

/** name: SetWhatsAppNumberPhone — the number WhatsApp reported when it paired. */
export async function setNumberPhone(tx: Tx, scope: Scope, id: string, phone: string): Promise<void> {
  const rows = await tx`
    UPDATE whatsapp_numbers
    SET phone = ${phone}, updated_at = now()
    WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
  `;
  if (rows.count === 0) throw wrap(ErrNotFound, "set phone");
}

/** name: MarkWhatsAppOutboxRetry — back in the queue after a failed attempt. */
export async function markOutboxRetry(
  tx: Tx,
  scope: Scope,
  id: string,
  reason: string,
  at: Date,
): Promise<void> {
  await tx`
    UPDATE whatsapp_outbox
    SET status = 'queued', not_before = ${at}, last_error = ${reason},
        claimed_at = NULL
    WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
  `;
}
