# `@timbolu/bolu-character`

Karakter Bolu — 16 bentuk tubuh, 10 ekspresi, palet, dan kakinya — dalam bentuk
data murni plus dua adapter. Satu sumber untuk web dan app Expo. Tanpa
dependensi dan tanpa build; berkas `.ts`/`.tsx` diimpor apa adanya.

## Isi paket

- `names.ts` — `BoluShapeName`, `BoluExpressionName`, `BOLU_SHAPE_NAMES`,
  `BOLU_EXPRESSION_NAMES`. Urutannya bagian dari kontrak: picker berjalan
  mengikuti daftar ini.
- `palette.ts` — `BOLU_SWATCHES`, `hexToHsl`, `inkFor`, `feetColorFor`.
- `characters.ts` — **core**. `BOLU_SHAPES`, `BOLU_EXPRESSIONS`, `footPath()`,
  dan `charSVG()`. `charSVG()` mengembalikan `BoluDescription`: deskripsi
  lengkap satu karakter — `viewBox` 200×200, dimensi, warna tinta, kelas
  animasi, kaki, bodi, wajah, clip path, dan gradien pipi. Setiap elemen
  (path, circle, ellipse, rect, polygon, group, transform, paint) sudah
  membawa koordinatnya sendiri.
- `dom.tsx` — **adapter web**. `BoluCharacter` memetakan `BoluDescription`
  menjadi SVG DOM. Tidak ada geometri yang dihitung ulang di sini.

## Aturan: core tanpa React

`names.ts`, `palette.ts`, dan `characters.ts` tidak boleh mengimpor React,
menulis JSX, atau menyentuh DOM. Itu yang membuat app Expo bisa memakai
`charSVG()` tanpa React DOM. `characters.test.ts` membaca ketiga berkas itu dan
gagal kalau menemukan impor React atau tag JSX.

Adapter berikutnya (`react-native-svg`) cukup menulis pemetaan elemen yang sama
seperti `dom.tsx`; deskripsi tidak perlu diubah.

## Web

`apps/frontend/tsconfig.json` memetakan `@timbolu/bolu-character/*` ke
`../../packages/bolu-character/*`, mengikuti pola `@timbolu/contracts/*`. Image
web di-build dari root repo (`apps/frontend/Dockerfile.root`), jadi `packages/`
ada di dalam konteks build.

```tsx
import { BoluCharacter } from "@timbolu/bolu-character/dom";

<BoluCharacter shape="bintang" expression="kaget" color="#2E9BEF" size={120} />;
```

`BoluCharacter` memakai `useId()` untuk memberi prefix unik pada id clip path
dan gradien, jadi beberapa karakter boleh tampil di satu halaman. Kalau
memanggil `charSVG()` langsung, kirim `idPrefix` unik per instance.

`apps/frontend/src/components/bolu/characters.tsx` tetap ada sebagai jalur impor
lama untuk halaman web; isinya hanya re-export dari paket ini.

## Menjalankan test

```bash
cd packages/bolu-character && npm test
```

atau langsung:

```bash
node --test packages/bolu-character/palette.test.ts packages/bolu-character/characters.test.ts
```

Node 22.18+ menanggalkan tipe `.ts` sendiri, jadi tidak ada test runner atau
dependensi yang perlu dipasang.
