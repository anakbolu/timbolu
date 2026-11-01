/**
 * Transactional email: address verification, password reset, invitations.
 *
 * Ported from apps/backend/internal/platform/mail/mail.go. Staging and
 * production use the SMTP sender; local development and the demo use the log
 * sender, so the flow works without an SMTP server.
 */

/** One transactional email. */
export interface Message {
  to: string;
  subject: string;
  text: string;
}

/** Delivers a message. The signal cancels a delivery in flight. */
export interface Sender {
  send(message: Message, signal?: AbortSignal): Promise<void>;
}

/**
 * The format-style logging function the Go side passes around. It receives a
 * `%s`/`%d`/`%q`/`%v` format and its arguments, exactly like `zap`'s SugaredLogger.
 */
export type LogFunc = (format: string, ...args: unknown[]) => void;

/** Writes the message to the log instead of sending it. */
export class LogSender implements Sender {
  private readonly log: LogFunc;

  constructor(logf: LogFunc | null = null) {
    this.log = logf ?? (() => {});
  }

  async send(message: Message, _signal?: AbortSignal): Promise<void> {
    validate(message);
    // Secrets like reset/verification tokens must never reach the log,
    // otherwise any log reader can take over accounts when MAIL_DRIVER=log.
    const safeBody = redactSecrets(message.text);
    this.log(
      formatLog("mail (not sent, log sender): to=%s subject=%q\n%s", message.to, message.subject, safeBody),
    );
  }
}

/** A recipient and a subject are the two things a message cannot lack. */
export function validate(message: Message): void {
  if (message.to.trim() === "") {
    throw new Error("mail: recipient is required");
  }
  if (message.subject.trim() === "") {
    throw new Error("mail: subject is required");
  }
}

/**
 * Applies the verbs the mail package uses in its log lines: %s, %d, %q, %v and
 * %%.
 */
export function formatLog(format: string, ...args: unknown[]): string {
  let index = 0;
  return format.replace(/%([sdqv%])/g, (_match, verb: string) => {
    if (verb === "%") return "%";
    const value = args[index++];
    switch (verb) {
      case "q":
        return JSON.stringify(value === undefined ? "" : String(value));
      case "d":
        return String(Number(value));
      case "v":
        return value instanceof Error ? value.message : String(value);
      default:
        return String(value);
    }
  });
}

/**
 * Query parameters and bearer tokens that must not be leaked to the log.
 * Redacts query params (?token=..., &code=..., etc.) and Authorization: Bearer tokens.
 */
const SECRET_QUERY_PARAM_REGEX = /([?&](?:token|code|key|access_token|refresh_token)=)[^&\s]+/gi;
const BEARER_TOKEN_REGEX = /(Bearer\s+)[^\s]+/gi;

/**
 * Redacts sensitive credentials from email text before logging.
 * Preserves structure and query parameter keys so debug traces remain useful.
 */
export function redactSecrets(text: string): string {
  return text
    .replace(SECRET_QUERY_PARAM_REGEX, "$1***")
    .replace(BEARER_TOKEN_REGEX, "$1***");
}
