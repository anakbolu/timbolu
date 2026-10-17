/**
 * Exposes a linked WhatsApp number as a tool the hub can run and an event a
 * routine can watch.
 *
 * It is an adapter in the hub's sense: the model sees tools with names and
 * schemas, and nothing downstream learns that WhatsApp is not Gmail. The sending
 * tool is labelled `write_external`, which is what puts every message through
 * the approval gate — a reply to a customer is an action that leaves the
 * workspace, and the gate is the whole reason that label exists.
 *
 * Ported 1:1 from internal/modules/whatsapp/adapter/adapter.go.
 */

import type { Adapter, Call, Result, ToolSpec, TriggerSpec } from "../../integration/domain/adapter.ts";
import { ErrSchemaViolation, ErrUnknownTool } from "../../integration/domain/adapter.ts";
import { LabelRead, LabelWriteExternal } from "../../integration/domain/integration.ts";
import {
  ErrInvalidInput,
  ErrNotConfigured,
  KindText,
  NilUUID,
  type Inbound,
  type Outbox,
  type Scope,
} from "../domain/whatsapp.ts";
import { EventMessageReceived, type EnqueueRequest } from "../domain/service.ts";

/** The application key this adapter answers for. */
export const App = "whatsapp";

/** A message to a customer. It is `write_external`: somebody decides first. */
export const ToolSend = "whatsapp.send_message";
/** Reads what a customer wrote earlier. */
export const ToolHistory = "whatsapp.read_messages";

/** The part of the module this adapter uses. */
export interface Numbers {
  /** Records one message to send. */
  enqueue(scope: Scope, request: EnqueueRequest): Promise<Outbox>;
  /** Returns what one conversation has said, oldest first. */
  history(scope: Scope, integrationID: string, chatJID: string, limit: number): Promise<Inbound[]>;
  /** Answers which number a connection speaks as. */
  numberByIntegration(scope: Scope, integrationID: string): Promise<string>;
}

/** The WhatsApp integration. */
export class WhatsAppAdapter implements Adapter {
  constructor(private readonly numbers: Numbers | null) {}

  /** The application key. */
  app(): string {
    return App;
  }

  /**
   * The credential a WhatsApp call needs is the session, and the session lives
   * in the gateway that holds the connection open. There is no token for the hub
   * to store.
   */
  needsToken(): boolean {
    return false;
  }

  /** What a Bolu can do with a number. */
  tools(): ToolSpec[] {
    return [
      {
        name: ToolSend,
        label: LabelWriteExternal,
        description:
          "Kirim pesan WhatsApp ke seorang pelanggan. Pesannya akan " +
          "menunggu persetujuan sebelum benar-benar terkirim.",
        schema: `{
				"type": "object",
				"properties": {
					"to": {"type": "string", "description": "Nomor tujuan dalam format internasional, contoh 628123456789"},
					"text": {"type": "string", "description": "Isi pesan yang akan dikirim"}
				},
				"required": ["to", "text"],
				"additionalProperties": false
			}`,
      },
      {
        name: ToolHistory,
        label: LabelRead,
        description: "Baca percakapan WhatsApp dengan satu nomor, dari yang paling awal.",
        schema: `{
				"type": "object",
				"properties": {
					"chat": {"type": "string", "description": "Nomor pelanggan, contoh 628123456789"},
					"limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "Berapa pesan terakhir yang dibaca"}
				},
				"required": ["chat"],
				"additionalProperties": false
			}`,
      },
    ];
  }

  /** A message arriving is the event a routine can watch. */
  triggers(): TriggerSpec[] {
    return [
      {
        app: App,
        kind: "webhook",
        event: EventMessageReceived,
        description: "Pesan WhatsApp masuk",
      },
    ];
  }

  /** Runs one tool call. */
  async execute(_ctx: unknown, call: Call): Promise<Result> {
    if (this.numbers === null) {
      throw new Error(`${ErrNotConfigured.message}: no whatsapp runtime is configured`);
    }

    const scope: Scope = {
      userId: call.scope.userId ?? null,
      workspaceId: call.scope.workspaceId,
    };

    switch (call.tool) {
      case ToolSend:
        return this.send(scope, call);
      case ToolHistory:
        return this.history(scope, call);
      default:
        throw new Error(`${ErrUnknownTool.message}: ${call.tool}`);
    }
  }

  /**
   * Queues one message.
   *
   * It queues rather than sends: the message goes out on the number's own pace,
   * with the typing indicator and the hourly ceiling applied, which is the
   * difference between answering a customer and blasting one.
   */
  private async send(scope: Scope, call: Call): Promise<Result> {
    let args: { to?: unknown; text?: unknown };
    try {
      args = JSON.parse(call.arguments) as { to?: unknown; text?: unknown };
    } catch (err) {
      throw new Error(`${ErrSchemaViolation.message}: ${(err as Error).message}`);
    }

    const numberID = await this.numberFor(scope, call.integrationId);

    const to = typeof args.to === "string" ? args.to.trim() : "";
    const text = typeof args.text === "string" ? args.text : "";

    if (to === "") throw new Error(`${ErrInvalidInput.message}: pesan tidak menyebut nomor tujuan`);
    if (text.trim() === "") throw new Error(`${ErrInvalidInput.message}: pesan kosong`);

    const queued = await (this.numbers as Numbers).enqueue(scope, {
      numberID,
      toJID: to,
      kind: KindText,
      text,
      mediaKey: "",
      draftID: NilUUID,
    });

    return {
      content: JSON.stringify({
        queued: true,
        message_id: queued.id,
        to,
        status: queued.status,
      }),
      summary: `Pesan WhatsApp masuk antrean untuk ${to}`,
    };
  }

  /** Reads one conversation. */
  private async history(scope: Scope, call: Call): Promise<Result> {
    let args: { chat?: unknown; limit?: unknown };
    try {
      args = JSON.parse(call.arguments) as { chat?: unknown; limit?: unknown };
    } catch (err) {
      throw new Error(`${ErrSchemaViolation.message}: ${(err as Error).message}`);
    }

    const chat = typeof args.chat === "string" ? args.chat.trim() : "";
    if (chat === "") {
      throw new Error(`${ErrInvalidInput.message}: percakapan mana yang dibaca belum disebut`);
    }

    let limit = typeof args.limit === "number" ? Math.trunc(args.limit) : 0;
    if (limit <= 0 || limit > 100) limit = 20;

    const messages = await (this.numbers as Numbers).history(scope, call.integrationId, chat, limit);

    const rendered = messages.map((message) => {
      const entry: Record<string, unknown> = {
        from: message.senderJID,
        at: message.receivedAt.toISOString().replace(/\.\d{3}Z$/, "Z"),
        kind: message.kind,
        ours: message.isFromMe,
        group: message.isGroup,
        text: message.text,
      };
      if (message.senderName !== "") entry.name = message.senderName;
      if (message.media.status !== "") {
        entry.media = { kind: message.media.kind, status: message.media.status };
      }
      return entry;
    });

    return {
      content: JSON.stringify({ chat, messages: rendered }),
      summary: `${rendered.length} pesan terakhir dengan ${chat}`,
    };
  }

  /**
   * Resolves which number a call speaks as.
   *
   * It is read from the connection the call names rather than from the
   * arguments: an approved action was approved for one connection, and letting
   * the model pick the number would let a reply to one customer go out of
   * another's line.
   */
  private async numberFor(scope: Scope, integrationID: string): Promise<string> {
    if (!integrationID || integrationID === NilUUID) {
      throw new Error(`${ErrInvalidInput.message}: aksi ini tidak menyebut sambungannya`);
    }

    return (this.numbers as Numbers).numberByIntegration(scope, integrationID);
  }
}
