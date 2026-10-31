# Deep link

Kontrak ini menjawab satu pertanyaan: kalau sebuah notifikasi diketuk, layar mana yang harus
terbuka, dan dengan id apa. Tanpa kontrak, app Expo dan web akan menebak URL sendiri-sendiri, dan
notifikasi yang membuka layar salah lebih buruk daripada notifikasi yang tidak datang: orang
mengira pekerjaannya hilang.

Dokumen ini **kontrak, bukan implementasi**. App Expo-nya sudah ada sejak #488: `apps/mobile`
memakai Expo SDK 57 dan `expo-router`, dengan layar `index` (gerbang sesi), `login`, `(tabs)/bolu`,
dan `chat/[agentID]`; detailnya di `docs/mobile.md`. `scheme` `bolu` juga sudah dideklarasikan di
`apps/mobile/app.json`. Yang belum ada adalah penanganan link masuk: tidak ada `+not-found` di
`apps/mobile/app` dan tidak ada listener `Linking`/`useURL` yang memetakan URL masuk ke layar, jadi
hari ini app tidak membuka link apa pun. `notification.pending` juga belum punya produsen, dan
`routine.run` baru ada sebagai nama. Yang ditetapkan di sini adalah bentuk yang harus dipenuhi saat
bagian-bagian itu dibangun. Nama event selalu bisa dicek di `packages/contracts/events.ts` dan
`docs/api.md` (§ The stream).

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

`/tasks/*`, `/routines/*`, dan `/settings/*` di berkas well-known adalah glob dari baris
`/tasks/<task_id>`, baris `/routines/<routine_id>`, dan baris-baris `/settings/...`. `/routines/*`
mencakup kedua bentuk `/routines/...` karena keduanya dilayani satu berkas dinamis yang sama, jadi
id run pun sampai ke app dan berakhir di layar "tidak ditemukan". Tidak ada path lain di bawah
ketiganya yang dilayani.

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
dibuka" (§ Skema `bolu://`); aturan di atas adalah sisi web-nya, dan app memakai kalimat yang sama.

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
- `app/login.tsx` form masuk; `app/(tabs)/bolu.tsx` daftar Bolu; `app/chat/[agentID].tsx` satu obrolan.
- Token: refresh token disimpan di `expo-secure-store` (`apps/mobile/src/lib/tokens.ts`), access token hanya di memori, dan refresh dikirim lewat body `POST /auth/refresh` tanpa cookie (`apps/mobile/src/lib/api.ts`, `refreshSession`).
- Gate CI: job `mobile` di `.github/workflows/checks.yml` menjalankan `make typecheck-mobile` (#493).
- Belum: penanganan link masuk. Tidak ada `+not-found` di `apps/mobile/app`, dan tidak ada listener `Linking`/`useURL` yang memetakan URL ke layar; satu-satunya pemakaian `expo-linking` adalah `openURL` ke `/onboarding` di `app/(tabs)/bolu.tsx`.
- Belum: push notification dan registrasi `POST /devices`; endpoint-nya ada, kliennya belum.
- Belum: streaming SSE/WebSocket (`/events/stream`, `/events/socket`); obrolan memuat ulang setelah kirim (`docs/mobile.md`).

## Skema `bolu://`

| Link | `<id>` | Layar yang dibuka | Padanan web hari ini |
| --- | --- | --- | --- |
| `bolu://task/<task_id>` | `tasks.id` (UUID) | Detail tugas: keadaan, alasan berhenti, langkah, hasil serahan | `/tasks/<task_id>` |
| `bolu://chat/<conversation_id>` | `conversations.id` (UUID) | Obrolan itu: 1:1 dengan Bolu, grup, atau thread | `/chat?conversation=<conversation_id>` |
| `bolu://approval/<draft_id>` | `drafts.id` (UUID) | Inbox persetujuan, dibuka pada draf itu (Fase 1) | `/approval/<draft_id>` |
| `bolu://routine/<routine_id>` | `routines.id` (UUID) | Daftar rutinitas, dibuka pada rutin itu | `/routines/<routine_id>` |

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
5. **Path yang tidak dikenal jatuh ke dasbor**, bukan ke layar error. Link lama yang pathnya sudah
   tidak ada tidak boleh membuat app berhenti.

Perilaku per resource saat resource tidak bisa dibuka:

| Resource | Jawaban API | Yang dilakukan app |
| --- | --- | --- |
| Tugas | `task_not_found` (404) | Layar "tugas tidak ditemukan" dengan tombol ke Dasbor; tidak membuka tugas lain dan tidak crash |
| Obrolan | `conversation_not_found` (404) | `/chat` tanpa parameter, dengan pemberitahuan bahwa obrolannya tidak ada |
| Draf | `draft_not_found` (404) | Inbox persetujuan dengan pemberitahuan bahwa drafnya tidak ada |
| Rutinitas | `routine_not_found` (404) | `/routines` tanpa fokus, dengan pemberitahuan bahwa rutinnya tidak ada |
| Draf yang sudah diputuskan | `draft_already_decided` (409) | Tetap buka drafnya dan tampilkan keputusan yang sudah berlaku; ini bukan link rusak |

## Universal link

Universal link adalah jalur yang dipakai notifikasi. Bentuknya `https://timbolu.com/<path>`, host
yang sama dengan web hari ini (`apps/frontend` dilayani di `timbolu.com`, dan API-nya di host yang
sama — lihat `docs/deployment.md`). Pemetaannya diturunkan dari route yang benar-benar ada:

App hari ini belum memetakan link apa pun, baik `bolu://` maupun universal link. Selama penanganan
link masuk belum ditulis, yang berlaku bila sebuah link membuka app adalah perilaku bawaan
`expo-router`: URL dipetakan ke path berkas layar (`/chat/<id>` ke `app/chat/[agentID].tsx`, dan
seterusnya), tanpa penerjemahan ke tabel di bawah dan tanpa penanganan link tertunda. Path yang tidak
punya berkas layar tidak punya tujuan, karena repo ini belum punya `+not-found`. Tabel berikut adalah
target yang harus dipenuhi app, bukan deskripsi perilakunya sekarang.

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

Bagian ini mencatat target yang **belum punya layar**, supaya tidak ada URL karangan di dokumen ini
atau di app nanti.

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
  `apps/frontend/src/lib/dashboard-view.ts`, dan konstanta Go/Bun. Satu run membuka tepat
  satu task (`routine_runs.task_id`), jadi kontraknya memakai `task_id`, tetapi produsennya wajib
  membawa field itu saat event ini benar-benar dipancarkan.
- `notification.pending` juga belum punya produsen; ia hanya konstanta dengan komentar "the hook the
  notification worker consumes". Belum ada payload yang bisa dipakai membangun link.

### Identitas app yang belum diketahui

Apple Team ID, bundle id iOS, package name Android, dan SHA-256 sertifikat Android belum ada di repo
ini. `apps/mobile/app.json` sudah ada, tetapi tidak mendeklarasikan `ios.bundleIdentifier` maupun
`android.package`, dan belum ada `eas.json`. Apple Team ID dan bundle id menunggu akun Apple
Developer; package name dan SHA-256 menunggu Play Console atau keystore rilis. Lihat bagian berikut.

## Berkas well-known

Dua berkas ini yang membuat universal link dipercaya platform. Keduanya **belum dibuat** oleh
dokumen ini; yang ditetapkan hanya bentuk dan syarat penyajiannya.

### `/.well-known/apple-app-site-association`

Tanpa ekstensi, satu objek JSON, tanpa signature (iOS 9 ke atas tidak meminta signature):

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
          { "/": "/routines" },
          { "/": "/routines/*" },
          { "/": "/groups" },
          { "/": "/bolu/new" },
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
bukan sertifikat debug, dan bukan sertifikat upload jika Play App Signing memakai kunci lain.

### Syarat penyajian

- URL: `https://timbolu.com/.well-known/apple-app-site-association` dan
  `https://timbolu.com/.well-known/assetlinks.json`.
- **`200` langsung, `Content-Type: application/json`, tanpa redirect.** Apple dan Google tidak
  mengikuti `301`/`302` untuk berkas ini. Tanpa autentikasi, tanpa cookie, tanpa query.
- Berkas AASA tidak punya ekstensi. Kalau ditaruh di `public/`, Next bisa melayaninya sebagai
  `application/octet-stream`; itu gagal. Pakai route handler atau header eksplisit yang memaksa
  `application/json`.
- Apple mengambil AASA lewat CDN-nya sendiri, jadi perubahan tidak langsung terlihat. Saat menguji,
  jangan menyimpulkan "salah" hanya dari satu percobaan setelah deploy.

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
| `routine.run` | belum ada produsen; run menyimpan `task_id` (`routine_runs.task_id`) | `bolu://task/<task_id>` | Satu run adalah satu tugas, dan hasilnya adalah ringkasan tugas itu; tidak ada halaman per-run untuk dibuka |
| `notification.pending` | belum ada produsen; hanya konstanta "hook the notification worker consumes" | Tidak punya layar sendiri | Ini pintu masuk worker, bukan notifikasi. Worker harus menerjemahkannya ke event di atas dan memakai baris itu; mengirimnya apa adanya berarti mengirim notifikasi tanpa tujuan |

Aturan tambahan:

1. **Link dibangun di server, dari id di baris event.** App tidak merangkai id dari teks notifikasi
   dan tidak menebak dari judul.
2. **Tidak ada notifikasi tanpa tujuan.** Kalau link tidak bisa dibangun (id kosong, event tidak
   dikenal), notifikasi tidak dikirim. Notifikasi yang membuka dasbor "karena tidak tahu" melatih
   orang mengabaikan notifikasi.
3. **`task.finished` dengan `trigger` `routine` tetap memakai layar tugas.** Sampai `routine.run`
   punya produsen, run rutin sudah terlihat sebagai `task.finished` dengan `trigger: "routine"`;
   tidak perlu menunggu event baru untuk bisa membuka layar yang benar.
4. **`bolu://routine/<routine_id>` bukan link notifikasi.** Ia untuk navigasi di dalam app (daftar
   rutinitas ke satu rutin). Notifikasi run tetap membuka tugasnya, karena tugas itulah yang memuat
   hasil run.

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

1. **Berkas well-known.**
   ```bash
   curl -sS -D- -o /dev/null https://timbolu.com/.well-known/apple-app-site-association
   curl -sS -D- -o /dev/null https://timbolu.com/.well-known/assetlinks.json
   ```
   Harapan: `200`, `content-type: application/json`, tanpa `location`.
2. **Apple melihat berkas yang sama.**
   ```bash
   curl -sS https://app-site-association.cdn-apple.com/a/v1/timbolu.com
   ```
   Harapan: JSON yang sama dengan langkah 1. Kalau kosong, tunggu propagasi CDN sebelum menyalahkan
   konfigurasi.
3. **Android sudah memverifikasi domain.**
   ```bash
   adb shell pm get-app-links <package_name>
   ```
   Harapan: `timbolu.com` berstatus `verified`.
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
