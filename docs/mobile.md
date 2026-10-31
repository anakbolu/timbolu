# Aplikasi mobile (Expo)

`apps/mobile` adalah klien iOS + Android dari satu codebase: Expo SDK 57,
`expo-router` (file-based routing), TypeScript. Potongan pertama ini hanya
menyediakan alur **masuk → daftar Bolu → satu obrolan**, sesuai
[#487](https://github.com/anakbolu/timbolu/issues/487).

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
npx expo-doctor    # memeriksa versi paket terhadap SDK
```

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

## Link masuk `bolu://`

App membaca link `bolu://` sejak #500. Router tidak dipakai untuk memetakannya:
`bolu://chat/<id obrolan>` akan cocok dengan `chat/[agentID]`, yang mengharapkan
id Bolu, dan akan membuka thread yang tidak disebut link. Karena itu
`app/+native-intent.tsx` (`redirectSystemPath`) mengembalikan `null` untuk URL
`bolu://` — router tetap di tempatnya — dan `app/_layout.tsx` yang memutuskan
tujuan sebenarnya.

Alurnya:

1. `app/_layout.tsx` (`IncomingLinks`) membaca URL peluncuran lewat
   `Linking.getInitialURL()` **sebelum** sesi diketahui — notifikasi paling
   sering membuka app yang sudah mati — lalu URL berikutnya lewat
   `Linking.addEventListener("url", …)`. Hanya `bolu://` yang diambil; URL lain
   tetap ditangani router.
2. `src/lib/deep-link.ts` (`parseIncomingLink`) menerjemahkan URL menjadi target
   `chat`/`task`/`approval`/`routine`, atau `fallback` dengan alasan
   `unknown-path`, `invalid-id`, atau `not-a-link`. Id wajib UUID; query dan
   fragment diabaikan, segmen path tambahan tidak.
3. `src/lib/pending-link.ts` menyimpan target itu. Link yang datang saat belum
   masuk tidak dibuang: `_layout.tsx` menunggu status `signed-in`, baru memanggil
   `takePendingLink()` (sekali pakai) dan `router.replace(...)`.
4. `src/lib/link-destination.ts` (`destinationFor`) menentukan tujuan dan
   kalimatnya; `setLinkNotice` memasang kalimat itu dan
   `app/(tabs)/bolu.tsx` menampilkannya sebagai banner yang bisa ditutup.

Hari ini **setiap** target berakhir di daftar Bolu `/(tabs)/bolu` dengan
pemberitahuan, karena layarnya belum ada di app:

| Link | Target `parseIncomingLink` | Yang terjadi sekarang |
| --- | --- | --- |
| `bolu://chat/<conversation_id>` | `{ kind: "chat" }` | daftar Bolu + "Layar yang dituju tautan ini belum ada di app. …" — `chat/[agentID]` butuh id Bolu, bukan id obrolan, jadi tidak dibuka |
| `bolu://task/<task_id>` | `{ kind: "task" }` | daftar Bolu + kalimat yang sama; belum ada layar detail tugas |
| `bolu://approval/<draft_id>` | `{ kind: "approval" }` | daftar Bolu + kalimat yang sama; belum ada layar draf |
| `bolu://routine/<routine_id>` | `{ kind: "routine" }` | daftar Bolu + kalimat yang sama; belum ada layar rutinitas |
| `bolu://` dengan path lain, segmen ekstra, atau id bukan UUID | `{ kind: "fallback" }` | daftar Bolu + "Tautan ini tidak dikenal. Pilih Bolu di bawah untuk melanjutkan." |

`app/+not-found.tsx` adalah layar cadangan expo-router untuk path yang tidak
punya berkas layar (URL non-`bolu://`): isinya `UNKNOWN_LINK_NOTICE`
("Tautan ini tidak dikenal.") dan tombol "Kembali ke daftar Bolu". Target
`bolu://` yang dikenali tidak diarahkan ke sana — `destinationFor` selalu
mengembalikan `/(tabs)/bolu`.

`docs/deep-links.md` § Skema `bolu://` adalah kontraknya; tabel di atas
mencatat apa yang app lakukan sekarang, bukan apa yang seharusnya.

## Yang belum ada

Semua ini sengaja di luar potongan pertama:

- **Push notification** dan registrasi `POST /devices` (endpoint-nya sudah ada).
- **Universal link** `timbolu.com/…` — app hanya membaca skema `bolu://`, dan
  `app.json` belum mendeklarasikan associated domains; bentuk dan syaratnya ada
  di [deep-links.md](deep-links.md) § Universal link.
- **Layar untuk target `bolu://`** — `task`, `approval`, `routine`, dan
  `chat/<conversation_id>` belum punya layar, jadi semuanya berakhir di daftar
  Bolu dengan pemberitahuan (§ Link masuk `bolu://`).
- **Streaming SSE/WebSocket** (`/events/stream`, `/events/socket`): balasan Bolu
  muncul lewat satu pembacaan ulang 5 detik setelah kirim, plus tombol "Muat
  ulang" dan tarik-untuk-menyegarkan.
- **Offline cache** dan antrean kirim.
- **Adapter `react-native-svg`** untuk `packages/bolu-character`: sekarang
  karakter digambar dari warna dan bentuk di `src/lib/bolu.ts` +
  `src/components/bolu-face.tsx` (tabel radius per bentuk, warna dari registry),
  bukan dari geometri `characters.ts`. Konstanta palet disalin, bukan diimpor,
  karena paket itu berada di luar root Metro dan mengimpornya butuh
  `metro.config.js` monorepo — paketnya sendiri tidak diubah.
- **Blok pesan selain `text`** (tabel, draft, chart, mermaid, html): dihitung dan
  disebutkan jumlahnya, belum dirender.
- **Pemilih workspace**, pengaturan, dan layar lain di tab selain `Bolu`.
- **Tab bar berikon** — scaffold ini tidak menambah paket ikon.
