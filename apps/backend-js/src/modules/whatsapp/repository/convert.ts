/**
 * The conversions from the generated rows to the domain types.
 *
 * sqlc gives every query its own row type even when two queries select the same
 * columns, so each table is read through one shared field set. The nullable
 * columns are the interesting ones: a Bolu that has not been chosen, a draft a
 * message did not come from, and the media a message does not carry are all
 * "absent" in the domain rather than a zero value that reads as a real id.
 *
 * Ported 1:1 from internal/modules/whatsapp/repository/convert.go.
 */

import { NilUUID, type Inbound, type Number, type Outbox } from "../domain/whatsapp.ts";

/** The column set every number query returns. */
export interface NumberRow {
  id: string;
  workspace_id: string;
  integration_id: string | null;
  phone: string;
  label: string;
  agent_id: string | null;
  status: string;
  status_reason: string;
  tos_accepted_at: Date | null;
  hourly_limit: number;
  connected_at: Date | null;
  last_seen_at: Date | null;
  created_at: Date;
  updated_at: Date;
}

/** The column set every message query returns. */
export interface MessageRow {
  id: string;
  workspace_id: string;
  number_id: string;
  wa_message_id: string;
  chat_jid: string;
  sender_jid: string;
  sender_name: string;
  is_group: boolean;
  is_from_me: boolean;
  kind: string;
  text: string;
  media_key: string;
  media_kind: string;
  media_size: number;
  media_status: string;
  media_error: string;
  received_at: Date;
  task_id: string | null;
  created_at: Date;
}

/** The column set every outbox query returns. */
export interface OutboxRow {
  id: string;
  workspace_id: string;
  number_id: string;
  to_jid: string;
  kind: string;
  text: string;
  media_key: string;
  draft_id: string | null;
  status: string;
  attempts: number;
  last_error: string;
  not_before: Date;
  claimed_at: Date | null;
  wa_message_id: string;
  sent_at: Date | null;
  created_at: Date;
}

/** Renders one stored number. */
export function toNumber(row: NumberRow): Number {
  return {
    id: row.id,
    workspaceID: row.workspace_id,
    integrationID: row.integration_id ?? NilUUID,
    phone: row.phone,
    label: row.label,
    agentID: row.agent_id ?? NilUUID,
    status: row.status,
    statusReason: row.status_reason,
    tosAcceptedAt: row.tos_accepted_at,
    hourlyLimit: row.hourly_limit,
    connectedAt: row.connected_at,
    lastSeenAt: row.last_seen_at,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

/** Renders one stored message. */
export function toMessage(row: MessageRow): Inbound {
  return {
    id: row.id,
    workspaceID: row.workspace_id,
    numberID: row.number_id,
    waMessageID: row.wa_message_id,
    chatJID: row.chat_jid,
    senderJID: row.sender_jid,
    senderName: row.sender_name,
    isGroup: row.is_group,
    isFromMe: row.is_from_me,
    kind: row.kind,
    text: row.text,
    receivedAt: row.received_at,
    createdAt: row.created_at,
    taskID: row.task_id ?? NilUUID,
    media: {
      ref: "",
      kind: row.media_kind,
      size: row.media_size,
      status: row.media_status,
      key: row.media_key,
      error: row.media_error,
    },
  };
}

/** Renders one stored outbox item. */
export function toOutbox(row: OutboxRow): Outbox {
  return {
    id: row.id,
    workspaceID: row.workspace_id,
    numberID: row.number_id,
    toJID: row.to_jid,
    kind: row.kind,
    text: row.text,
    mediaKey: row.media_key,
    draftID: row.draft_id ?? NilUUID,
    status: row.status,
    attempts: row.attempts,
    lastError: row.last_error,
    notBefore: row.not_before,
    claimedAt: row.claimed_at,
    waMessageID: row.wa_message_id,
    sentAt: row.sent_at,
    createdAt: row.created_at,
  };
}

/**
 * Writes an id that may be absent. A zero uuid in the database would be a
 * reference to a row nobody has, so absent is null.
 */
export function optionalID(id: string): string | null {
  return !id || id === NilUUID ? null : id;
}

/** Writes a filter that may not narrow anything. */
export function optionalText(value: string): string | null {
  return value === "" ? null : value;
}
