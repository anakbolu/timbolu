import { describe, expect, test } from "bun:test";
import { formatLog, LogSender, redactSecrets, validate } from "./mail";

describe("redactSecrets", () => {
  test("redacts token query parameter in URLs", () => {
    const input = "Buka tautan: https://timbolu.com/forgot-password?token=abc123";
    const result = redactSecrets(input);
    expect(result).not.toContain("abc123");
    expect(result).toContain("token=***");
    expect(result).toBe("Buka tautan: https://timbolu.com/forgot-password?token=***");
  });

  test("redacts all specified query parameters (&code, &key, access_token, refresh_token)", () => {
    const input =
      "https://example.com/callback?code=secret_code_99&key=my_api_key&access_token=at_123&refresh_token=rt_456&keep=visible";
    const result = redactSecrets(input);
    expect(result).not.toContain("secret_code_99");
    expect(result).not.toContain("my_api_key");
    expect(result).not.toContain("at_123");
    expect(result).not.toContain("rt_456");
    expect(result).toBe(
      "https://example.com/callback?code=***&key=***&access_token=***&refresh_token=***&keep=visible",
    );
  });

  test("redacts Bearer tokens in headers or body", () => {
    const input = "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-ID";
    const result = redactSecrets(input);
    expect(result).not.toContain("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-ID");
    expect(result).toBe("Authorization: Bearer ***");
  });

  test("leaves text without secrets untouched", () => {
    const input = "Halo Bolu, selamat datang di workspace! Silakan cek dasbor di https://timbolu.com/dashboard.";
    expect(redactSecrets(input)).toBe(input);
  });
});

describe("LogSender", () => {
  test("redacts secrets in the logged output while preserving recipient, subject, and text body", async () => {
    const loggedLines: string[] = [];
    const sender = new LogSender((fmt, ...args) => {
      loggedLines.push(formatLog(fmt, ...args));
    });

    await sender.send({
      to: "user@example.com",
      subject: "Atur ulang kata sandi",
      text: "Klik tautan ini: https://timbolu.com/forgot-password?token=supersecret123\nHeader: Authorization: Bearer eyJtoken456",
    });

    expect(loggedLines).toHaveLength(1);
    const line = loggedLines[0];
    expect(line).toContain("to=user@example.com");
    expect(line).toContain('subject="Atur ulang kata sandi"');
    expect(line).toContain("token=***");
    expect(line).toContain("Bearer ***");
    expect(line).not.toContain("supersecret123");
    expect(line).not.toContain("eyJtoken456");
  });

  test("validates required recipient and subject", async () => {
    const sender = new LogSender(() => {});
    await expect(sender.send({ to: "", subject: "test", text: "body" })).rejects.toThrow(
      "mail: recipient is required",
    );
    await expect(sender.send({ to: "user@example.com", subject: "", text: "body" })).rejects.toThrow(
      "mail: subject is required",
    );
  });
});
