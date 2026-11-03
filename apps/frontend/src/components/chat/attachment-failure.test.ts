import assert from "node:assert/strict";
import test from "node:test";

import type { ApiError } from "@/lib/api";

import { blamedAttachmentIds } from "./attachment-failure";

test("marks only the id invalid_attachment_id names", () => {
  const error: ApiError = {
    ok: false,
    status: 400,
    code: "invalid_attachment_id",
    message: 'attachment "att_123" is not valid',
  };
  assert.deepEqual(blamedAttachmentIds(error), ["att_123"]);
});

test("marks no chip when invalid_attachment_id names no id", () => {
  const error: ApiError = {
    ok: false,
    status: 400,
    code: "invalid_attachment_id",
    message: "one of the attachments is not valid",
  };
  assert.deepEqual(blamedAttachmentIds(error), []);
});

test("marks no chip when an attachment is missing, since the API names none", () => {
  const error: ApiError = {
    ok: false,
    status: 404,
    code: "attachment_not_found",
    message: "attachment could not be found",
  };
  assert.deepEqual(blamedAttachmentIds(error), []);
});

test("leaves every chip uploaded when the refusal is not about a file", () => {
  const refused = (status: number, code: string): ApiError => ({
    ok: false,
    status,
    code,
    message: `the request was refused with "${code}"`,
  });

  assert.deepEqual(blamedAttachmentIds(refused(402, "quota_exceeded")), []);
  assert.deepEqual(blamedAttachmentIds(refused(403, "read_only")), []);
  assert.deepEqual(blamedAttachmentIds(refused(500, "internal_error")), []);
  assert.deepEqual(blamedAttachmentIds(refused(429, "too_many_requests")), []);
});
