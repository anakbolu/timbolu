# `@timbolu/contracts`

Nama event realtime dan tipe payload-nya, satu sumber untuk web dan app Expo.
Kebenarannya `docs/api.md` (§ The stream); `events.ts` adalah turunannya.
Tanpa dependensi dan tanpa build — `events.ts` diimpor apa adanya.

`events.ts` memuat `CHAT_EVENT_TYPES`, `REALTIME_EVENT_TYPES`, `AGENT_STATES`
(ketiganya `as const`), plus `AgentStateEvent` dan `DraftCreatedEvent`.

Frontend masih punya salinannya sendiri: ia harus memuat semua nama di docs kecuali `task.step` dan
`notification.pending`, dua event internal yang tidak ditangani layar web. Nama di luar docs selalu ditolak.

`make contracts-check` membandingkan `docs/api.md`, `events.ts`, dan konstanta Go di
`apps/backend/internal`; exit 1 dengan nama yang hilang/berlebih kalau berselisih.