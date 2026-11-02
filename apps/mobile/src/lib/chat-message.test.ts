// @ts-ignore
import { describe, it, expect } from "bun:test";
import { readMessage } from "./chat-message";

describe("readMessage", () => {
  it("keeps attachments when valid attachments exist", () => {
    const raw = {
      message_id: "msg_123",
      conversation_id: "conv_456",
      status: "complete",
      blocks: [{ type: "text", markdown: "Ini buktinya" }],
      attachments: [
        {
          id: "att_1",
          filename: "struk_transfer.png",
          content_type: "image/png",
          byte_size: 1048576,
          url: "https://example.com/files/struk.png",
        },
      ],
      created_at: "2026-11-02T10:00:00Z",
    };

    const message = readMessage(raw);
    expect(message).not.toBeNull();
    expect(message?.attachments).toEqual([
      {
        id: "att_1",
        filename: "struk_transfer.png",
        content_type: "image/png",
        byte_size: 1048576,
        url: "https://example.com/files/struk.png",
      },
    ]);
  });

  it("returns empty array for frame without attachments", () => {
    const raw = {
      message_id: "msg_124",
      conversation_id: "conv_456",
      status: "complete",
      blocks: [{ type: "text", markdown: "Pesan tanpa lampiran" }],
    };

    const message = readMessage(raw);
    expect(message).not.toBeNull();
    expect(message?.attachments).toEqual([]);
  });

  it("filters out broken attachments missing id, filename, or url, and applies safe defaults", () => {
    const raw = {
      message_id: "msg_125",
      conversation_id: "conv_456",
      status: "complete",
      blocks: [],
      attachments: [
        { filename: "no-id.pdf", url: "https://example.com/1.pdf" },
        { id: "", filename: "empty-id.pdf", url: "https://example.com/2.pdf" },
        { id: "att_valid", filename: "valid.pdf", url: "https://example.com/valid.pdf" },
        { id: "att_no_url", filename: "no-url.pdf" },
        { id: "att_no_filename", url: "https://example.com/no-name.pdf" },
        "string_item",
        null,
      ],
    };

    const message = readMessage(raw);
    expect(message).not.toBeNull();
    expect(message?.attachments).toEqual([
      {
        id: "att_valid",
        filename: "valid.pdf",
        content_type: "",
        byte_size: 0,
        url: "https://example.com/valid.pdf",
      },
    ]);
  });

  it("returns null for frame missing message_id or conversation_id or invalid status", () => {
    expect(
      readMessage({
        conversation_id: "conv_456",
        status: "complete",
        blocks: [],
      })
    ).toBeNull();

    expect(
      readMessage({
        message_id: "",
        conversation_id: "conv_456",
        status: "complete",
        blocks: [],
      })
    ).toBeNull();

    expect(
      readMessage({
        message_id: "msg_1",
        status: "complete",
        blocks: [],
      })
    ).toBeNull();

    expect(
      readMessage({
        message_id: "msg_1",
        conversation_id: "conv_1",
        status: "unknown_status",
        blocks: [],
      })
    ).toBeNull();
  });
});
