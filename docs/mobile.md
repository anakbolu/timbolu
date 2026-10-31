# Aplikasi mobile (Expo)

`apps/mobile` adalah klien iOS + Android dari satu codebase: Expo SDK 57,
`expo-router` (file-based routing), TypeScript. Tiga tab yang ada: **Bolu**,
**Persetujuan**, dan **Tugas** (`apps/mobile/app/(tabs)/_layout.tsx`). Alur yang
sudah ada: **masuk → daftar Bolu → satu obrolan**, **kotak masuk persetujuan →
satu draf**, dan **daftar tugas → detail satu tugas**; potongan pertama sesuai
[#487](https://github.com/anakbolu/timbolu/issues/487), kotak masuknya sesuai
[#515](https://github.com/anakbolu/timbolu/issues/515), tab tugas dan layar
detailnya sesuai [#523](https://github.com/anakbolu/timbolu/pull/523).

## Menjalankan di Expo Go

Tidak perlu akun EAS dan tidak perlu build native: semua modul yang dipakai
(`expo-router`, `expo-secure-store`, `expo-status-bar`) ada di Expo Go SDK 57.

```bash
cd apps/mobile
bun install
npx expo start
```

Lalu pindai QR yang muncul dengan aplikasi Expo Go di HP (satu jaringan Wi-Fi
dengan mesin ini). `a` membuka emulator Android, `i` membuka simulator iOS kalau
Xcode terpasang.

Perintah lain:

```bash
npx tsc --noEmit   # typecheck; hijau adalah syarat PR
npm run export:ios # membentuk bundle iOS; membuktikan babel.config.js, plugin, dan app.json masih bisa dipakai
npx expo-doctor    # memeriksa versi paket terhadap SDK
```

`babel-preset-expo` ditulis eksplisit di `devDependencies` walaupun `expo` sudah
bergantung padanya: npm bisa memasang salinannya di bawah
`node_modules/expo/node_modules/` alih-alih di root,
sementara Babel menyelesaikan nama preset dari `apps/mobile/babel.config.js` —
tanpa deklarasi sendiri, `npx expo export` berhenti dengan `Cannot find module
'babel-preset-expo'` (#545). Versinya `~57.0.16`, lini yang sama dengan
`expo ~57.0.31`; `babel-preset-expo` 58.x sejalan dengan SDK 58, bukan SDK ini.

`bun install` menulis lockfile, dan lockfile-nya sengaja tidak masuk repo
(lihat `.gitignore`) supaya diff scaffold ini bisa dibaca. Setiap orang yang
memasang akan mendapat resolusi baru; itu diterima sampai ada PR yang memang
menambah/mengunci dependensi.

## Menunjuk ke produksi atau lokal

Base URL dibaca dari `EXPO_PUBLIC_API_URL` saat bundling (`src/lib/api.ts`),
default `https://timbolu.com`:

```bash
# produksi (default)
npx expo start

# API lokal
EXPO_PUBLIC_API_URL=http://192.168.1.10:8080 npx expo start
```

`localhost` tidak bisa dipakai dari HP — itu loopback HP itu sendiri. Pakai alamat
LAN mesin ini, atau tunnel (`npx expo start --tunnel`) untuk jaringan yang
memisahkan keduanya. Untuk Android emulator, `http://10.0.2.2:8080` menunjuk ke
host.

## Auth: kenapa begini

- **Access token di memori saja.** Ia berumur pendek; salinan di disk akan hidup
  lebih lama daripada sesinya.
- **Refresh token di `expo-secure-store`** (keystore platform), karena ia harus
  bertahan saat app dimatikan — kalau tidak, setiap cold start jadi login ulang.
- **Refresh lewat body, tanpa cookie.** `POST /auth/refresh {"token": …}`
  menjawab `refresh_token` baru di body (rotasi), jadi app tidak butuh cookie jar
  — lihat [api.md](api.md#authentication). Token yang dipresentasikan langsung
  dicabut, dan klien menyimpan hasil rotasinya.
- **Satu refresh dalam satu waktu.** Token yang dipakai dicabut, jadi dua refresh
  bersamaan akan saling mencabut; `refreshSession` membagi satu percobaan ke
  semua pemanggil.
- **401 → satu refresh → ulangi sekali.** Access token 15 menit sementara HP
  menganggur lebih lama, jadi `authorizedFetch` memperbarui sebelum panggilan
  yang tokennya sudah habis, dan sekali lagi saat API menolak.

`POST /auth/login` membalas sesi dengan refresh token di **cookie** `httpOnly`
(perilaku untuk browser), bukan di body. Expo Go tidak menyediakan cookie jar
yang bisa dibaca, jadi klien ini mengambil nilainya dari header `Set-Cookie`
respons login. Kalau platform menyembunyikan header itu, sesi tetap berjalan
sampai access token pertama habis, lalu app meminta login lagi pada cold start
berikutnya — bukan kehilangan data.

Workspace dipilih otomatis: `GET /workspaces`, lalu yang pertama dipakai untuk
header `X-Workspace-Id`. Scaffold ini tidak punya storage selain keystore, jadi
pilihan workspace tidak diingat antar-jalan; begitu pemilih workspace masuk, itu
tempat menyimpannya.

## Link masuk

Skema `bolu` dan host universal link `timbolu.com` dideklarasikan di `apps/mobile/app.json`
(`scheme`, `ios.associatedDomains`, `android.intentFilters`); kontrak lengkapnya ada di
[deep-links.md](deep-links.md) § Skema `bolu://` dan § Universal link.

App membaca link `bolu://` sejak #500 dan `https://timbolu.com/...` sejak #540. Router tidak dipakai
untuk memetakannya: `bolu://chat/<id obrolan>` akan cocok dengan `chat/[agentID]`, yang mengharapkan
id Bolu, dan akan membuka thread yang tidak disebut link. Karena itu
`app/+native-intent.tsx` (`redirectSystemPath`) mengembalikan `null` untuk URL `bolu://` dan untuk
URL https yang diklaim app — router tetap di tempatnya — dan `app/_layout.tsx` yang memutuskan
tujuan sebenarnya.

Alurnya:

1. `app/_layout.tsx` (`IncomingLinks`) membaca URL peluncuran lewat
   `Linking.getInitialURL()` **sebelum** sesi diketahui — notifikasi paling
   sering membuka app yang sudah mati — lalu URL berikutnya lewat
   `Linking.addEventListener("url", …)`. Yang diambil adalah `bolu://` dan URL
   `https://timbolu.com/...` yang path-nya punya tujuan di app; URL lain tetap
   ditangani router.
2. `src/lib/deep-link.ts` (`parseIncomingLink`) menerjemahkan URL — `bolu://`
   maupun `https://timbolu.com/...` — menjadi target
   `chat`/`task`/`approval`/`routine`, atau `fallback` dengan alasan
   `unknown-path`, `invalid-id`, atau `not-a-link`. Id wajib UUID; query dan
   fragment diabaikan, segmen path tambahan tidak.
3. `src/lib/pending-link.ts` menyimpan target itu. Link yang datang saat belum
   masuk tidak dibuang: `_layout.tsx` menunggu status `signed-in`, baru memanggil
   `takePendingLink()` (sekali pakai) dan `router.replace(...)`.
4. `src/lib/link-destination.ts` (`destinationFor`) menentukan tujuan dan
   kalimatnya. Target `task`, `approval`, dan `routine` menjadi tujuan ber-id —
   `{ kind: "task", taskID, notice: null }`, `{ kind: "draft", draftID, notice:
   null }`, dan `{ kind: "routine", routineID, notice: null }` — lalu dibuka
   sebagai `/task/[taskID]`, `/draft/[draftID]`, dan `/routine/[routineID]`.
   Target `chat` dan `fallback` menjadi daftar Bolu dengan kalimat.
   `setLinkNotice` memasang kalimat itu — `null` menghapus banner yang
   ditinggalkan link sebelumnya — dan `app/(tabs)/bolu.tsx` menampilkannya
   sebagai banner yang bisa ditutup.

Hari ini target `task`, `approval`, dan `routine` dibuka ke layarnya sendiri —
`/task/[taskID]`, `/draft/[draftID]`, dan `/routine/[routineID]` — tanpa banner
karena layarnya yang menjelaskan keadaan resource. Dua target lain berakhir di
daftar Bolu `/(tabs)/bolu` dengan pemberitahuan:

| Link | Target `parseIncomingLink` | Yang terjadi sekarang |
| --- | --- | --- |
| `bolu://chat/<conversation_id>` | `{ kind: "chat" }` | daftar Bolu + "Layar yang dituju tautan ini belum ada di app. …" — `chat/[agentID]` butuh id Bolu, bukan id obrolan, jadi tidak dibuka |
| `bolu://task/<task_id>` | `{ kind: "task" }` | layar `/task/[taskID]` — `destinationFor` mengembalikan `{ kind: "task", taskID, notice: null }`, dan `app/_layout.tsx` memanggil `router.replace({ pathname: "/task/[taskID]", params: { taskID } })` |
| `bolu://approval/<draft_id>` | `{ kind: "approval" }` | layar `/draft/[draftID]` — `destinationFor` mengembalikan `{ kind: "draft", draftID, notice: null }`, dan `app/_layout.tsx` memanggil `router.replace({ pathname: "/draft/[draftID]", params: { draftID } })` |
| `bolu://routine/<routine_id>` | `{ kind: "routine" }` | layar `/routine/[routineID]` — `destinationFor` mengembalikan `{ kind: "routine", routineID, notice: null }`, dan `app/_layout.tsx` memanggil `router.replace({ pathname: "/routine/[routineID]", params: { routineID } })` |
| `bolu://` dengan path lain, segmen ekstra, atau id bukan UUID | `{ kind: "fallback" }` | daftar Bolu + "Tautan ini tidak dikenal. Pilih Bolu di bawah untuk melanjutkan." |

`app/+not-found.tsx` adalah layar cadangan expo-router untuk path yang tidak
punya berkas layar (URL di luar `bolu://` dan `https://timbolu.com/...` yang
diklaim app): isinya `UNKNOWN_LINK_NOTICE`
("Tautan ini tidak dikenal.") dan tombol "Kembali ke daftar Bolu". Target
`bolu://` yang dikenali tidak diarahkan ke sana: `destinationFor` membuka layar
ber-id untuk target `task`, `approval`, dan `routine`, serta daftar Bolu untuk
target `chat` dan `fallback`.

`docs/deep-links.md` § Skema `bolu://` adalah kontraknya; tabel di atas
mencatat apa yang app lakukan sekarang, bukan apa yang seharusnya.

## Obrolan hidup dari `/api/events/stream`

Layar chat membuka **satu** koneksi SSE ke `/api/events/stream`
(`src/lib/chat-stream.ts`) selama layar itu yang tampil, memakai `fetch` streaming
dari `expo/fetch` — modulnya ikut Expo, jadi tidak ada dependensi baru. Token dan
workspace ikut di query string karena route stream memang mengautentikasi dari
sana (`requireStreamAuth`), bukan dari header.

- **Satu koneksi, bukan dua.** Koneksinya diikat ke fokus layar
  (`useFocusEffect`), jadi cleanup-nya berjalan saat layar blur maupun unmount:
  layar yang tertutup di belakang layar lain berhenti mendengarkan, dan kembali
  menyambung saat dibuka lagi.
- **Frame diparse apa adanya.** `id:`, `event:`, dan `data:` dibaca per blok;
  baris komentar (`: connected`, `: keep-alive`) dilewati, dan satu frame yang
  terpotong antar-chunk disatukan dulu sebelum diurai. Hanya `message.new` dan
  `message.updated` untuk percakapan yang sedang terbuka yang masuk ke daftar —
  pesan baru ditambahkan di ujung terbaru, pembaruan placeholder menggantikan
  pesan yang sudah ada di tempatnya, tanpa reload penuh.
- **Cursor di memori.** Setiap id yang maju disimpan di ref layar dan dikirim
  sebagai `last_event_id` saat koneksi dibuka lagi, jadi reconnect tidak
  melewatkan pesan dan tidak memutar ulang seluruh riwayat workspace.
- **Latar berhenti, depan menyambung langsung.** Socket tidak bertahan saat app
  masuk latar — iOS dan Android memutusnya dalam beberapa detik — jadi stream
  membaca `AppState`: `background`/`inactive` menutup koneksi dan menghentikan
  loop reconnect, karena percobaan di sana hanya menghabiskan baterai dan kuota
  untuk layar yang tidak dilihat siapa pun. Kursor `last_event_id` tetap di
  memori, dan saat app kembali `active` koneksi baru dibuka segera — bukan
  menunggu backoff 30 detik, sebab tidur bukan tanda route sedang sakit — dengan
  cursor yang sama supaya server memutar ulang frame yang terlewat. Karena
  server boleh membatasi replay dan cursor bisa tertinggal, layar juga membaca
  ulang satu halaman pesan saat kembali aktif; fokus layar tetap pemilik tunggal
  koneksi, jadi app di depan dan layar di fokus tidak pernah membuka dua.
- **Stream mati bukan jalan buntu.** Sebuah koneksi baru dihitung sehat setelah
  bertahan 5 detik; yang gagal lebih cepat menaikkan hitungan gagal dan
  dicoba ulang dengan jeda 0,5 → 1 → 2 → 4 → 8 detik, lalu 30 detik plus jitter.
  Tidak ada polling: satu pembacaan ulang 5 detik setelah kirim, satu halaman
  saat app kembali aktif, dan tombol "Muat ulang" tetap bekerja walau stream
  tidak pernah tersambung. Tarik-untuk-menyegarkan ada di daftar Bolu, kotak
  masuk Persetujuan, dan daftar Tugas, belum di layar chat.

`/events/socket` tidak dipakai app ini. Kontrak kedua transport, status klien,
dan arti setiap frame ada di [realtime.md](realtime.md) dan
[api.md](api.md#the-stream).

## Persetujuan

Tab kedua, **Persetujuan**, adalah kotak masuk draf yang menunggu keputusan:
`GET /drafts?status=pending&limit=50` (terbaru dulu) lewat `listDrafts` di
`api.ts`, dan tiap baris menampilkan jenis aksi, judul, ringkasan, serta umur
drafnya. Menekan satu baris membuka `app/draft/[draftID].tsx`, yang membaca
`GET /drafts/:id` dan menampilkan judul, ringkasan, isi `payload` (`key: value`
per baris), serta `blocked_reason`/`not_before` bila aksinya ditahan jam kerja.
Keputusannya memakai `POST /drafts/:id/{approve,revise,cancel}`
(`approveDraft`, `rejectDraft`, `cancelDraft`): **Tolak** wajib beralasan
(tombolnya nonaktif sampai isian tidak kosong, karena Bolu tanpa catatan akan
menulis draf yang sama lagi), **Batalkan** minta konfirmasi singkat, dan jawaban
`draft_already_decided` (409) tampil sebagai kalimat lalu kembali ke daftar —
bukan sebagai galat mentah. Daftar dibaca ulang setiap kali tab difokuskan, dan
badge angka di tab memakai `meta.count` dari respons yang sama (tanpa endpoint
hitungan baru dan tanpa polling). Layar ini **belum dilihat di HP atau
simulator**: yang terbukti baru `npx tsc --noEmit` dan `npx expo export
--platform ios`.

## Tugas

Tab ketiga, **Tugas**, adalah riwayat tugas ruang kerja:
`GET /tasks?limit=50` lewat `listTasks` (`api.ts`), terbaru dulu, dibaca ulang
setiap kali tab difokuskan dan bisa ditarik untuk disegarkan. Tiap baris
menampilkan judul (atau "Tugas" bila kosong), umur, label keadaan dari
`taskState` (`src/lib/task-view.ts`), dan nama Bolu pemiliknya. Daftar Bolu
untuk nama itu dibaca terpisah lewat `listAgents`, jadi kegagalannya tidak
menyembunyikan tugas: baris tanpa nama tetap tampil. Tab ini tidak memulai
tugas — API tidak punya route untuk itu (`api.md` § Task runtime).

Menekan satu baris membuka `app/task/[taskID].tsx`, yang membaca `GET /tasks/:id`
dan `GET /tasks/:id/steps` (`getTask`, `listTaskSteps`) serta `listAgents` untuk
nama Bolu. Layar itu menampilkan keadaan, alasan berhenti, alasan menunggu
(hanya selama tugas masih hidup), ringkasan, jumlah langkah, waktu selesai,
tombol "Buka hasil" untuk setiap rujukan konten (`${API_URL}/content/<ref>`),
dan daftar langkah: urutan, jenis, nama tool, potongan `output` (atau `input`
bila `output` belum ada), dan waktu. Langkah yang dipangkas kebijakan retensi
disebut apa adanya, bukan digambar kosong; langkah yang gagal dimuat tidak
menyembunyikan keadaan tugas, dan punya tombol "Muat ulang langkah". Id yang
tidak ada dan id yang bukan UUID sama-sama berakhir di "Tugas tidak ditemukan."
dengan tombol "Kembali ke daftar tugas".

## Yang belum ada

Semua ini sengaja di luar potongan pertama:

- **Push notification** dan registrasi `POST /devices` (endpoint-nya sudah ada).
- **Universal link yang hidup** — app sudah mendeklarasikan `timbolu.com` dan
  membaca URL `https://timbolu.com/...` sejak #540, tetapi kedua berkas
  well-known (`apple-app-site-association`, `assetlinks.json`) belum disajikan
  di prod dan identitas app belum ada di repo. Selama itu mengetuk link masih
  membuka browser, dan link belum pernah dibuktikan di perangkat. Bentuk dan
  syaratnya ada di [deep-links.md](deep-links.md) § Universal link.
- **Layar untuk target `bolu://`** — `chat/<conversation_id>` masih belum bisa
  dibedakan dari `chat/<id Bolu>`, jadi tautan obrolan tetap mendarat di daftar
  Bolu dengan pemberitahuan (§ Link masuk `bolu://`). `task`, `approval`, dan
  `routine` sudah punya layar dan tautannya sudah dipetakan ke sana:
  `/task/[taskID]`, `/draft/[draftID]`, dan `/routine/[routineID]`.
- **WebSocket `/events/socket`** dan status transport di layar. App memakai SSE
  (`/events/stream`) — lihat bagian di atas.
- **Offline cache** dan antrean kirim.
- **Adapter `react-native-svg`** untuk `packages/bolu-character`: sekarang
  karakter digambar dari warna dan bentuk di `src/lib/bolu.ts` +
  `src/components/bolu-face.tsx` (tabel radius per bentuk, warna dari registry),
  bukan dari geometri `characters.ts`. Konstanta palet disalin, bukan diimpor,
  karena paket itu berada di luar root Metro dan mengimpornya butuh
  `metro.config.js` monorepo — paketnya sendiri tidak diubah.
- **Blok pesan selain `text`** (tabel, draft, chart, mermaid, html): dihitung dan
  disebutkan jumlahnya, belum dirender.
- **Pemilih workspace**, pengaturan, dan tab lain di luar `Bolu`,
  `Persetujuan`, dan `Tugas`.
- **Tab bar berikon** — scaffold ini tidak menambah paket ikon.
