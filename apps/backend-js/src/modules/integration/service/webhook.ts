/**
 * The inbound half: issuing a webhook URL, verifying a delivery, and the queue
 * it waits in.
 *
 * Ported 1:1 from internal/modules/integration/service/webhook.go.
 */

import { ErrInvalidInput, adopt, errorMessage, is, isNilId, wrap, type Scope } from "../domain/integration.ts";
import {
  ErrDeliveryExists,
  ErrNoWebhook,
  ErrSignatureInvalid,
  type QueuedEvent,
  type WebhookRef,
  type WebhookRequest,
} from "../domain/webhook.ts";
import type { TriggerEvent } from "../domain/oauth.ts";
import type { IntegrationService } from "./service.ts";

/**
 * Returns the address a provider should call for one connection.
 *
 * It is built from a signed reference rather than from the ids themselves: the
 * workspace is a UUID that appears in API responses, so an unsigned URL would
 * let a genuine delivery for one workspace be replayed against another.
 */
export async function webhookUrl(
  service: IntegrationService,
  _ctx: unknown,
  scope: Scope,
  integrationId: string,
  baseUrl: string,
): Promise<string> {
  if (isNilId(integrationId)) {
    throw wrap(ErrInvalidInput, "an integration id is required");
  }
  if (!service.deps.refs) {
    throw wrap(ErrInvalidInput, "no reference codec is configured");
  }

  const connection = await service.deps.repository.get(scope, integrationId);

  let value: string;
  try {
    value = await service.deps.refs.signRef({
      workspaceId: scope.workspaceId,
      integrationId,
      app: connection.app,
    });
  } catch (err) {
    throw new Error(`integration: sign webhook reference: ${errorMessage(err)}`);
  }

  return `${baseUrl.replace(/\/+$/, "")}/api/webhooks/${connection.app}?ref=${value}`;
}

/**
 * Verifies a delivery and queues it, reporting how many events were newly
 * accepted. A delivery that was already queued reports zero, which is a
 * success: telling the provider otherwise would make it retry forever.
 *
 * Two things are checked and both are needed. The signed reference proves the
 * URL was issued by this deployment for a specific connection, which is what
 * names the tenant without a session. The adapter's own verification proves the
 * request came from the provider, because the URL alone is only a capability.
 */
export async function receiveWebhook(
  service: IntegrationService,
  ctx: unknown,
  app: string,
  request: WebhookRequest,
): Promise<number> {
  const trimmed = app.trim();
  if (trimmed === "") {
    throw wrap(ErrInvalidInput, "an app is required");
  }
  if (!service.deps.refs || !service.deps.queue) {
    throw wrap(ErrInvalidInput, "webhooks are not configured");
  }

  let ref: WebhookRef;
  try {
    ref = await service.deps.refs.verifyRef(request.query["ref"] ?? "");
  } catch (err) {
    // A reference this deployment did not sign is the module's own sentinel, so
    // the endpoint answers 401 rather than 500.
    throw adopt(ErrSignatureInvalid, err);
  }
  // The URL names the app it was issued for, so a delivery for one application
  // cannot be replayed at another's endpoint.
  if (ref.app !== trimmed) {
    throw wrap(ErrSignatureInvalid, `the reference is for ${JSON.stringify(ref.app)}`);
  }

  const adapter = service.webhookAdapter(trimmed);
  if (!adapter) {
    throw wrap(ErrNoWebhook, JSON.stringify(trimmed));
  }

  const events = await adapter.verifyWebhook(ctx, request);
  if (events.length === 0) {
    // A genuine request that carries nothing to act on. It is not an error, and
    // the provider should not retry it.
    throw wrap(ErrInvalidInput, "the delivery carries no event");
  }

  // One delivery may carry several events. Each is deduplicated on its own key
  // rather than the batch colliding on one.
  let accepted = 0;
  // The event kinds this delivery actually added, deduplicated: one delivery
  // carrying the same kind twice starts the routines watching it once.
  const fired = new Set<string>();

  for (const [index, event] of events.entries()) {
    try {
      await service.deps.queue.enqueue({
        workspaceId: ref.workspaceId,
        integrationId: ref.integrationId,
        app: trimmed,
        deliveryId: deliveryId(trimmed, event, BigInt(request.receivedAt.getTime()) * 1_000_000n, index, events.length),
        event: event.event,
        payload: new TextEncoder().encode(event.payload),
      });
    } catch (err) {
      if (is(err, ErrDeliveryExists)) {
        // A retry. The delivery is already queued, so the answer is the same as
        // the first time: accepted, and the provider stops.
        service.deps.logger.info({ app: trimmed, event: event.event }, "a webhook delivery arrived twice");
        continue;
      }
      throw err;
    }
    accepted++;
    fired.add(event.event);
  }

  // The routines a workspace set up for "when this happens" are started here,
  // because this is the moment the event arrived.
  await startTriggers(service, ctx, { workspaceId: ref.workspaceId, userId: "", agentId: "", taskId: "" }, fired);

  return accepted;
}

/** Hands each newly queued event kind to the routines watching it. */
export async function startTriggers(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  events: Set<string>,
): Promise<void> {
  if (!service.deps.triggers) {
    return;
  }

  for (const event of events) {
    try {
      const started = await service.deps.triggers.fire(ctx, scope, event);
      if (started > 0) {
        service.deps.logger.info({ event, routines: started }, "a delivery started the routines watching it");
      }
    } catch (err) {
      service.deps.logger.warn({ event, err }, "could not start the routines watching a delivery");
    }
  }
}

/**
 * The key a delivery is deduplicated on.
 *
 * The provider's own id is what makes a retry harmless. A provider that sends
 * none leaves nothing to deduplicate on, so the arrival time stands in.
 */
function deliveryId(app: string, event: TriggerEvent, receivedAt: bigint, index: number, total: number): string {
  let id = event.eventId;
  if (id === "") {
    // Go formats `receivedAt.UnixNano()` as an int64; BigInt keeps every digit
    // a JavaScript number would round away.
    id = `${app}@${receivedAt}`;
  }
  if (total > 1) {
    // One delivery carrying several events must not collapse them onto one key.
    return `${id}#${index}`;
  }
  return id;
}

/** The oldest deliveries a workspace has not acted on. */
export async function pendingWebhooks(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  limit: number,
): Promise<QueuedEvent[]> {
  if (!service.deps.queue) {
    throw wrap(ErrInvalidInput, "webhooks are not configured");
  }
  return service.deps.queue.pending(scope, limit);
}

/**
 * Reads and claims the deliveries of one event kind. The claim happens in the
 * same step as the read: two routines watching one event must not both act on
 * one delivery.
 */
export async function claimPendingEvents(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  event: string,
  limit: number,
): Promise<QueuedEvent[]> {
  if (!service.deps.queue) {
    throw wrap(ErrInvalidInput, "webhooks are not configured");
  }
  if (event.trim() === "") {
    throw wrap(ErrInvalidInput, "an event name is required");
  }

  return service.deps.queue.claimEvents(scope, event.trim(), limit);
}

/** Marks a delivery as acted on. */
export async function webhookProcessed(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  id: string,
): Promise<void> {
  if (!service.deps.queue) {
    throw wrap(ErrInvalidInput, "webhooks are not configured");
  }
  return service.deps.queue.processed(scope, id);
}

/** Records an attempt that did not work. */
export async function webhookFailed(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  id: string,
  reason: string,
): Promise<void> {
  if (!service.deps.queue) {
    throw wrap(ErrInvalidInput, "webhooks are not configured");
  }
  return service.deps.queue.failed(scope, id, reason);
}
