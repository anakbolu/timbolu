/**
 * The two text rules of the module: what must never be stored, and what a
 * message is asking for.
 *
 * Ported 1:1 from internal/modules/memory/service/filter.go.
 */

import { KindFact, KindInstruction, KindPreference, LayerMain, LayerPrivate } from "../domain/memory.ts";
import { trimLeft, trimSpace } from "./chunk.ts";

/**
 * The patterns a memory must not carry. They are labels plus a value rather
 * than bare numbers, because a bare number is a price, an order number, or a
 * date as often as it is a secret — and a filter that refuses ordinary business
 * facts is a filter people turn off.
 */
// Go's `\s` is the ASCII set, so it is written out rather than left as `\s`,
// which in JavaScript also matches Unicode spaces.
const ASCII_SPACE = "\\t\\n\\f\\r ";

/** A password is recognised by its label: the value after it is what must not be stored. */
const passwordPattern = new RegExp(
  `\\b(?:kata[${ASCII_SPACE}]*sandi|password|passwd|pwd|pin)\\b[${ASCII_SPACE}]*(?::|=|-|>|adalah|ialah)?[${ASCII_SPACE}]*[^${ASCII_SPACE}]{4,}`,
  "i",
);

/** A one-time code is digits near the words that mean one. */
const otpPattern = new RegExp(
  `\\b(?:otp|kode[${ASCII_SPACE}]*otp|kode[${ASCII_SPACE}]*verifikasi|kode[${ASCII_SPACE}]*rahasia|kode[${ASCII_SPACE}]*autentikasi|token)\\b[^\\d\\n]{0,20}\\d{4,8}\\b`,
  "i",
);

/** An identity or account number carries its own label. */
const identityPattern = new RegExp(
  `\\b(?:nik|ktp|npwp|nomor[${ASCII_SPACE}]*identitas|nomor[${ASCII_SPACE}]*rekening|rekening|nomor[${ASCII_SPACE}]*kartu|cvv|cvc)\\b[^\\d\\n]{0,20}\\d{3,20}\\b`,
  "i",
);

/** A long digit run is a card candidate; the Luhn check below is what decides. */
const cardCandidate = /(?:\d[ -]?){12,18}\d/g;

/**
 * Reports whether text carries something that must not be remembered, with a
 * short reason in the language the user reads.
 *
 * The reason is the phrase the API returns and the chat confirmation names, so
 * it is a noun a person understands ("nomor kartu") and never the name of a
 * rule.
 *
 * The check runs before every write — a person asking for it, the extractor
 * proposing it, or a document being ingested — because a filter that only
 * guards one of those three doors is not a filter.
 */
export function sensitive(text: string): [string, boolean] {
  if (passwordPattern.test(text)) {
    return ["kata sandi", true];
  }
  if (otpPattern.test(text)) {
    return ["kode OTP", true];
  }
  if (identityPattern.test(text)) {
    return ["nomor identitas atau rekening", true];
  }

  for (const candidate of text.match(cardCandidate) ?? []) {
    const digits = candidate.replace(/\D/g, "");
    // Luhn holds for card numbers and for about one in ten arbitrary long
    // numbers, so it is what keeps an invoice number from being refused.
    if (digits.length >= 13 && digits.length <= 19 && luhn(digits)) {
      return ["nomor kartu", true];
    }
  }

  return ["", false];
}

/** The checksum every card number satisfies. */
function luhn(digits: string): boolean {
  let sum = 0;
  let double = false;

  for (let i = digits.length - 1; i >= 0; i--) {
    let value = digits.charCodeAt(i) - 48;
    if (double) {
      value *= 2;
      if (value > 9) {
        value -= 9;
      }
    }
    sum += value;
    double = !double;
  }

  return sum % 10 === 0;
}

/**
 * What a Bolu answers when something must not be remembered.
 *
 * It names the reason and says what to do about it, because a refusal that only
 * says "no" leaves a person guessing which part of their sentence was the
 * problem.
 */
export function sensitiveSentence(reason: string): string {
  return (
    "Aku tidak menyimpannya karena ada " +
    reason +
    " di dalamnya. " +
    "Tulis lagi tanpa bagian itu ya, supaya aman untuk kuingat."
  );
}

/**
 * The ways a person asks for something to be remembered, in the order they are
 * checked. The longest marker wins, so "tolong ingat ya" strips the whole phrase
 * rather than leaving "ya" behind.
 */
const rememberMarkers: string[] = [
  "mulai sekarang",
  "tolong ingat ya",
  "tolong ingat",
  "jangan lupa ya",
  "jangan lupa",
  "ingat ya",
  "ingatlah",
  "ingat dong",
  "ingat:",
  "ingat bahwa",
  "inget ya",
  "inget:",
  "catat ya",
  "catat:",
  "note:",
  "notes:",
  "perlu diingat",
];

/**
 * Reads a message that asks for something to be remembered.
 *
 * Detection is a pattern rather than a model call because it has to be free and
 * instant: it runs on every message, and a message that is not a request must
 * not spend a token. What it finds is the content — the words after the marker —
 * which is what gets stored and what the confirmation names.
 */
export function rememberIntent(text: string): [string, boolean] {
  const trimmed = trimSpace(text);
  if (trimmed === "") {
    return ["", false];
  }

  const lower = trimmed.toLowerCase();

  for (const marker of rememberMarkers) {
    if (!lower.startsWith(marker)) {
      continue;
    }

    let rest = trimmed.slice(marker.length);
    // The marker is usually followed by punctuation or a space, both of which
    // are part of the sentence rather than of the memory.
    rest = trimLeft(rest, " ,.:;-–—\t\n");
    rest = trimSpace(rest);

    if (rest === "") {
      // "ingat ya" on its own asks for nothing in particular. Answering it with
      // a confirmation of nothing would be worse than letting the ordinary
      // reply ask what to remember.
      return ["", false];
    }

    return [rest, true];
  }

  return ["", false];
}

/** Guesses what sort of memory this is. The marker settles it when one is present; otherwise the words decide, and the default is a fact. */
export function classifyKind(content: string): string {
  const lower = content.toLowerCase();

  if (containsAny(lower, "mulai sekarang", "selalu", "jangan", "usahakan", "ke depannya", "setiap kali")) {
    return KindInstruction;
  }
  if (containsAny(lower, "suka", "lebih baik", "prefer", "utamakan", "senang", "kesukaan")) {
    return KindPreference;
  }

  return KindFact;
}

/**
 * Decides which memory a direct request belongs to.
 *
 * Talking about the Bolu in the second person — "kamu", "anda" — is talking
 * about that Bolu's own way of working, which belongs to it alone. Anything else
 * is a fact about the workspace, and the main memory is what every call gets.
 */
export function classifyLayer(content: string): string {
  const padded = " " + content.toLowerCase() + " ";

  if (containsAny(padded, " kamu ", " anda ", " kau ", " kalian ")) {
    return LayerPrivate;
  }

  return LayerMain;
}

function containsAny(haystack: string, ...needles: string[]): boolean {
  for (const needle of needles) {
    if (haystack.includes(needle)) {
      return true;
    }
  }
  return false;
}
