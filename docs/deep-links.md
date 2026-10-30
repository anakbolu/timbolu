# Deep link

Kontrak ini menjawab satu pertanyaan: kalau sebuah notifikasi diketuk, layar mana yang harus
terbuka, dan dengan id apa. Tanpa kontrak, app Expo dan web akan menebak URL sendiri-sendiri, dan
notifikasi yang membuka layar salah lebih buruk daripada notifikasi yang tidak datang: orang
mengira pekerjaannya hilang.

Dokumen ini **kontrak, bukan implementasi**. Belum ada app Expo di repo ini, `notification.pending`
belum punya produsen, dan `routine.run` baru ada sebagai nama. Yang ditetapkan di sini adalah bentuk
yang harus dipenuhi saat bagian-bagian itu dibangun. Route yang disebut bisa dicek di
`apps/frontend/src/app`, tetapi daftar nama `page.tsx` saja tidak cukup: berkasnya harus dibaca.
Sebuah `page.tsx` boleh hanya berisi `redirect()` ke layar lain, dan itu berarti layarnya tidak ada.
Nama event selalu bisa dicek di `packages/contracts/events.ts` dan `docs/api.md` (§ The stream).

Route group `(workspace)` tidak menambah segmen ke URL: `app/(workspace)/tasks/[taskID]/page.tsx`
dilayani sebagai `/tasks/<task_id>`. Itu sebabnya tabel di bawah memakai path publik, bukan path
berkas.

## Skema `bolu://`

| Link | `<id>` | Layar yang dibuka | Padanan web hari ini |
| --- | --- | --- | --- |
| `bolu://task/<task_id>` | `tasks.id` (UUID) | Detail tugas: keadaan, alasan berhenti, langkah, hasil serahan | `/tasks/<task_id>` |
| `bolu://chat/<conversation_id>` | `conversations.id` (UUID) | Obrolan itu: 1:1 dengan Bolu, grup, atau thread | `/chat?conversation=<conversation_id>` |
| `bolu://approval/<draft_id>` | `drafts.id` (UUID) | Inbox persetujuan, dibuka pada draf itu (Fase 1) | **belum ada** — lihat [Celah](#celah) |

Aturan yang mengikat ketiganya:

1. **`<id>` wajib UUID, tanpa query, fragment, atau token.** Id yang bukan UUID ditolak sebelum
   layar dibuka; app tidak boleh mengarang id dari string lain.
2. **Link tidak membawa workspace.** App membuka link di workspace aktif, bukan berpindah workspace
   karena link. Setiap read di API di-scope tenant, dan id milik workspace lain dijawab
   `task_not_found` / `conversation_not_found` / `draft_not_found` (404) — bukan 403. Jadi link
   lintas-workspace tidak punya arti yang bisa dihormati.
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
| Draf yang sudah diputuskan | `draft_already_decided` (409) | Tetap buka drafnya dan tampilkan keputusan yang sudah berlaku; ini bukan link rusak |

## Universal link

Universal link adalah jalur yang dipakai notifikasi. Bentuknya `https://timbolu.com/<path>`, host
yang sama dengan web hari ini (`apps/frontend` dilayani di `timbolu.com`, dan API-nya di host yang
sama — lihat `docs/deployment.md`). Pemetaannya diturunkan dari route yang benar-benar ada:

| Universal link | Layar app | Catatan |
| --- | --- | --- |
| `/dashboard` | Dasbor | Daftar keputusan yang menunggu, tanpa fokus ke satu draf |
| `/chat?agent=<agent_id>` | Obrolan 1:1 | Bentuk link yang ditulis sidebar untuk thread langsung |
| `/chat?conversation=<conversation_id>` | Obrolan | Grup dan thread; ini yang dipakai `bolu://chat/<id>` |
| `/tasks` | Dasbor | Layar daftar tugas sudah dihapus; `/tasks` hanya `redirect("/dashboard")`, jadi path ini tidak punya layar app sendiri |
| `/tasks/<task_id>` | Detail tugas | Tujuan `bolu://task/<id>` |
| `/routines` | Rutinitas | Daftar dan riwayat; tidak ada halaman per-run |
| `/groups` | Grup | |
| `/bolu/new` | Bolu baru | |
| `/dashboard/integrations` | Integrasi | |
| `/settings` | Pengaturan | |
| `/settings/team` | Anggota ruang kerja | |
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

## Celah

Bagian ini mencatat target yang **belum punya layar**, supaya tidak ada URL karangan di dokumen ini
atau di app nanti.

### Layar web yang belum ada

- **`/approval/<draft_id>` tidak ada.** Web hari ini punya tiga tempat yang mendekati, dan ketiganya
  bukan layar draf: panel "Menunggu persetujuanmu" di `/dashboard` (tombol setujui ada di sana,
  tetapi drafnya tidak bisa dialamatkan), tombol "Lihat drafnya" di `/tasks/<task_id>` yang justru menuju
  `/dashboard`, dan blok `draft` di dalam thread `/chat?conversation=<id>`. Karena itu
  `bolu://approval/<draft_id>` sementara hanya bisa dibuka oleh app Fase 1 (inbox persetujuan);
  universal link `https://timbolu.com/approval/<draft_id>` **tidak boleh** diumumkan sampai layar
  itu ada.
- **`/chat/<conversation_id>` tidak ada.** Obrolan dialamatkan lewat query (`?agent=` atau
  `?conversation=`), bukan segmen path.
- **`/routines/<run_id>` tidak ada.** `/routines` adalah daftar dan riwayat; satu run tidak punya
  halaman sendiri.
- **`/drafts/<draft_id>` tidak ada.** `GET /drafts/:id` ada di API, tetapi tidak ada halaman web
  yang menampilkannya.

### Payload event yang belum membawa id yang dibutuhkan

- `task.waiting_approval` dipancarkan dari worker (`apps/backend/internal/modules/task/activity/activity.go`,
  `apps/backend-js/src/modules/task/activity/activity.ts`) dengan payload lima field: `task_id`,
  `agent_id`, `status`, `trigger`, `title`. **`waiting_draft_id` tidak ada di payload itu**, padahal
  field tersebut ada di `GET /tasks/:id` (`PayloadShape.WaitingDraftID`). Sampai emit-nya diperluas,
  worker notifikasi harus membaca tugasnya dulu untuk mendapatkan id draf.
- `routine.run` belum punya produsen: nama itu hanya ada sebagai nama di
  `packages/contracts/events.ts`, `docs/api.md`, `apps/frontend/src/lib/realtime.ts`,
  `apps/frontend/src/lib/dashboard-view.ts`, dan konstanta Go/Bun. Satu run membuka tepat
  satu task (`routine_runs.task_id`), jadi kontraknya memakai `task_id`, tetapi produsennya wajib
  membawa field itu saat event ini benar-benar dipancarkan.
- `notification.pending` juga belum punya produsen; ia hanya konstanta dengan komentar "the hook the
  notification worker consumes". Belum ada payload yang bisa dipakai membangun link.

### Identitas app yang belum diketahui

Apple Team ID, bundle id iOS, package name Android, dan SHA-256 sertifikat Android belum ada di repo
ini (tidak ada proyek Expo, `app.json`, atau `eas.json`). Apple Team ID dan bundle id menunggu akun
Apple Developer; package name dan SHA-256 menunggu Play Console atau keystore rilis. Lihat bagian
berikut.

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
`/tasks/*` tetap ada karena detail tugas punya layar sendiri.

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
| `task.waiting_approval` | `task_id` (envelope), `conversation_id` (envelope); `waiting_draft_id` **tidak** ada di payload worker | `bolu://approval/<draft_id>` kalau id draf bisa dibaca, selain itu `bolu://task/<task_id>` | Notifikasi ini ada supaya orang bisa memutuskan; mendarat di tugas tanpa tombol putus adalah perjalanan yang sia-sia |
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
5. **Web hari ini belum memenuhi aturan ini.** `apps/frontend/src/components/dashboard/workspace-data.tsx`
   mengalihkan ke `/login?sesi=berakhir` tanpa menyimpan tujuan, dan
   `apps/frontend/src/components/auth/credentials-form.tsx` selalu menuju `/dashboard` atau
   `/onboarding`. Artinya universal link yang dibuka saat sesi habis kehilangan tujuannya. Ini celah
   yang harus ditutup sebelum universal link diumumkan, bukan perilaku yang boleh ditiru app.

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
