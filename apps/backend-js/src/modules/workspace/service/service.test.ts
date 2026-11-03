import { describe, expect, test } from "bun:test";

import { invitationLink } from "./service";

describe("invitationLink", () => {
  test("builds an absolute link the mail client can open", () => {
    expect(invitationLink("https://timbolu.com", "t0ken")).toBe("https://timbolu.com/join?token=t0ken");
  });

  test("strips a trailing slash instead of doubling it", () => {
    expect(invitationLink("https://timbolu.com/", "t0ken")).toBe("https://timbolu.com/join?token=t0ken");
  });

  test("refuses to build a relative link when the frontend URL is unset", () => {
    expect(() => invitationLink("", "t0ken")).toThrow();
  });
});
