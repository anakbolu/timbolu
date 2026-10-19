/**
 * The SMTP sender.
 *
 * Ported from apps/backend/internal/platform/mail/smtp.go. The session is
 * driven by hand rather than through a library helper, because the helper has
 * no deadline and no cancellation: a server that accepts the connection and
 * then stops talking would hold the caller until the kernel gave up.
 */

import { connect as netConnect, type Socket } from "node:net";
import { connect as tlsConnect, type TLSSocket } from "node:tls";
import { validate, type Message, type Sender } from "./mail";

/** Bounds one delivery when the configuration names none. */
export const defaultSMTPTimeoutMs = 10_000;

/** The SMTP connection settings. */
export interface SMTPConfig {
  host: string;
  port: number;
  username: string;
  password: string;
  from: string;
  /** Bounds the dial and every read and write of the session. 0 takes the default. */
  timeoutMs: number;
}

/** Delivers messages through an SMTP server. */
export class SMTPSender implements Sender {
  private readonly cfg: SMTPConfig;

  private constructor(cfg: SMTPConfig) {
    this.cfg = cfg;
  }

  /** Host and From are required: a sender that silently drops mail is worse. */
  static create(cfg: SMTPConfig): SMTPSender {
    if (cfg.host.trim() === "") {
      throw new Error("mail: smtp host is required");
    }
    if (cfg.from.trim() === "") {
      throw new Error("mail: smtp from address is required");
    }
    return new SMTPSender({
      ...cfg,
      port: cfg.port === 0 ? 587 : cfg.port,
      timeoutMs: cfg.timeoutMs > 0 ? cfg.timeoutMs : defaultSMTPTimeoutMs,
    });
  }

  /** The bound this sender applies to one delivery. */
  timeout(): number {
    return this.cfg.timeoutMs;
  }

  async send(message: Message, signal?: AbortSignal): Promise<void> {
    validate(message);
    try {
      await this.deliver(message, signal);
    } catch (err) {
      throw new Error(`mail: send to ${message.to}: ${(err as Error).message}`);
    }
  }

  private async deliver(message: Message, signal?: AbortSignal): Promise<void> {
    const session = await SmtpSession.open(this.cfg, signal);
    try {
      await session.hello("localhost");

      if (session.extensions.has("STARTTLS")) {
        await session.startTls(this.cfg.host);
        await session.hello("localhost");
      }

      // A server that does not advertise AUTH is not asked for it.
      const auth = session.extensions.get("AUTH");
      if (this.cfg.username !== "" && auth !== undefined) {
        await session.authenticate(this.cfg.username, this.cfg.password, auth);
      }

      await session.command(`MAIL FROM:<${addressOf(this.cfg.from)}>`, [250]);
      await session.command(`RCPT TO:<${addressOf(message.to)}>`, [250, 251]);
      await session.command("DATA", [354]);
      await session.writeData(buildPayload(this.cfg.from, message));
      await session.command("QUIT", [221]);
    } finally {
      session.close();
    }
  }
}

/** A minimal line-oriented SMTP client bound to one connection. */
class SmtpSession {
  private socket: Socket | TLSSocket;
  private readonly deadline: number;
  private buffer = "";
  private pending: Array<{ resolve: (lines: string[]) => void; reject: (err: Error) => void }> = [];
  private failure: Error | null = null;
  /** The EHLO keywords, upper-cased, with their parameter line. */
  readonly extensions = new Map<string, string>();

  private constructor(socket: Socket, deadline: number) {
    this.socket = socket;
    this.deadline = deadline;
    this.attach(socket);
  }

  static async open(cfg: SMTPConfig, signal?: AbortSignal): Promise<SmtpSession> {
    const deadline = Date.now() + cfg.timeoutMs;
    if (signal?.aborted) {
      throw new Error("mail: delivery cancelled");
    }
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    const socket = netConnect({ host: cfg.host, port: cfg.port, timeout: cfg.timeoutMs });
    socket.once("connect", resolve);
    socket.once("error", reject);
    socket.once("timeout", () => reject(new Error("mail: dial timed out")));
    await promise;
    socket.removeAllListeners("error");
    socket.removeAllListeners("timeout");
    socket.removeAllListeners("connect");

    const session = new SmtpSession(socket, deadline);
    // Cancellation closes the connection out from under the session, which
    // fails the pending read at once.
    signal?.addEventListener("abort", () => session.close(), { once: true });

    const greeting = await session.read();
    if (!greeting.startsWith("220")) {
      session.close();
      throw new Error(`mail: unexpected greeting: ${greeting}`);
    }
    return session;
  }

  /** Sends EHLO and records the advertised extensions. */
  async hello(name: string): Promise<void> {
    this.extensions.clear();
    const lines = await this.commandMulti(`EHLO ${name}`, [250]);
    for (const line of lines.slice(1)) {
      const keyword = line.split(/\s+/)[0]?.toUpperCase() ?? "";
      if (keyword !== "") {
        this.extensions.set(keyword, line);
      }
    }
  }

  async startTls(host: string): Promise<void> {
    await this.command("STARTTLS", [220]);
    const plain = this.socket as Socket;
    plain.removeAllListeners("data");
    plain.removeAllListeners("error");
    plain.removeAllListeners("close");

    const secured = tlsConnect({ socket: plain, servername: host });
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    secured.once("secureConnect", resolve);
    secured.once("error", reject);
    await promise;
    secured.removeAllListeners("error");

    // One live socket at a time: the session continues on the TLS socket.
    this.socket = secured;
    this.attach(secured);
  }

  private attach(socket: Socket | TLSSocket): void {
    socket.setEncoding("utf8");
    socket.on("data", (chunk: string) => {
      this.buffer += chunk;
      this.drain();
    });
    socket.on("error", (err) => this.fail(err));
    socket.on("close", () => this.fail(new Error("mail: connection closed")));
    socket.setTimeout(Math.max(0, this.deadline - Date.now()), () => {
      this.fail(new Error("mail: session timed out"));
      socket.destroy();
    });
  }

  /** AUTH PLAIN when offered, AUTH LOGIN otherwise. */
  async authenticate(username: string, password: string, advertised: string): Promise<void> {
    if (/\bPLAIN\b/i.test(advertised) || !/\bLOGIN\b/i.test(advertised)) {
      const token = Buffer.from(`\0${username}\0${password}`, "utf8").toString("base64");
      await this.command(`AUTH PLAIN ${token}`, [235]);
      return;
    }

    await this.command("AUTH LOGIN", [334]);
    await this.command(Buffer.from(username, "utf8").toString("base64"), [334]);
    await this.command(Buffer.from(password, "utf8").toString("base64"), [235]);
  }

  async writeData(payload: string): Promise<void> {
    // Dot-stuffing and the terminating dot: a line that is a single "." would
    // otherwise end the message early.
    const body = payload
      .replace(/\r\n/g, "\n")
      .split("\n")
      .map((line) => (line.startsWith(".") ? `.${line}` : line))
      .join("\r\n");
    this.socket.write(`${body}\r\n.\r\n`);
    const reply = await this.read();
    if (!reply.startsWith("250")) {
      throw new Error(`mail: data rejected: ${reply}`);
    }
  }

  /** Sends one command and requires one of the accepted reply codes. */
  async command(line: string, accepted: number[]): Promise<string> {
    this.socket.write(`${line}\r\n`);
    const reply = await this.read();
    if (!accepted.some((code) => reply.startsWith(String(code)))) {
      throw new Error(`mail: ${line.split(" ")[0]} rejected: ${reply}`);
    }
    return reply;
  }

  private async commandMulti(line: string, accepted: number[]): Promise<string[]> {
    this.socket.write(`${line}\r\n`);
    const lines = await this.readMulti();
    const last = lines[lines.length - 1] ?? "";
    if (!accepted.some((code) => last.startsWith(String(code)))) {
      throw new Error(`mail: ${line.split(" ")[0]} rejected: ${last}`);
    }
    return lines;
  }

  private async read(): Promise<string> {
    const lines = await this.readMulti();
    return lines[lines.length - 1] ?? "";
  }

  private readMulti(): Promise<string[]> {
    if (this.failure) {
      return Promise.reject(this.failure);
    }
    const { promise, resolve, reject } = Promise.withResolvers<string[]>();
    this.pending.push({ resolve, reject });
    this.drain();
    return promise;
  }

  /** Resolves the queued readers whose replies have fully arrived. */
  private drain(): void {
    while (this.pending.length > 0) {
      const parsed = parseReply(this.buffer);
      if (parsed === null) return;
      this.buffer = this.buffer.slice(parsed.consumed);
      const waiter = this.pending.shift();
      waiter?.resolve(parsed.lines);
    }
  }

  private fail(err: Error): void {
    if (this.failure) return;
    this.failure = err;
    const waiters = this.pending;
    this.pending = [];
    for (const waiter of waiters) waiter.reject(err);
  }

  close(): void {
    this.socket.end();
    this.socket.destroy();
  }
}

/**
 * Reads one complete SMTP reply from the front of the buffer: the final line is
 * the one whose fourth character is not "-".
 */
function parseReply(buffer: string): { lines: string[]; consumed: number } | null {
  const lines: string[] = [];
  let offset = 0;

  for (;;) {
    const end = buffer.indexOf("\r\n", offset);
    if (end < 0) return null;
    const line = buffer.slice(offset, end);
    offset = end + 2;
    lines.push(line);
    if (line.length < 4 || line[3] !== "-") {
      return { lines, consumed: offset };
    }
  }
}

/** Writes a minimal RFC 5322 message with UTF-8 headers. */
export function buildPayload(from: string, message: Message): string {
  return (
    `From: ${from}\r\n` +
    `To: ${message.to}\r\n` +
    `Subject: ${message.subject}\r\n` +
    `MIME-Version: 1.0\r\n` +
    `Content-Type: text/plain; charset=UTF-8\r\n` +
    `\r\n` +
    message.text
  );
}

/** The bare address inside a `Name <address>` form. */
function addressOf(value: string): string {
  const match = /<([^>]+)>/.exec(value);
  return (match?.[1] ?? value).trim();
}
