/**
 * The inbound half of the adapter contract: what a provider pushes at us, and
 * how we decide whether it is really from them.
 *
 * Ported 1:1 from internal/modules/integration/domain/webhook.go.
 */

import type { Adapter } from "./adapter.ts";
import type { TriggerEvent } from "./oauth.ts";
import type { Scope } from "./integration.ts";

/** The request did not carry a signature this deployment accepts. */
export const ErrSignatureInvalid = new Error("integration: the webhook signature is not valid");
/** An application that pushes nothing was called anyway. */
export const ErrNoWebhook = new Error("integration: this application does not send webhooks");
/** This delivery is already queued. */
export const ErrDeliveryExists = new Error("integration: this delivery is already queued");

/** An adapter whose application pushes events at us. */
export interface TriggerAdapter extends Adapter {
  verifyWebhook(ctx: unknown, request: WebhookRequest): Promise<TriggerEvent[]>;
}

/** What the endpoint hands the adapter. */
export interface WebhookRequest {
  app: string;
  /** The request's headers, lowercased. */
  headers: Record<string, string>;
  /** The raw bytes, which is what a signature is computed over. */
  body: Uint8Array;
  /** The request's query string. */
  query: Record<string, string>;
  /** When the request arrived, which a signature with a timestamp is checked against. */
  receivedAt: Date;
}

/** Reads a header, treating a missing one and an empty one the same way. */
export function header(request: WebhookRequest, name: string): string {
  return request.headers[name.toLowerCase()] ?? "";
}

/** The connection a webhook URL belongs to. */
export interface WebhookRef {
  workspaceId: string;
  integrationId: string;
  app: string;
}

/** Signs and verifies a webhook reference. */
export interface RefCodec {
  signRef(ref: WebhookRef): Promise<string>;
  verifyRef(value: string): Promise<WebhookRef>;
}

/** One delivery waiting to be acted on. */
export interface QueuedEvent {
  id: string;
  workspaceId: string;
  integrationId: string;
  app: string;
  deliveryId: string;
  event: string;
  payload: Uint8Array;
  receivedAt: Date;
  attempts: number;
}

/** A verified delivery on its way into the queue. */
export interface EnqueueInput {
  workspaceId: string;
  integrationId: string;
  app: string;
  deliveryId: string;
  event: string;
  payload: Uint8Array;
}

/** The durable list of deliveries that arrived and have not been acted on yet. */
export interface Queue {
  enqueue(input: EnqueueInput): Promise<QueuedEvent>;
  pending(scope: Scope, limit: number): Promise<QueuedEvent[]>;
  claimEvents(scope: Scope, event: string, limit: number): Promise<QueuedEvent[]>;
  processed(scope: Scope, id: string): Promise<void>;
  failed(scope: Scope, id: string, reason: string): Promise<void>;
}
