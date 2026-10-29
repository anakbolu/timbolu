# `@timbolu/bolu-character`

Bagian netral dari karakter Bolu: nama 16 bentuk, 10 ekspresi, palet `BOLU_SWATCHES`, dan tiga helper
warna (`hexToHsl`, `inkFor`, `feetColorFor`), plus `BoluEyeOptions` — opsi mata yang tidak menyebut tipe
React sama sekali, jadi adapter `react-native-svg` bisa menghormatinya. Tanpa dependensi dan tanpa build:
`index.ts` diimpor apa adanya, sama seperti `@timbolu/contracts`.

Yang **tidak** ada di sini: geometri. `BOLU_SHAPES` dan `BOLU_EXPRESSIONS` badannya `ReactNode` (JSX),
jadi keduanya masih di `apps/frontend/src/components/bolu/characters.tsx` sampai ada representasi netral
plus dua adapter — DOM/SVG untuk web dan `react-native-svg` untuk app (roadmap #302 Fase 0 butir 2).
File itu me-re-export setiap nama di paket ini, jadi layar web tetap mengimpor dari satu modul.

## Web

`apps/frontend/tsconfig.json` memetakan `@timbolu/bolu-character` ke `index.ts` di sini, dan
`next.config.ts` menaikkan `turbopack.root` ke root repo — Turbopack tidak me-resolve file di luar root.
Build lokal (`npm run build` dari `apps/frontend`) hijau dengan pemetaan itu.

Image `timbolu`, bagaimanapun, di-build dengan build context `apps/frontend` saja, jadi `packages/`
tidak ada di dalam image dan impor lintas-paket gagal di `npm run build` — persis kegagalan #328 yang
membuat nama event di `src/lib/realtime.ts` disalin secara lokal (#330). Dua hal harus berubah sebelum
impor ini bisa sampai ke produksi: source path service `timbolu` di Easypanel dipindah ke root repo
(`/`, Dockerfile `apps/frontend/Dockerfile`), dan `packages/**` men-trigger deploy `timbolu` di
`.github/workflows/deployment.yml`. Sampai keduanya beres, `docker build apps/frontend` berhenti di
`Module not found: Can't resolve '@timbolu/bolu-character'`.

## App

```js
import { BOLU_SWATCHES, feetColorFor, inkFor } from "@timbolu/bolu-character";
```
