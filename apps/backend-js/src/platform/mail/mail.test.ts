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
    const rawToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig123";
    const input = `Authorization: Bearer ${rawToken}`;
    const result = redactSecrets(input);
    expect(result).not.toContain(rawToken);
    expect(result).toContain("Bearer ***");
    expect(result).toBe("Authorization: Bearer ***");
  });

  test("leaves text without secrets untouched", () => {
    const input = "Halo Bolu, selamat datang di workspace! Silakan cek dasbor di https://timbolu.com/dashboard.";
    expect(redactSecrets(input)).toBe(input);
  });

  test("redacts sensitive parameters across delimiters (#, ?, &, ;) and html attributes", () => {
    const input =
      '<p>Klik <a href="https://timbolu.com/verify#token=hash_token_789">verifikasi</a> atau https://example.com/api?signature=sig123&password=pass456&secret=sec789&apikey=ak1&api_key=ak2&credential=cred3;token=tok999</p>';
    const result = redactSecrets(input);
    expect(result).not.toContain("hash_token_789");
    expect(result).not.toContain("sig123");
    expect(result).not.toContain("pass456");
    expect(result).not.toContain("sec789");
    expect(result).not.toContain("ak1");
    expect(result).not.toContain("ak2");
    expect(result).not.toContain("cred3");
    expect(result).not.toContain("tok999");
    expect(result).toBe(
      '<p>Klik <a href="https://timbolu.com/verify#token=***">verifikasi</a> atau https://example.com/api?signature=***&password=***&secret=***&apikey=***&api_key=***&credential=***;token=***</p>',
    );
  });
});

describe("LogSender", () => {
  test("redacts secrets in the logged output while preserving recipient, subject, and text body", async () => {
    const loggedLines: string[] = [];
    const sender = new LogSender((fmt, ...args) => {
      loggedLines.push(formatLog(fmt, ...args));
    });

    const rawBearer = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig456";
    const rawToken = "supersecret123";

    await sender.send({
      to: "user@example.com",
      subject: "Atur ulang kata sandi",
      text: `Klik tautan ini: https://timbolu.com/forgot-password?token=${rawToken}\nHeader: Authorization: Bearer ${rawBearer}`,
    });

    expect(loggedLines).toHaveLength(1);
    const line = loggedLines[0];
    expect(line).toContain("to=user@example.com");
    expect(line).toContain('subject="Atur ulang kata sandi"');
    expect(line).toContain("token=***");
    expect(line).toContain("Bearer ***");
    expect(line).not.toContain(rawToken);
    expect(line).not.toContain(rawBearer);
  });

  test("redacts secrets in recipient and subject when passed to LogSender", async () => {
    const loggedLines: string[] = [];
    const sender = new LogSender((fmt, ...args) => {
      loggedLines.push(formatLog(fmt, ...args));
    });

    const secretInTo = "secret_recipient_key_111";
    const secretInSubject = "secret_subject_sig_222";

    await sender.send({
      to: `recipient@example.com?key=${secretInTo}`,
      subject: `Konfirmasi Akun?signature=${secretInSubject}`,
      text: '<a href="https://timbolu.com/invite#token=invite_hash_333">Terima Undangan</a>',
    });

    expect(loggedLines).toHaveLength(1);
    const line = loggedLines[0];
    expect(line).toContain("to=recipient@example.com?key=***");
    expect(line).toContain('subject="Konfirmasi Akun?signature=***"');
    expect(line).toContain('href="https://timbolu.com/invite#token=***"');
    expect(line).not.toContain(secretInTo);
    expect(line).not.toContain(secretInSubject);
    expect(line).not.toContain("invite_hash_333");
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
