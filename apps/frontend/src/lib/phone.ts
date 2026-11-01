/**
 * WhatsApp phone number normalization.
 *
 * The API rejects any phone number starting with '0' (apps/backend-js/src/modules/whatsapp/service/service.ts:
 * "tulis nomornya dengan kode negara, contoh 62812… bukan 0812…") because WhatsApp requires international
 * format. When users type national format ('0812…') or copy-paste international formatting with extra zeros
 * ('620812…' or '+62 812…'), normalizeWhatsAppPhone cleans and transforms it into the canonical format
 * starting with country code '62'.
 */

/**
 * Normalizes user-entered WhatsApp phone number for API submission.
 *
 * Rules:
 * - strips all non-digit characters
 * - converts leading '0' to '62' (e.g. 0812-3456-789 -> 628123456789)
 * - collapses leading '620' to '62' (e.g. 620812... -> 62812...)
 * - preserves existing numbers starting with '62'
 * - returns "" for empty or all-non-digit input
 */
export function normalizeWhatsAppPhone(input: string): string {
  const digits = input.replace(/\D/g, "");
  if (!digits) {
    return "";
  }
  if (digits.startsWith("0")) {
    return `62${digits.slice(1)}`;
  }
  if (digits.startsWith("620")) {
    return `62${digits.slice(3)}`;
  }
  return digits;
}
