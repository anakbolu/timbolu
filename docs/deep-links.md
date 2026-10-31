# Deep link

Kontrak ini menjawab satu pertanyaan: kalau sebuah notifikasi diketuk, layar mana yang harus
terbuka, dan dengan id apa. Tanpa kontrak, app Expo dan web akan menebak URL sendiri-sendiri, dan
notifikasi yang membuka layar salah lebih buruk daripada notifikasi yang tidak datang: orang
mengira pekerjaannya hilang.

Dokumen ini **kontrak, bukan implementasi**. App Expo-nya sudah ada sejak #488: `apps/mobile`
memakai Expo SDK 57 dan `expo-router`, dengan layar `index` (gerbang sesi), `login`, empat tab
`(tabs)/bolu`, `(tabs)/approval`, `(tabs)/tasks`, `(tabs)/routine`
(`apps/mobile/app/(tabs)/_layout.tsx`), dan layar per id `chat/[agentID]`, `task/[taskID]`,
`draft/[draftID]`, serta `routine/[routineID]`; detailnya di `docs/mobile.md`. `scheme` `bolu` juga
sudah dideklarasikan di `apps/mobile/app.json`.

Link masuk sudah dibaca app: `bolu://` sejak #500 dan `https://timbolu.com/...` sejak #540.
`app/+native-intent.tsx` (`redirectSystemPath`) mengembalikan `null` untuk URL `bolu://` maupun URL
https yang path-nya punya tujuan di app, sehingga router berhenti dan tidak mencocokkan
`bolu://chat/<id>` ke `chat/[agentID]` — berkas itu mengharapkan id Bolu, dan mencocokkannya akan
membuka thread yang tidak disebut link. Yang memutuskan tujuan adalah `app/_layout.tsx`
(`IncomingLinks`): URL peluncuran dibaca sebelum sesi diketahui, `src/lib/deep-link.ts`
(`parseIncomingLink`) menerjemahkannya menjadi `LinkTarget`, `src/lib/pending-link.ts` menahan
tautan yang datang sebelum sesi siap, dan `src/lib/link-destination.ts` (`destinationFor`)
menentukan tujuan beserta kalimatnya. `app/+not-found.tsx` menangani path yang tidak punya berkas
layar.

Layar tujuannya sudah ada untuk `task`, `approval`, dan `routine`:
`apps/mobile/app/task/[taskID].tsx` (detail tugas, dibuka dari tab `apps/mobile/app/(tabs)/tasks.tsx`),
`apps/mobile/app/draft/[draftID].tsx` (satu draf, dibuka dari tab
`apps/mobile/app/(tabs)/approval.tsx`), dan `apps/mobile/app/routine/[routineID].tsx` (satu rutin
beserta 20 run terakhir, dibuka dari tab `apps/mobile/app/(tabs)/routine.tsx`).

Sejak #532 `apps/mobile/src/lib/link-destination.ts` (`destinationFor`) memetakan `task`,
`approval`, dan `routine` berturut-turut ke `task/[taskID]`, `draft/[draftID]`, dan
`routine/[routineID]`, dan `apps/mobile/app/_layout.tsx` membukanya dengan `router.replace`. Jadi
`bolu://task/<id>`, `bolu://approval/<id>`, dan `bolu://routine/<id>` masing-masing mendarat di
layarnya sendiri.

Sejak #557 `apps/mobile/app/chat/thread/[conversationID].tsx` menerima id obrolan: thread 1:1
yang hasil `GET /conversations/:id` punya tepat satu partisipan `is_agent` dengan `agent_id`
diteruskan ke `/chat/<agent_id>`, obrolan grup atau thread lainnya ditampilkan kalimat
"Obrolan grup belum bisa dibuka di app. Pilih Bolu di bawah untuk melanjutkan.", dan id yang
tidak dikenal — termasuk yang dikembalikan `conversation_not_found` (404) — ditampilkan kalimat
"Obrolan tidak ditemukan. Pilih Bolu di bawah untuk melanjutkan." dengan tombol ke daftar Bolu.
`apps/mobile/src/lib/deep-link.ts` (`parseIncomingLink`) sudah memisahkan `chat` dari resource
lain, dan `apps/mobile/app/+native-intent.tsx` menahan seluruh `bolu://` serta URL
`https://timbolu.com/...` yang diklaim app dari router. Path tak dikenal mendarat di daftar
Bolu dengan `UNKNOWN_LINK_NOTICE`.

Selain itu `notification.pending` belum punya produsen dan `routine.run` baru ada sebagai nama.
Sisi app dari universal link sudah ada sejak #540: `apps/mobile/app.json` mendeklarasikan
`ios.associatedDomains` (`applinks:timbolu.com`) dan `android.intentFilters` (`autoVerify: true`,
host `timbolu.com`), `apps/mobile/src/lib/deep-link.ts` (`parseIncomingLink`) menerima
`https://timbolu.com/...` sebagai sumber kedua dan memetakan path yang punya tujuan di app ke
`LinkTarget` yang sama dengan `bolu://`, dan `apps/mobile/app/+native-intent.tsx` tidak lagi
menyerahkan URL itu ke router. Yang masih menunggu: kedua berkas well-known sudah menjawab `200` di
prod sejak #548, tetapi belum mengklaim app apa pun sampai identitas app diisi, jadi link https
**masih belum terbukti** membuka app — belum ada build bertanda tangan dan belum ada perangkat.

Nama event selalu bisa dicek di `packages/contracts/events.ts` dan `docs/api.md`
(§ The stream).

Route group `(workspace)` tidak menambah segmen ke URL: `app/(workspace)/tasks/[taskID]/page.tsx`
dilayani sebagai `/tasks/<task_id>`. Itu sebabnya tabel di bawah memakai path publik, bukan path
berkas.

## Sumber kebenaran

Rute web dibaca dari `apps/frontend/src/app`. Sebuah path hanya disebut "ada" kalau ada `page.tsx`
yang merendernya; `page.tsx` yang isinya hanya `redirect()` dihitung sebagai layar yang sudah
pindah. Nama endpoint, kode error, dan bentuk payload dibaca dari `docs/api.md`. Kalau keduanya
bertentangan, berkas halaman menang untuk path dan `docs/api.md` menang untuk endpoint.

## Audit rute

Setiap path yang disebut di dokumen ini, dicocokkan dengan `apps/frontend`. Kolom berkas relatif
terhadap `apps/frontend/src/app`.

| Path | Berkas halaman | Status |
| --- | --- | --- |
| `/` | `page.tsx` | ada — landing pemasaran, bukan layar app |
| `/login` | `login/page.tsx` | ada |
| `/signup` | `signup/page.tsx` | ada |
| `/verify` | `verify/page.tsx` | ada |
| `/forgot-password` | `forgot-password/page.tsx` | ada |
| `/onboarding` | `onboarding/page.tsx` | ada |
| `/join` | `join/page.tsx` | ada |
| `/pricing` | `pricing/page.tsx` | ada |
| `/contact` | `contact/page.tsx` | ada |
| `/privacy` | `privacy/page.tsx` | ada |
| `/terms` | `terms/page.tsx` | ada |
| `/admin/**` | `admin/page.tsx` (redirect ke `/admin/dashboard`), `admin/dashboard/page.tsx`, `admin/users/page.tsx` | ada |
| `/dashboard` | `(workspace)/dashboard/page.tsx` | ada |
| `/dashboard/integrations` | `(workspace)/dashboard/integrations/page.tsx` | ada |
| `/tasks` | `(workspace)/tasks/page.tsx` | pindah ke `/dashboard` — berkas hanya `redirect("/dashboard")`, tidak punya layar |
| `/tasks/<task_id>` | `(workspace)/tasks/[taskID]/page.tsx` | ada — keadaan id tidak valid diatur di § Keadaan saat tautan tidak bisa dibuka |
| `/chat` | `(workspace)/chat/page.tsx` | ada |
| `/chat?agent=<agent_id>` | `(workspace)/chat/page.tsx` | ada — query dibaca `components/chat/chat-view.tsx`; Bolu asing atau id bukan UUID berakhir di banner "Bolu tidak ditemukan …" / "Tautan tidak valid — id Bolu …" dengan tautan ke `/chat`, tanpa coba lagi |
| `/chat?conversation=<conversation_id>` | `(workspace)/chat/page.tsx` | ada — query dibaca `components/chat/chat-view.tsx`; obrolan asing berakhir di banner "Obrolan tidak ditemukan …" dengan tautan ke `/chat` tanpa parameter |
| `/groups` | `(workspace)/groups/page.tsx` | ada |
| `/routines` | `(workspace)/routines/page.tsx` | ada — daftar dan riwayat, tanpa halaman per-run |
| `/bolu/new` | `(workspace)/bolu/new/page.tsx` | ada |
| `/settings` | `(workspace)/settings/page.tsx` | ada |
| `/settings/team` | `(workspace)/settings/team/page.tsx` | ada — `TeamView`: tim dan Bolu-nya, bukan anggota manusia |
| `/settings/model` | `(workspace)/settings/model/page.tsx` | ada |
| `/settings/memory` | `(workspace)/settings/memory/page.tsx` | ada |
| `/settings/data` | `(workspace)/settings/data/page.tsx` | ada |
| `/settings/usage` | `(workspace)/settings/usage/page.tsx` | ada |
| `/settings/subscription` | `(workspace)/settings/subscription/page.tsx` | ada |
| `/approval/<draft_id>` | `(workspace)/approval/[draftID]/page.tsx` | ada — keadaan draf tidak ada diatur di § Keadaan saat tautan tidak bisa dibuka |
| `/chat/<conversation_id>` | — | tidak ada |
| `/routines/<run_id>` | `(workspace)/routines/[routineID]/page.tsx` | ada — bentuk path ini dilayani halaman rutin yang sama, tetapi id run bukan id rutin sehingga berakhir di layar "Rutinitas tidak ditemukan"; tetap tidak ada layar per-run |
| `/routines/<routine_id>` | `(workspace)/routines/[routineID]/page.tsx` | ada — halaman rutin yang difokus: kartu rutin disorot dan riwayat rutin itu yang disaring; id yang tidak ada berakhir di "Rutinitas tidak ditemukan" |
| `/drafts/<draft_id>` | — | tidak ada |

`/tasks/*`, `/approval/*`, `/routines/*`, dan `/settings/*` di berkas well-known adalah glob dari
baris `/tasks/<task_id>`, baris `/approval/<draft_id>`, baris `/routines/<routine_id>`, dan
baris-baris `/settings/...`. `/routines/*` mencakup kedua bentuk `/routines/...` karena keduanya
dilayani satu berkas dinamis yang sama, jadi id run pun sampai ke app dan berakhir di layar "tidak
ditemukan". Tidak ada path lain di bawah keempatnya yang dilayani.

## Keadaan saat tautan tidak bisa dibuka

Tautan dari notifikasi sering basi: tugas yang dirujuk sudah dihapus, drafnya sudah disetujui dari
perangkat lain, atau id-nya berasal dari ruang kerja lain. Orang yang mengetuk tautan itu tidak bisa
menindaklanjuti pesan developer, jadi layar yang tampil harus selalu kalimat Bahasa Indonesia yang
menyebut keadaannya, plus satu jalan keluar. Keadaan ini justru yang paling sering terjadi, karena
notifikasi memang dibaca belakangan — bukan kasus pinggiran.

Hasil pengamatan produksi 30 Oktober 2026 (akun uji baru), pada bentuk tautan yang dipakai
notifikasi. Baris tugas dan draf diamati setelah #448:

| Tautan | Yang tampil |
| --- | --- |
| `/routines/<uuid acak>` | "Rutinitas tidak ditemukan …" + tombol ke daftar rutinitas — BENAR |
| `/approval/<uuid acak>` | "Draf tidak ditemukan …" + tombol ke semua draf dan ke Dasbor — BENAR |
| `/tasks/<uuid acak>` | "Tugas tidak ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain." + tombol "← Kembali ke Dasbor" — BENAR |
| `/tasks/bukan-uuid` | "Tautan tidak valid — id tugas di tautan ini tidak dikenali. Buka dasbor lalu pilih tugas dari kartunya." + tombol "← Kembali ke Dasbor" — BENAR |
| `/approval/bukan-uuid` | "Tautan tidak valid — id draf di tautan ini tidak dikenali. Buka daftar draf lalu pilih dari sana." + tombol "← Kembali ke semua draf" — BENAR |

Aturan yang mengikat semua layar yang dibuka dari tautan:

1. **Pesan error server tidak pernah tampil.** `ApiError.message` — dan teks apa pun yang datang
   dari server — tidak dirender ke layar. Halaman bercabang dulu pada status atau kode, baru memilih
   kalimatnya sendiri. Pesan server hanya boleh masuk log.
2. **404 dan id yang tidak lolos validasi adalah layar yang sama.** Id asing dijawab
   `*_not_found` (404); id yang bukan UUID dijawab `invalid_input` (400) dengan teks seperti
   `task id is not a uuid` (`apps/backend/internal/modules/task/handler/handler.go`,
   `apps/backend/internal/modules/approval/handler/handler.go`). Keduanya berarti "tidak
   ditemukan" bagi orang yang mengetuk tautan, jadi keduanya memakai layar yang sama. Jangan ada
   layar "id bukan uuid": itu detail skema, bukan keadaan.
3. **Copy Bahasa Indonesia, menyebut objeknya, tanpa istilah developer.** "Tugas tidak ditemukan",
   bukan `task not found`; "Draf tidak ditemukan", bukan `draft not found`. Sebut juga kemungkinan
   penyebabnya dalam bahasa manusia — dihapus, sudah diputuskan, atau dari ruang kerja lain.
4. **Selalu ada jalan keluar.** Setiap layar tidak ditemukan menyertakan tautan kembali: Dasbor
   untuk tugas dan draf, `/routines` untuk rutinitas, `/chat` tanpa parameter untuk obrolan. Layar
   tanpa tautan keluar memaksa orang menekan tombol back sistem, dan di app itu berarti keluar dari
   app.
5. **Tidak ada retry otomatis untuk 404.** "Coba lagi" hanya untuk kegagalan jaringan atau 5xx;
   menawarkannya pada 404 menjanjikan sesuatu yang tidak akan berubah.
6. **Draf yang sudah diputuskan bukan tautan rusak.** `draft_already_decided` (409) membuka drafnya
   dan menampilkan keputusan yang berlaku — lihat tabel perilaku per resource di § Skema `bolu://`.

Padanan app untuk keadaan yang sama ada di tabel "Perilaku per resource saat resource tidak bisa
dibuka" (§ Skema `bolu://`); aturan di atas adalah sisi web-nya. Layar app yang sudah ada memakai
kalimat yang sama — `app/task/[taskID].tsx`, `app/draft/[draftID].tsx`, dan
`app/routine/[routineID].tsx` — sedangkan target `chat` belum punya layar, jadi belum ada kalimat
app untuk `conversation_not_found`.

`/tasks/*` dan `/approval/<id bukan UUID>` sekarang memenuhi aturan 1–4: `ApiError.message` tidak
dirender, 404 dan id yang tidak lolos validasi memakai layar yang sama, kalimatnya Bahasa Indonesia
yang menyebut objeknya, dan setiap layar membawa tautan kembali. Pemetaan status dan kode ke kalimat
itu ada di satu tempat, `apps/frontend/src/lib/friendly-error.ts` (`friendlyLoadFailure`), dipakai
`components/dashboard/task-detail-view.tsx` dan `components/dashboard/approval-view.tsx` (#448).

`/chat?conversation=<id asing>` sudah memenuhi aturan 1–5 sejak #454: setelah daftar
obrolan selesai dimuat dan id itu tidak ada di dalamnya, panel menampilkan "Obrolan tidak ditemukan
— mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain." beserta tautan "Kembali ke
obrolan" ke `/chat` tanpa parameter, dan tidak menawarkan coba lagi. Id yang bukan UUID berakhir di
layar yang sama, sesuai aturan 2.

Tidak ada pelanggaran yang diketahui tersisa pada bentuk tautan yang sudah punya layar keadaan:
`/tasks/<id>`, `/approval/<id>`, `/routines/<id>`, `/chat?conversation=<id>`, dan
`/chat?agent=<id>` kini memenuhi aturan 1–5. Untuk `/chat?agent=<id asing>`, `POST
/conversations/direct` menjawab 404 `agent_not_found`, sehingga layar menampilkan "Bolu tidak
ditemukan — mungkin sudah dihapus, atau tautannya berasal dari ruang kerja lain." beserta tautan
"Kembali ke obrolan" ke `/chat` dan tidak menawarkan coba lagi; id yang bukan UUID menampilkan
"Tautan tidak valid — id Bolu di tautan ini tidak dikenali. …" dengan tautan yang sama. Kalimatnya
ada di `apps/frontend/src/lib/friendly-error.ts` (`INVALID_LINK.agent`, `NOT_FOUND.agent`) dan
dipakai `components/chat/chat-view.tsx` (`openFailure`); 404 untuk Bolu asing ditutup #455, layar
keadaannya #456.

## Status implementasi app

Keadaan `apps/mobile` hari ini, dicocokkan ke berkas:

- App ada sejak #488: `apps/mobile` memakai Expo SDK 57 dan `expo-router` (`apps/mobile/package.json`, `main` `expo-router/entry`), dengan `scheme` `bolu` dan plugin `expo-router` di `apps/mobile/app.json`. Ringkasannya di `docs/mobile.md`.
- `app/index.tsx` bukan layar berisi: ia gerbang yang menunggu pemulihan sesi lalu mengalihkan ke `(tabs)/bolu` atau `/login`.
- `app/login.tsx` form masuk; `app/(tabs)/_layout.tsx` menyusun empat tab — `app/(tabs)/bolu.tsx` daftar Bolu, `app/(tabs)/approval.tsx` draf yang menunggu keputusan, `app/(tabs)/tasks.tsx` riwayat tugas, `app/(tabs)/routine.tsx` rutinitas — dan layar per id: `app/chat/[agentID].tsx` satu obrolan, `app/task/[taskID].tsx` detail tugas, `app/draft/[draftID].tsx` satu draf, `app/routine/[routineID].tsx` satu rutin beserta riwayat run.
- Token: refresh token disimpan di `expo-secure-store` (`apps/mobile/src/lib/tokens.ts`), access token hanya di memori, dan refresh dikirim lewat body `POST /auth/refresh` tanpa cookie (`apps/mobile/src/lib/api.ts`, `refreshSession`).
- Gate CI: job `mobile` di `.github/workflows/checks.yml` menjalankan `make typecheck-mobile` (#493).
- Link masuk (#500, #540): `app/+native-intent.tsx` mengembalikan `null` untuk `bolu://` dan untuk URL `https://timbolu.com/...` yang diklaim app supaya router tidak mencocokkan path, `app/_layout.tsx` (`IncomingLinks`) membaca `Linking.getInitialURL()` dan `Linking.addEventListener("url")`, lalu `src/lib/deep-link.ts` (`parseIncomingLink`), `src/lib/pending-link.ts`, dan `src/lib/link-destination.ts` (`destinationFor`) menerjemahkan, menahan, dan menentukan tujuan. `apps/mobile/app.json` mendeklarasikan `ios.associatedDomains` (`applinks:timbolu.com`) dan `android.intentFilters` untuk host yang sama, dan `parseIncomingLink` menerima `https://timbolu.com/...` selain `bolu://` (#540). `app/+not-found.tsx` adalah layar cadangan untuk path yang tidak punya berkas layar. Target `task`, `approval`, dan `routine` dibuka di `app/task/[taskID].tsx`, `app/draft/[draftID].tsx`, dan `app/routine/[routineID].tsx` (#532); hanya target `chat` yang berakhir di `/(tabs)/bolu` dengan notis karena id-nya id obrolan, bukan id Bolu (lihat § Skema `bolu://`).
- Belum: push notification dan registrasi `POST /devices`; endpoint-nya ada, kliennya belum.
- Obrolan hidup (#505, #514): layar chat membuka satu koneksi SSE ke `/events/stream` lewat `src/lib/chat-stream.ts` selama layar itu tampil, `AppState` menutupnya saat app masuk latar, dan koneksi berikutnya membawa kursor `last_event_id` yang tetap di memori sehingga server memutar ulang frame yang terlewat. Frame `message.new`/`message.updated` masuk langsung ke daftar tanpa memuat ulang layar; satu pembacaan ulang 5 detik setelah kirim dan tombol "Muat ulang" tetap ada sebagai jaring (tarik-untuk-menyegarkan baru ada di daftar Bolu). `/events/socket` (WebSocket) tidak dipakai app, dan status transport belum ditampilkan (`docs/mobile.md` § Obrolan hidup dari `/api/events/stream`).

## Skema `bolu://`

| Link | `<id>` | Layar yang dibuka | Padanan web hari ini |
| --- | --- | --- | --- |
| `bolu://task/<task_id>` | `tasks.id` (UUID) | Detail tugas: keadaan, alasan berhenti, langkah, hasil serahan | `/tasks/<task_id>` |
| `bolu://chat/<conversation_id>` | `conversations.id` (UUID) | Obrolan itu: 1:1 dengan Bolu, grup, atau thread | `/chat?conversation=<conversation_id>` |
| `bolu://approval/<draft_id>` | `drafts.id` (UUID) | Draf persetujuan itu: keputusan yang berlaku dan tombol putusnya | `/approval/<draft_id>` |
| `bolu://routine/<routine_id>` | `routines.id` (UUID) | Rutin itu beserta 20 run terakhir | `/routines/<routine_id>` |

Kolom "Layar yang dibuka" adalah tujuan kontrak, dan sejak #532 baris `task`, `approval`, dan
`routine` sudah memenuhinya di app: `destinationFor` mengembalikan target `task`, `draft`, dan
`routine`, lalu `app/_layout.tsx` membuka `task/[taskID]`, `draft/[draftID]`, dan
`routine/[routineID]`. Hanya `chat` yang belum: tidak ada layar per `conversations.id` —
`app/chat/[agentID].tsx` membuka dari id Bolu — jadi `bolu://chat/<id>` mendarat di `/(tabs)/bolu`
dengan notis "Layar yang dituju tautan ini belum ada di app…". Path yang tidak dikenal juga
mendarat di daftar Bolu, dengan `UNKNOWN_LINK_NOTICE`.

Aturan yang mengikat keempatnya:

1. **`<id>` wajib UUID, tanpa query, fragment, atau token.** Id yang bukan UUID ditolak sebelum
   layar dibuka; app tidak boleh mengarang id dari string lain.
2. **Link tidak membawa workspace.** App membuka link di workspace aktif, bukan berpindah workspace
   karena link. Setiap read di API di-scope tenant, dan id milik workspace lain dijawab
   `task_not_found` / `conversation_not_found` / `draft_not_found` / `routine_not_found` (404) —
   bukan 403. Jadi link lintas-workspace tidak punya arti yang bisa dihormati.
3. **Hilang dan terlarang adalah jawaban yang sama.** API tidak membocorkan "ada tapi bukan
   milikmu": id asing dijawab 404. App memperlakukan 404 sebagai "tidak ditemukan", bukan sebagai
   error jaringan yang layak di-retry.
4. **`bolu://` tidak diverifikasi siapa pun.** Android tidak memverifikasi pemilik skema custom, dan
   iOS tidak menjamin app mana yang menang kalau ada dua app mendaftarkan skema yang sama. Karena
   itu notifikasi memakai universal link (bagian berikut); `bolu://` dipakai untuk navigasi internal
   dan pengujian.
5. **Path yang tidak dikenal jatuh ke daftar Bolu**, bukan ke layar error. Link lama yang pathnya
   sudah tidak ada tidak boleh membuat app berhenti.

Perilaku per resource saat resource tidak bisa dibuka:

Tabel ini menggambarkan layar resource-nya, bukan jalur link ke sana. `app/task/[taskID].tsx`
sudah memenuhinya untuk tugas: 404 dan `invalid_input` sama-sama menjadi "Tugas tidak ditemukan."
dengan tombol kembali. `app/draft/[draftID].tsx` dan `app/routine/[routineID].tsx` juga sudah punya
layar "Draf tidak ditemukan." dan "Rutinitas tidak ditemukan.", dan sejak #532 keduanya juga bisa
dicapai lewat `bolu://approval/...` dan `bolu://routine/...`, bukan hanya lewat tab. Target `chat`
tetap mendarat di daftar Bolu, jadi barisnya belum pernah sampai ke layar resource.

| Resource | Jawaban API | Yang dilakukan app |
| --- | --- | --- |
| Tugas | `task_not_found` (404), `invalid_input` (400) | Layar "Tugas tidak ditemukan." dengan tombol "Kembali ke daftar tugas"; tidak membuka tugas lain, tidak menawarkan coba lagi, dan tidak crash |
| Obrolan | `conversation_not_found` (404) | Belum ada layar: link `bolu://chat/<id>` mendarat di daftar Bolu dengan notis |
| Draf | `draft_not_found` (404) | Layar "Draf tidak ditemukan." dengan tombol "Kembali" (`router.back()`) |
| Rutinitas | `routine_not_found` (404), `invalid_input` (400) | Layar "Rutinitas tidak ditemukan." dengan tombol "Kembali ke daftar rutinitas" |
| Draf yang sudah diputuskan | `draft_already_decided` (409) | Draf yang dibuka menampilkan keputusan yang berlaku tanpa kontrol putus; kalau keputusan dikirim setelah orang lain memutuskan, app menampilkan "Draf ini sudah diputuskan orang lain." lalu kembali ke daftar |

## Universal link

Universal link adalah jalur yang dipakai notifikasi. Bentuknya `https://timbolu.com/<path>`, host
yang sama dengan web hari ini (`apps/frontend` dilayani di `timbolu.com`, dan API-nya di host yang
sama — lihat `docs/deployment.md`). Pemetaannya diturunkan dari route yang benar-benar ada:

Sisi app dari universal link sudah terpasang sejak #540: `apps/mobile/app.json` mendeklarasikan
`ios.associatedDomains` (`applinks:timbolu.com`) dan `android.intentFilters` dengan
`autoVerify: true` untuk host `timbolu.com`, dan `apps/mobile/src/lib/deep-link.ts`
(`parseIncomingLink`) menerima URL `https://timbolu.com/...` yang punya tujuan di app — bentuk
`/tasks/<id>`, `/approval/<id>`, `/routines/<id>`, dan `/chat?conversation=<id>` — sebagai sumber
kedua di samping `bolu://`. `/chat?agent=<id>` belum dipetakan: `?agent=` menyebut id Bolu,
sedangkan target link harus menyebut id obrolan, jadi `parseUniversalLink` mengembalikan
`fallback("unknown-path")` dan link itu mendarat di daftar Bolu dengan notis, sama seperti
`bolu://chat/<id>`. `apps/mobile/app/+native-intent.tsx` mengembalikan `null` untuk URL itu supaya
router tidak memetakan path web ke berkas layar; URL https lain tetap ditangani router.

Yang belum: kedua berkas well-known sudah menjawab `200 application/json` di `timbolu.com` sejak
#548, tetapi belum mengklaim app apa pun sampai identitas app diisi (§ Identitas app yang belum
diketahui). Platform karena itu belum memverifikasi app sebagai pemilik domain, dan link https
**masih belum terbukti** membuka app: belum ada build bertanda tangan dan belum ada perangkat. § Cara
memverifikasi adalah langkah yang harus dijalankan setelah identitas itu ada. Tabel berikut adalah
tujuan tiap path di app, bukan bukti bahwa link sudah hidup.

| Universal link | Layar app | Catatan |
| --- | --- | --- |
| `/dashboard` | Dasbor | Daftar keputusan yang menunggu, tanpa fokus ke satu draf |
| `/chat?agent=<agent_id>` | Obrolan 1:1 | Bentuk link yang ditulis sidebar untuk thread langsung |
| `/chat?conversation=<conversation_id>` | Obrolan | Grup dan thread; ini yang dipakai `bolu://chat/<id>` |
| `/tasks` | Dasbor | Layar daftar tugas sudah dihapus; `/tasks` hanya `redirect("/dashboard")`, jadi path ini tidak punya layar app sendiri |
| `/tasks/<task_id>` | Detail tugas | Tujuan `bolu://task/<id>` |
| `/approval/<draft_id>` | Draf persetujuan | Tujuan `bolu://approval/<id>`; satu draf beserta tombol putusnya |
| `/routines` | Rutinitas | Daftar dan riwayat; tidak ada halaman per-run |
| `/routines/<routine_id>` | Rutinitas | Tujuan `bolu://routine/<id>`; kartu rutin disorot dan riwayatnya tersaring |
| `/groups` | Grup | |
| `/bolu/new` | Bolu baru | |
| `/dashboard/integrations` | Integrasi | |
| `/settings` | Pengaturan | |
| `/settings/team` | Tim dan Bolu-nya | `TeamView`: tim workspace beserta Bolu di dalamnya, bukan anggota manusia |
| `/settings/model` | Model | |
| `/settings/memory` | Memori | |
| `/settings/data` | Data dan penghapusan | |
| `/settings/usage` | Pemakaian | |
| `/settings/subscription` | Paket dan tagihan | |

Yang **tidak** diklaim sebagai universal link: `/login`, `/signup`, `/verify`, `/forgot-password`,
`/onboarding`, `/join`, `/pricing`, `/contact`, `/privacy`, `/terms`, `/`, dan seluruh `/admin/**`.
Halaman-halaman itu milik browser: app tidak punya layarnya, dan link yang membuka app ke layar
kosong lebih buruk daripada link yang tetap di web. `/admin/**` juga layar operator, bukan layar
anggota workspace.

Query string ikut terbawa ke app di kedua platform, jadi `/chat?conversation=<id>` tetap membawa
id-nya. Jangan mengubahnya menjadi `/chat/<id>`: path itu tidak ada di web.

`bolu://routine/<routine_id>` sudah punya padanan universal link: `/routines/<routine_id>`, halaman
rutin yang difokus (kartu rutin disorot, riwayatnya tersaring). Notifikasi run rutin tetap memakai
`bolu://task/<task_id>` (lihat tabel di bawah), bukan link rutin.

## Celah

Bagian ini mencatat target yang **belum punya layar web** atau **belum bisa dicapai dari link**,
supaya tidak ada URL karangan di dokumen ini atau di app.

### Layar web yang belum ada

- **`/chat/<conversation_id>` tidak ada.** Obrolan dialamatkan lewat query (`?agent=` atau
  `?conversation=`), bukan segmen path.
- **`/routines/<run_id>` tidak punya layar sendiri.** Path itu dilayani
  `(workspace)/routines/[routineID]/page.tsx`, tetapi id run bukan id rutin, jadi yang terbuka
  adalah layar "Rutinitas tidak ditemukan" dengan jalan kembali — bukan riwayat run itu.
- **`/drafts/<draft_id>` tidak ada.** `GET /drafts/:id` ada di API, tetapi tidak ada halaman web
  yang menampilkannya.

### Payload event yang belum membawa id yang dibutuhkan

- `task.waiting_approval` dipancarkan dari worker (`apps/backend/internal/modules/task/activity/activity.go`,
  `apps/backend-js/src/modules/task/activity/activity.ts`) dengan payload lima field: `task_id`,
  `agent_id`, `status`, `trigger`, `title`. Kedua runtime sekarang **juga** membawa
  `waiting_draft_id` (UUID string) saat tugas menunggu sebuah draf; key itu tidak dikirim saat tugas
  menunggu tanpa draf, bukan dikirim kosong, supaya worker bisa membedakan "tidak ada draf untuk
  dibuka" dari "draf ber-id kosong". Event lain tetap lima field, jadi field ini hanya ada di event
  ini. Worker notifikasi boleh membangun `bolu://approval/<waiting_draft_id>` langsung dari payload
  dan tidak perlu membaca `GET /tasks/:id` lebih dulu.
- `routine.run` belum punya produsen: nama itu hanya ada sebagai nama di
  `packages/contracts/events.ts`, `docs/api.md`, `apps/frontend/src/lib/realtime.ts`,
  `apps/frontend/src/lib/dashboard-view.ts`, konstanta Go/Bun, dan pemetaan push
  (`apps/backend-js/src/modules/push/domain/push.ts`, `notificationFor`). Pemetaan itu membangun
  `bolu://routine/<routine_id>` dari `routineId`, bukan dari `task_id` milik run, jadi produsennya
  wajib membawa `routine_id` saat event ini benar-benar dipancarkan.
- `notification.pending` juga belum punya produsen: konstanta Go/Bun menyebutnya "the hook the
  notification worker consumes", dan `notificationFor` sudah memetakannya ke
  `bolu://approval/<draft_id>` dari `draftId`, tetapi tidak ada yang memancarkannya, jadi belum ada
  payload nyata yang bisa dipakai membangun link.

### Identitas app yang belum diketahui

Apple Team ID, bundle id iOS, package name Android, dan SHA-256 sertifikat Android belum ada di repo
ini, dan nilainya dibaca dari tiga environment variable deployment: `APPLE_APP_ID`
(`<TEAM_ID>.<BUNDLE_ID>`), `ANDROID_PACKAGE_NAME`, dan `ANDROID_CERT_SHA256`.
`apps/frontend/src/lib/app-links.ts` (`appleAppSiteAssociation`, `assetLinks`) membacanya saat
request — kedua route handler memanggil `connection()` supaya jawabannya tidak dibekukan saat build —
jadi mengisi nilainya adalah satu perubahan konfigurasi deployment, bukan perubahan kode, dan image
yang sama menyajikan identitas environment tempat ia berjalan. `ANDROID_CERT_SHA256` boleh berisi
beberapa fingerprint dipisah koma: Play App Signing dapat merotasi sertifikat sementara build lama
masih ditandatangani kunci lama, dan keduanya harus terdaftar sampai rotasi selesai.

`apps/mobile/app.json` sekarang mendeklarasikan `ios.associatedDomains` dan `android.intentFilters`
(#540), tetapi tidak mendeklarasikan `ios.bundleIdentifier` maupun `android.package`, dan belum ada
`eas.json`; nilainya harus sama dengan yang diisikan ke ketiga variable di atas. Apple Team ID dan
bundle id menunggu akun Apple Developer; package name dan SHA-256 menunggu Play Console atau keystore
rilis.

Selama ketiganya kosong, kedua berkas **sengaja tidak mengklaim app apa pun**: AASA menjawab
`{"applinks":{"details":[]}}` dan assetlinks menjawab `[]`, bukan entri placeholder. Berkas yang
menyebut app yang salah bukan placeholder yang aman: Apple membacanya sebagai jawaban, tidak
menemukan app dengan identitas itu, lalu menolak setiap link selama berbulan-bulan karena CDN-nya
menyimpan hasilnya, tanpa satu pun pesan yang menjelaskan sebabnya; Google juga memeriksa lalu
menolak entri dengan package atau fingerprint placeholder. Klaim kosong adalah jawaban yang salah
dengan cara yang kelihatan dari satu `curl` dan pulih sendiri begitu identitasnya diisi.

## Berkas well-known

Dua berkas ini yang membuat universal link dipercaya platform. Keduanya sudah disajikan `timbolu.com`
sejak #548, tetapi belum mengklaim app apa pun: produksi menjawab `200 application/json` dengan badan
`{"applinks":{"details":[]}}` dan `[]`. Isi kosong itu disengaja — lihat § Identitas app yang belum
diketahui — jadi deklarasi domain di `apps/mobile/app.json` belum cukup untuk membuka app.

URL publiknya tidak datang dari folder `.well-known` di `app/`: Next memperlakukan nama berdot
sebagai folder privat, jadi berkas di situ lolos build dan tidak pernah tersaji. Yang menyajikan
adalah dua route handler, `apps/frontend/src/app/api/well-known/apple-app-site-association/route.ts`
dan `apps/frontend/src/app/api/well-known/assetlinks.json/route.ts`, sedangkan URL publiknya dipasang
rewrite di `apps/frontend/next.config.ts`. Kedua handler memakai `NextResponse.json` supaya
`Content-Type`-nya `application/json`.

Isi kedua berkas dibangun dari satu modul, `apps/frontend/src/lib/app-links.ts`: `appIDs` dan
`components` dari `APPLE_APP_ID` serta `UNIVERSAL_LINK_PATHS`, lalu `package_name` dan
`sha256_cert_fingerprints` dari `ANDROID_PACKAGE_NAME` serta `ANDROID_CERT_SHA256`. Daftar path di
modul itu mengikuti tabel § Universal link, jadi tabel itu tetap sumber kebenaran; bentuk dan syarat
penyajiannya ditetapkan di bawah.

### `/.well-known/apple-app-site-association`

Tanpa ekstensi, satu objek JSON, tanpa signature (iOS 9 ke atas tidak meminta signature). Bentuk ini
yang dibangun `appleAppSiteAssociation()` setelah `APPLE_APP_ID` diisi; selama kosong, badan yang
tersaji `{"applinks":{"details":[]}}`:

```json
{
  "applinks": {
    "details": [
      {
        "appIDs": ["<TEAM_ID>.<BUNDLE_ID>"],
        "components": [
          { "/": "/dashboard" },
          { "/": "/chat" },
          { "/": "/tasks/*" },
          { "/": "/approval/*" },
          { "/": "/routines" },
          { "/": "/routines/*" },
          { "/": "/groups" },
          { "/": "/bolu/new" },
          { "/": "/dashboard/integrations" },
          { "/": "/settings" },
          { "/": "/settings/*" }
        ]
      }
    ]
  }
}
```

Field yang wajib benar: `appIDs` berisi Team ID dan bundle id yang sama dengan yang ditandatangani
build; `components` hanya memuat path yang benar-benar dilayani app. Path yang diklaim tetapi tidak
bisa dirender app akan membuka app ke layar kosong — lebih buruk daripada tetap di browser.
`/tasks` sengaja tidak ada di daftar itu: path tersebut hanya redirect ke `/dashboard` di web.
`/tasks/*` tetap ada karena detail tugas punya layar sendiri. `/routines/*` ada karena halaman rutin
yang difokus punya alamat sendiri; `/routines` tetap dicantumkan untuk daftar biasa.

### `/.well-known/assetlinks.json`

Bentuk ini yang dibangun `assetLinks()` setelah `ANDROID_PACKAGE_NAME` dan `ANDROID_CERT_SHA256`
diisi; selama salah satunya kosong, badan yang tersaji `[]`:

```json
[
  {
    "relation": ["delegate_permission/common.handle_all_urls"],
    "target": {
      "namespace": "android_app",
      "package_name": "<PACKAGE_NAME>",
      "sha256_cert_fingerprints": ["<SHA256_FINGERPRINT>"]
    }
  }
]
```

`sha256_cert_fingerprints` harus sidik jari sertifikat yang menandatangani APK/AAB yang dipasang —
bukan sertifikat debug, dan bukan sertifikat upload jika Play App Signing memakai kunci lain. Kalau
rotasi Play App Signing sedang berjalan, cantumkan kedua sidik jari sebagai dua nilai dipisah koma di
`ANDROID_CERT_SHA256`; `assetLinks()` memecahnya menjadi array.

### Syarat penyajian

- URL: `https://timbolu.com/.well-known/apple-app-site-association` dan
  `https://timbolu.com/.well-known/assetlinks.json`.
- **`200` langsung, `Content-Type: application/json`, tanpa redirect.** Apple dan Google tidak
  mengikuti `301`/`302` untuk berkas ini. Tanpa autentikasi, tanpa cookie, tanpa query. Hari ini
  (31 Oktober 2026) keduanya sudah memenuhi itu: `200 application/json`, tanpa `location`, dengan
  badan `{"applinks":{"details":[]}}` dan `[]`.
- Berkas AASA tidak punya ekstensi. `public/` tidak dipakai justru karena itu: Next menyajikan berkas
  tanpa ekstensi sebagai `application/octet-stream`, dan Apple menolaknya. Yang dipakai route handler
  di atas, yang memaksa `application/json`; rewrite di `next.config.ts` yang memasang URL-nya.
- Badan kosong berubah menjadi klaim yang sebenarnya tanpa deploy kode baru: identitasnya dibaca
  per request, jadi cukup mengisi `APPLE_APP_ID`, `ANDROID_PACKAGE_NAME`, dan `ANDROID_CERT_SHA256`
  di deployment.
- Apple mengambil AASA lewat CDN-nya sendiri, jadi perubahan tidak langsung terlihat. Saat menguji,
  jangan menyimpulkan "salah" hanya dari satu percobaan setelah deploy; CDN juga bisa menyimpan
  jawaban kosong lebih lama dari yang diharapkan.

## Aturan pemetaan notifikasi → layar

Nama event di bawah adalah ejaan yang ada di kode, bukan terjemahan: `packages/contracts/events.ts`
dan `docs/api.md` (§ The stream). Setiap baris activity punya envelope
`{id, type, workspace_id, agent_id, conversation_id, task_id, draft_id, payload}` di wire
(`apps/backend/internal/modules/chat/handler/handler.go`), jadi id di kolom kedua bisa dibaca dari
envelope, bukan hanya dari `payload`.

| Event | Id yang tersedia hari ini | Deep link yang dibawa | Kenapa |
| --- | --- | --- | --- |
| `task.finished` | `task_id` (envelope dan payload), `conversation_id` (envelope), `status`, `trigger` | `bolu://task/<task_id>` | Layar tugas memuat ringkasan, alasan berhenti, dan langkah — satu-satunya tempat pertanyaan "apa yang tadi dikerjakan" dijawab utuh |
| `task.waiting_approval` | `task_id` (envelope dan payload), `conversation_id` (envelope), `waiting_draft_id` (payload, hanya saat menunggu sebuah draf) | `bolu://approval/<waiting_draft_id>` | Notifikasi ini ada supaya orang bisa memutuskan; mendarat di tugas tanpa tombol putus adalah perjalanan yang sia-sia. Kalau payload tidak membawa `waiting_draft_id` (menunggu tanpa draf), tidak ada yang bisa diputuskan dan tidak ada link yang dikirim |
| `draft.created` | `draft_id`, `task_id` (envelope dan payload), `conversation_id` (envelope), `title`, `summary`, `action_kind` | `bolu://approval/<draft_id>` | Draf adalah objek yang diputuskan; `task_id` hanya menjelaskan asalnya |
| `routine.run` | belum ada produsen; run menyimpan `task_id` (`routine_runs.task_id`) | `bolu://routine/<routine_id>` | `notificationFor` membangun link dari `routine_id` milik rutin, jadi produsennya wajib membawa id itu; tidak ada halaman per-run yang dibuka |
| `notification.pending` | belum ada produsen; pemetaannya memakai `draft_id` | `bolu://approval/<draft_id>` | `notificationFor` menunjuk draf yang menunggu keputusan; sampai ada pemancar, baris ini belum pernah terkirim |

Aturan tambahan:

1. **Link dibangun di server, dari id di baris event.** App tidak merangkai id dari teks notifikasi
   dan tidak menebak dari judul.
2. **Tidak ada notifikasi tanpa tujuan.** Kalau link tidak bisa dibangun (id kosong, event tidak
   dikenal), notifikasi tidak dikirim. Notifikasi yang membuka dasbor "karena tidak tahu" melatih
   orang mengabaikan notifikasi.
3. **`task.finished` dengan `trigger` `routine` tetap memakai layar tugas.** Sampai `routine.run`
   punya produsen, run rutin sudah terlihat sebagai `task.finished` dengan `trigger: "routine"`;
   tidak perlu menunggu event baru untuk bisa membuka layar yang benar.
4. **`bolu://routine/<routine_id>` memakai `routine_id` milik rutin, bukan `task_id` run.**
   `notificationFor` sudah membangun bentuk itu untuk `routine.run`; produsennya wajib membawa
   `routine_id` saat event ini dipancarkan.

## Auth saat cold start

Aturan yang harus dipegang app, dalam urutan yang sama:

1. **Link dibaca lebih dulu, sesi dicek kemudian.** App menyimpan link yang masuk (skema maupun
   universal) sebelum memutuskan layar awal, karena link yang tiba saat app belum jalan adalah
   justru kasus yang paling sering: notifikasi diketuk dari keadaan app mati.
2. **Belum login berarti link ditunda, bukan dibuang.** Setelah login (atau refresh sesi) berhasil,
   app kembali ke layar yang dituju link, bukan ke dasbor. Link yang tertunda harus bertahan kalau
   proses login memindahkan app ke browser (misalnya OAuth) dan app dibuka lagi setelahnya.
3. **Token tidak pernah lewat URL.** Tidak ada access token, refresh token, atau id sesi di
   `bolu://`, di query universal link, atau di notifikasi. App menyimpan refresh token di
   `expo-secure-store` dan access token di memori (Fase 0 butir 4); link hanya membawa id resource.
   API memang menerima token di query untuk stream (`GET /events/stream`, `GET /events/socket`),
   tetapi itu detail transport, bukan contoh untuk link yang ditulis ke notifikasi dan log.
4. **Workspace tetap yang aktif.** Login tidak mengubah workspace demi sebuah link; kalau resource
   ada di workspace lain, jawabannya 404 dan app menampilkan layar "tidak ditemukan".
5. **Web sudah memenuhi aturan ini untuk tautan workspace.** Gate di
   `apps/frontend/src/proxy.ts` membawa layar yang diminta sebagai
   `/login?next=<path+query>` ketika tidak ada sesi, dan
   `apps/frontend/src/components/auth/credentials-form.tsx` (`safeNextPath`) hanya menerima path
   internal — diawali satu `/`, tanpa skema, tanpa `//`, tanpa backslash — lalu mengembalikan orang ke
   layar itu setelah login berhasil; nilai yang tidak lolos dibuang seluruhnya, bukan diperbaiki
   menjadi tujuan lain. Sesi yang habis saat halaman sedang terbuka mengalihkan lewat
   `apps/frontend/src/components/dashboard/workspace-data.tsx` ke
   `/login?sesi=berakhir&next=<halaman itu>`, sehingga penjelasan sesi berakhir dan tujuannya
   terbawa bersama. Yang masih menjadi kontrak app, bukan perilaku web: membaca link sebelum sesi
   dan menyimpan link tertunda ketika login berpindah ke browser (butir 1–2); aturan 3–4 sudah
   berlaku di web karena tidak ada token di URL dan login tidak berpindah workspace.

## Cara memverifikasi

Butir 6 ditutup hanya kalau pemeriksaan ini punya bukti. Semua dijalankan pada build yang
ditandatangani (TestFlight/internal track), bukan build debug, karena asosiasi domain bergantung pada
tanda tangan.

1. **Berkas well-known tersaji.** Langkah pertama, dan satu-satunya yang bisa ditutup hari ini:
   ```bash
   curl -sS -D- https://timbolu.com/.well-known/apple-app-site-association
   curl -sS -D- https://timbolu.com/.well-known/assetlinks.json
   ```
   Harapan: `200`, `content-type: application/json`, tanpa `location`; badan AASA
   `{"applinks":{"details":[]}}` dan assetlinks `[]` selama identitas app belum diisi. Badan kosong
   bukan kegagalan langkah ini — penyajiannya sudah benar; yang belum ada adalah identitas app, jadi
   langkah 2 dan 3 masih menunggu konfigurasi deployment itu, dan langkah 4–8 menunggu build serta
   perangkat.
2. **Apple melihat berkas yang sama.**
   ```bash
   curl -sS https://app-site-association.cdn-apple.com/a/v1/timbolu.com
   ```
   Harapan: JSON yang sama dengan langkah 1. Selama langkah 1 masih `details: []`, jawaban di sini
   juga belum mengklaim app, jadi langkah ini baru membuktikan asosiasi setelah `APPLE_APP_ID` diisi.
   Kalau kosong, tunggu propagasi CDN sebelum menyalahkan konfigurasi.
3. **Android sudah memverifikasi domain.**
   ```bash
   adb shell pm get-app-links <package_name>
   ```
   Harapan: `timbolu.com` berstatus `verified`. Langkah ini butuh APK bertanda tangan yang package
   name-nya sama dengan `ANDROID_PACKAGE_NAME`; selama nilainya kosong, belum ada app yang bisa
   diverifikasi.
4. **Cold start, app mati, link diketuk.** Hentikan app, lalu buka link dari notifikasi (bukan dari
   kolom alamat):
   ```bash
   adb shell am force-stop <package_name>
   adb shell am start -a android.intent.action.VIEW -d "https://timbolu.com/tasks/<task_id>"
   # iOS, skema custom di simulator:
   xcrun simctl openurl booted "bolu://task/<task_id>"
   ```
   Harapan: app terbuka pada detail tugas dengan id yang sama. Universal link di iOS diuji di
   perangkat, karena simulator tidak membuktikan asosiasi domain.
5. **Belum login.** Keluar dari akun, ketuk link yang sama, login. Harapan: mendarat di layar yang
   dituju, bukan di dasbor.
6. **Resource hilang.** Ulangi dengan UUID acak. Harapan: layar "tidak ditemukan" dengan jalan
   kembali; app tidak crash dan tidak membuka resource lain.
7. **Lintas workspace.** Pakai id dari workspace lain. Harapan: jawaban yang sama dengan langkah 6
   (404), bukan pesan "terlarang" yang membocorkan keberadaan resource.
8. **Draf yang sudah diputuskan.** Ketuk notifikasi `draft.created` setelah draf disetujui dari
   perangkat lain. Harapan: inbox membuka draf itu dan menampilkan keputusan yang berlaku, bukan
   error.

Bukti yang dilampirkan saat menutup butir 6: rekaman layar untuk langkah 4–8, keluaran `curl -D-`
untuk langkah 1–2, keluaran `pm get-app-links` untuk langkah 3, plus model perangkat, versi OS, dan
nomor build app.

## Rujukan

- `docs/api.md` — § The stream, § Approval and drafts, tabel error, § Routines and scheduling.
- `packages/contracts/events.ts` — nama event dan payload `draft.created`/`draft.decided`.
- `apps/backend/internal/modules/chat/handler/handler.go` — envelope event di wire.
- `apps/backend/internal/modules/task/activity/activity.go` dan
  `apps/backend-js/src/modules/task/activity/activity.ts` — payload `task.*` dari worker.
- `apps/backend/internal/modules/task/service/wire.go` — `PayloadShape`, termasuk `waiting_draft_id`.
- `apps/backend/internal/modules/approval/service/service.go` — envelope dan payload `draft.*`.
- `apps/frontend/src/app/(workspace)/**/page.tsx` — route yang diklaim universal link.
- `apps/frontend/src/components/chat/chat-view.tsx` — `?agent=` dan `?conversation=`.
- `apps/frontend/src/components/dashboard/task-detail-view.tsx` — perilaku halaman tugas.
- `apps/frontend/src/components/dashboard/workspace-data.tsx` dan
  `apps/frontend/src/components/auth/credentials-form.tsx` — perilaku sesi habis dan login.
