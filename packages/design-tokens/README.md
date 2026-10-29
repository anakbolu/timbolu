# `@timbolu/design-tokens`

Token desain — warna, radius, tipografi, animasi — satu sumber untuk web dan app Expo.
`tokens.json` adalah turunan mesin dari kebenaran yang berlaku sekarang: blok `@theme` di
`apps/frontend/src/app/globals.css`. Tanpa dependensi dan tanpa build; file JSON dibaca apa adanya.

Bentuknya mengikuti nama variabel CSS-nya: `--color-bolu-bg` menjadi `color.bolu-bg`, `--radius-md`
menjadi `radius.md`, `--font-sans` menjadi `font.sans`, `--animate-soft-pulse` menjadi
`animate.soft-pulse`. Nilainya disalin persis dari CSS, termasuk `var(--color-bolu-surface)` dan
`rgb(30 27 46 / 0.4)` — jangan "dirapikan", karena pembandingnya membandingkan string.

## Web

Web tetap memakai `@theme` di `globals.css`, jadi utility Tailwind (`bg-bolu-surface`,
`text-bolu-ink`, `rounded-lg`, `font-display`) membaca variabel CSS yang sama. Sampai Tailwind
diarahkan ke JSON ini (tick terpisah), `globals.css` tetap tempat menulis token; perubahan di sana
wajib disalin ke `tokens.json` dalam commit yang sama.

## App

App tidak punya Tailwind. Nilainya dibaca dari `tokens.json` dan dipakai lewat `StyleSheet.create`:

```js
import tokens from "@timbolu/design-tokens/tokens";

const styles = StyleSheet.create({
  screen: { backgroundColor: tokens.color["bolu-bg"] },
  title: { color: tokens.color["bolu-ink"], fontSize: 17 },
});
```

Jangan menyalin hex dengan tangan ke app; kalau sebuah nilai belum ada di `tokens.json`, tambahkan
token-nya di CSS dulu.

## Kapan menjalankan pemeriksa

`make design-tokens-check` (juga bagian dari `make ci`) membandingkan `tokens.json` dengan blok
`@theme`: nama yang hanya ada di CSS, nama yang hanya ada di JSON, dan nilai yang berbeda. Jalankan
setiap kali token ditambah, dihapus, atau diubah nilainya; exit 1 menyebut token yang berselisih.

Override tema gelap (blok `.dark`) belum masuk JSON: namanya sama dengan token terang, jadi satu
entri JSON tidak bisa memuat dua nilai. Sampai app punya tema gelap, `.dark` tetap hanya di CSS.