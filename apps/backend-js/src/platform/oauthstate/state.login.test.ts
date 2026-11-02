import { describe, expect, test } from "bun:test";
import { Codec, InvalidStateError } from "./state.ts";

const TEST_SECRET = "a-very-long-secret-key-that-is-at-least-32-chars-long";

describe("Codec login state", () => {
  const codec = Codec.create(TEST_SECRET)!;

  test("signLogin -> verifyLogin returns matching app, nonce, and expiresAt", () => {
    const expiresAt = new Date(Math.floor(Date.now() / 1000) * 1000 + 60_000);
    const input = {
      app: "google",
      nonce: "test-nonce-12345",
      expiresAt,
    };

    const signed = codec.signLogin(input);
    const verified = codec.verifyLogin(signed);

    expect(verified.app).toBe(input.app);
    expect(verified.nonce).toBe(input.nonce);
    expect(verified.expiresAt.getTime()).toBe(input.expiresAt.getTime());
  });

  test("tampered character throws InvalidStateError", () => {
    const expiresAt = new Date(Date.now() + 60_000);
    const signed = codec.signLogin({
      app: "google",
      nonce: "nonce-tamper-check",
      expiresAt,
    });

    const cut = signed.indexOf(".");
    const firstChar = signed[0] === "a" ? "b" : "a";
    const tamperedPayload = firstChar + signed.slice(1);
    expect(() => codec.verifyLogin(tamperedPayload)).toThrow(InvalidStateError);

    const tamperedSig = signed.slice(0, cut + 1) + (signed[cut + 1] === "a" ? "b" : "a") + signed.slice(cut + 2);
    expect(() => codec.verifyLogin(tamperedSig)).toThrow(InvalidStateError);
  });

  test("expiresAt 1 second in the past throws InvalidStateError", () => {
    const pastExpiresAt = new Date(Date.now() - 1000);
    const signed = codec.signLogin({
      app: "google",
      nonce: "expired-nonce",
      expiresAt: pastExpiresAt,
    });

    expect(() => codec.verifyLogin(signed)).toThrow(InvalidStateError);
  });

  test("Codec.verify regular method rejects login state", () => {
    const expiresAt = new Date(Date.now() + 60_000);
    const signed = codec.signLogin({
      app: "google",
      nonce: "no-tenant-nonce",
      expiresAt,
    });

    expect(() => codec.verify(signed)).toThrow(InvalidStateError);
  });
});
