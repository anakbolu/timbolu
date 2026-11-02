// @ts-ignore
import { describe, it, expect } from "bun:test";
import { draftIdOf, draftIdsOf, messageText } from "./chat-message";
import type { MessageBlock } from "./api";

describe("messageText and draft blocks", () => {
  it("renders draft block as 'Draf menunggu persetujuan'", () => {
    const blocks: MessageBlock[] = [
      { type: "text", markdown: "Saya sudah siapkan draf untuk Anda periksa:" },
      { type: "draft", draft_id: "draft_123", title: "Kirim email invoice" },
    ];

    expect(messageText(blocks)).toBe(
      "Saya sudah siapkan draf untuk Anda periksa:\n\nDraf menunggu persetujuan",
    );
  });

  it("counts other non-text non-draft blocks without counting drafts", () => {
    const blocks: MessageBlock[] = [
      { type: "text", markdown: "Berikut ringkasannya:" },
      { type: "draft", draft_id: "draft_123" },
      { type: "table", columns: ["A", "B"], rows: [["1", "2"]] },
      { type: "chart", spec: { kind: "bar" } },
    ];

    expect(messageText(blocks)).toBe(
      "Berikut ringkasannya:\n\nDraf menunggu persetujuan\n\n(2 bagian lain belum bisa ditampilkan di app ini)",
    );
  });

  it("extracts draft ID with draftIdOf", () => {
    expect(draftIdOf({ type: "draft", draft_id: "draft_abc" })).toBe("draft_abc");
    expect(draftIdOf({ type: "draft", draft_id: "" })).toBeUndefined();
    expect(draftIdOf({ type: "text", markdown: "Halo" })).toBeUndefined();
    expect(draftIdOf({ type: "table" })).toBeUndefined();
  });

  it("extracts unique draft IDs with draftIdsOf", () => {
    const blocks: MessageBlock[] = [
      { type: "text", markdown: "Halo" },
      { type: "draft", draft_id: "draft_1" },
      { type: "draft", draft_id: "draft_2" },
      { type: "draft", draft_id: "draft_1" },
    ];

    expect(draftIdsOf(blocks)).toEqual(["draft_1", "draft_2"]);
  });
});
