/**
 * Daftar nama bentuk Bolu yang sah — salinan lokal dari
 * `packages/bolu-character/names.ts`.
 *
 * apps/backend-js tidak mengimpor paket @timbolu/bolu-character (#564),
 * sehingga daftar ini dipertahankan di sini. Jika paket berubah (nama
 * ditambah, diubah, atau dihapus), daftar ini dan DEFAULT_BOLU_SHAPE
 * harus diperbarui bersama-sama.
 *
 * Sumber kebenaran: packages/bolu-character/names.ts
 */

/** Setiap nama bentuk Bolu yang sah, sesuai urutan referensi. */
export const BOLU_SHAPE_NAMES: readonly string[] = [
  "kacang",
  "hantu",
  "gumpal",
  "awan",
  "daun",
  "percik",
  "bintang",
  "genangan",
  "melambai",
  "tetes",
  "mochi",
  "bulat",
  "kotak",
  "segitiga",
  "hati",
  "kucing",
] as const;

/** Bentuk yang dipakai saat tidak ada shape yang diminta. */
export const DEFAULT_BOLU_SHAPE = "kacang";

/** Set untuk pencarian O(1). */
const BOLU_SHAPE_SET: ReadonlySet<string> = new Set(BOLU_SHAPE_NAMES);

/** Mengembalikan true jika nama adalah bentuk Bolu yang sah. */
export function isValidBoluShape(name: string): boolean {
  return BOLU_SHAPE_SET.has(name);
}
