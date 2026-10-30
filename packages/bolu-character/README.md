# `@timbolu/bolu-character`

Data karakter Bolu yang tidak menyentuh DOM — satu sumber untuk web dan app Expo.
Tanpa dependensi dan tanpa build; berkas `.ts` diimpor apa adanya.

## Yang sudah netral

- `palette.ts` — `BOLU_SWATCHES`, `hexToHsl`, `inkFor`, `feetColorFor`. Tidak ada JSX
  dan tidak ada impor React, jadi bisa dipanggil dari mana saja. Matematikanya disalin
  apa adanya dari port referensi; `palette.test.ts` mengunci nilainya.
- `names.ts` — `BoluShapeName`, `BoluExpressionName`, `BOLU_SHAPE_NAMES`,
  `BOLU_EXPRESSION_NAMES`. Urutannya bagian dari kontrak: picker berjalan mengikuti
  daftar ini.

## Yang belum netral

Geometri bodi dan wajah masih JSX di
`apps/frontend/src/components/bolu/characters.tsx` (`BOLU_SHAPES`, `BOLU_EXPRESSIONS`).
JSX tidak bisa dirender app, jadi sisa #302 adalah:

1. ganti JSX dengan model deskriptor elemen (path, circle, rect, transform, gaya), dan
2. tulis dua adapter di atasnya: SVG DOM untuk web, `react-native-svg` untuk app.

Sampai itu selesai, `characters.tsx` me-re-export isi paket ini supaya pemanggil lama
tidak berubah.

## Web

`apps/frontend/tsconfig.json` memetakan `@timbolu/bolu-character/*` ke
`../../packages/bolu-character/*`, mengikuti pola `@timbolu/contracts/*`. Image web
di-build dari root repo (`apps/frontend/Dockerfile.root`), jadi `packages/` ada di dalam
konteks build.

## Menjalankan test

```bash
node --test packages/bolu-character/palette.test.ts
```

Node 22.18+ menanggalkan tipe `.ts` sendiri, jadi tidak ada test runner atau dependensi yang
perlu dipasang.
