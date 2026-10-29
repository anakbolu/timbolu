# `@timbolu/contracts`

Nama event realtime dan tipe payload-nya, satu sumber untuk cek kontrak dan app Expo.
Kebenarannya `docs/api.md` (§ The stream); `events.ts` adalah turunannya.
Tanpa dependensi dan tanpa build — `events.ts` diimpor apa adanya.

`events.ts` memuat `CHAT_EVENT_TYPES`, `REALTIME_EVENT_TYPES`, `AGENT_STATES`
(ketiganya `as const`), plus `AgentStateEvent` dan `DraftCreatedEvent`.

`make contracts-check` membandingkan `docs/api.md`, `events.ts`, dan konstanta Go di
`apps/backend/internal`; exit 1 dengan nama yang hilang/berlebih kalau berselisih.

Frontend web **belum** mengimpor paket ini. Service Easypanel mem-build dengan build context
`apps/frontend` saja, jadi `packages/contracts` tidak ada di dalam image dan impor lintas-paket
gagal saat `npm run build` (#328). Sementara itu `apps/frontend/src/lib/realtime.ts` menyimpan
daftar namanya sendiri: ia harus memuat semua nama di docs kecuali `task.step` dan
`notification.pending`, dua event internal yang tidak ditangani layar web. Nama di luar docs selalu
ditolak. Setelah source path Easypanel diubah ke root repo, frontend bisa kembali mengimpor paket ini.
