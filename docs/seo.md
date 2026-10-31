# SEO

Dokumen ini menjawab empat hal: URL mana yang boleh muncul di hasil pencarian, apa yang sudah
ditegakkan kode, celah yang masih terbuka, dan apa yang harus dikerjakan di luar repo. Setiap klaim
di sini menunjuk berkas di repo atau hasil `GET` ke `https://timbolu.com` pada 31 Oktober 2026.
Dokumen ini tidak memuat angka lalu lintas.

## Apa yang dirayapi dan apa yang tidak

Aturannya satu baris: halaman pemasaran publik boleh masuk indeks; setiap halaman yang butuh sesi
tidak. Berkas relatif terhadap `apps/frontend/src/app`.

| Golongan | Path | Berkas halaman | Dirayapi | Alasan |
| --- | --- | --- | --- | --- |
| Publik | `/` | `page.tsx` | ya | landing pemasaran; isinya sama untuk setiap pengunjung |
| Publik | `/pricing` | `pricing/page.tsx` | ya | daftar harga, dibaca sebelum mendaftar |
| Publik | `/privacy` | `privacy/page.tsx` | ya | kebijakan privasi yang perlu bisa ditemukan |
| Publik | `/terms` | `terms/page.tsx` | ya | syarat layanan |
| Publik | `/contact` | `contact/page.tsx` | ya | jalan menghubungi penjual |
| Aplikasi | `/dashboard`, `/approval`, `/chat`, `/groups`, `/routines`, `/tasks/<task_id>`, `/settings/**`, `/bolu/new` | `(workspace)/**/page.tsx` | tidak | butuh login; isinya data satu workspace, bukan halaman yang dicari orang |
| Aplikasi | `/admin/**` | `admin/page.tsx` (hanya `redirect`), `admin/dashboard/page.tsx`, `admin/users/page.tsx` | tidak | permukaan operator, bukan layar anggota workspace |
| Aplikasi | `/login`, `/signup`, `/verify`, `/forgot-password`, `/onboarding`, `/join` | masing-masing `page.tsx` | tidak | langkah masuk, verifikasi, dan penerimaan undangan; tidak punya isi untuk dicari |

Halaman aplikasi tidak dirayapi karena tiga hal sekaligus: tanpa sesi halaman itu tidak menampilkan
apa pun yang berguna, isinya milik satu workspace sehingga tidak boleh muncul sebagai hasil pencarian
milik orang lain, dan tidak ada orang yang mencari "dasbor" lalu pantas mendarat di dasbor workspace
yang bukan miliknya. Batas ini berlaku untuk seluruh `/settings/**` juga: halaman pengaturan memuat
nama tim, model, dan pemakaian token satu workspace.

## Yang sudah ada di kode

| Berkas | Yang dideklarasikan |
| --- | --- |
| `apps/frontend/src/lib/metadata.ts` | `SITE_URL = "https://timbolu.com"` (di-export; diimpor `app/robots.ts` dan `app/sitemap.ts`), `SITE_NAME = "Timbolu"`, `openGraphCard()`, `pageMetadata()`; kartu memuat `locale: "id_ID"`, `type: "website"`, `og:url` absolut, dan `twitter.card: "summary"` |
| `apps/frontend/src/app/robots.ts` | metadata route `/robots.txt`: `userAgent: "*"`, `allow: "/"`, `disallow` untuk rute sesi (`/dashboard`, `/approval`, `/approval/`, `/chat`, `/groups`, `/routines`, `/tasks`, `/settings`, `/onboarding`, `/bolu`, `/admin`, `/api`), dan `sitemap: "${SITE_URL}/sitemap.xml"` |
| `apps/frontend/src/app/sitemap.ts` | metadata route `/sitemap.xml`: lima halaman publik (`/`, `/pricing`, `/privacy`, `/terms`, `/contact`) dengan URL absolut dari `SITE_URL`, `lastModified` tetap `2026-10-31` |
| `apps/frontend/src/app/seo-routes.test.ts` | 4 kasus: sitemap memuat persis lima halaman publik, sitemap tidak memuat halaman sesi, `lastModified` tidak berubah antar build, dan `robots.txt` mengizinkan situs, menutup rute workspace, serta menyebut sitemap. Dijalankan `cd apps/frontend && bun test src/app/seo-routes.test.ts` |
| `apps/frontend/src/app/layout.tsx` | kartu bawaan seluruh app: `title.default`, template `%s · Timbolu`, `description`, `openGraph.title`/`description`/`siteName`/`locale`/`type`; `<html lang="id">` |
| `apps/frontend/src/app/page.tsx` | memanggil `openGraphCard()` dengan `path: "/"` |
| `apps/frontend/src/app/pricing/page.tsx`, `privacy/page.tsx`, `terms/page.tsx`, `contact/page.tsx`, `join/page.tsx` | memanggil `pageMetadata()` dengan path masing-masing |
| 14 halaman `(workspace)`, `admin/dashboard/page.tsx`, `admin/users/page.tsx`, `not-found.tsx` | `robots: { index: false }` |

`openGraphCard()` mengembalikan `openGraph` dan `twitter`; `pageMetadata()` menambahkan `title` dan
`description` di atasnya. `SITE_URL` di-export karena app tidak menyetel `metadataBase`: modul itu
satu-satunya tempat asal absolut, dan `app/robots.ts` serta `app/sitemap.ts` mengimpornya supaya tidak
ada salinan origin kedua yang bisa menyimpang. `twitter.card` bernilai `summary` karena belum ada
`og:image`; komentar di `metadata.ts` menyebut `summary_large_image` akan merender kartu kosong
tanpa gambar.

`layout.tsx` adalah pemilik kartu bawaan. Next mewariskan kartu itu ke setiap route yang tidak
mendeklarasikan kartu sendiri, sehingga halaman yang hanya mengekspor `title` dan `description`
akan tampil di percakapan sebagai kartu landing. Karena itu setiap halaman publik membangun kartunya
lewat `openGraphCard()`/`pageMetadata()`, bukan mengandalkan warisan layout.

`robots: { index: false }` ada di 17 berkas: 14 dari 17 halaman `(workspace)` yang dirender,
`admin/dashboard/page.tsx`, `admin/users/page.tsx`, dan `not-found.tsx`. `(workspace)/tasks/page.tsx`
hanya berisi `redirect("/dashboard")`, jadi tidak punya metadata sendiri.

`/robots.txt` dan `/sitemap.xml` disajikan oleh `apps/frontend/src/app/robots.ts` dan
`apps/frontend/src/app/sitemap.ts` sejak #555 merge ke `main`. `apps/frontend/next.config.ts` tetap
hanya me-rewrite dua berkas well-known (Apple dan Android); metadata route tidak lewat rewrite.
Route handler lain di `app/` adalah `api/well-known/apple-app-site-association/route.ts` dan
`api/well-known/assetlinks.json/route.ts`.

## Celah hari ini, dengan bukti

`/robots.txt` dan `/sitemap.xml` sudah tertutup: dibuat #555, dan diperiksa langsung di produksi
31 Oktober 2026 pukul 10:43 UTC — keduanya `200`, berturut-turut `content-type: text/plain` dan
`content-type: application/xml`. Isi `robots.txt` cocok dengan `app/robots.ts` (disallow rute sesi
plus baris `Sitemap` mengarah ke `https://timbolu.com/sitemap.xml`), dan `sitemap.xml` memuat persis
lima `loc`: `/`, `/pricing`, `/privacy`, `/terms`, `/contact`. Dua berkas sumbernya
`apps/frontend/src/app/robots.ts` dan `apps/frontend/src/app/sitemap.ts`.

Tiga celah yang masih terbuka:

| Yang diperiksa | Hasil | Bukti di repo |
| --- | --- | --- |
| `GET https://timbolu.com/` | `200`, tanpa `<meta name="robots">` — **masih terbuka** | `page.tsx` hanya memanggil `openGraphCard()`; `layout.tsx` tidak menyetel `robots` |
| `GET https://timbolu.com/pricing` | `200`, tanpa `<meta name="robots">` — **masih terbuka** | `pricing/page.tsx` memanggil `pageMetadata()`; helper itu tidak menyetel `robots` |
| `og:image` | tidak ada — **masih terbuka** | `openGraphCard()` dan `layout.tsx` tidak punya field `images`; `metadata.ts` mencatat ketiadaannya |

`robots.txt` dan `sitemap.xml` dikerjakan #555, yang sudah merge ke `main` dan terbukti melayani
`200` di `https://timbolu.com` pada pemeriksaan produksi 31 Oktober 2026 (`text/plain` dan
`application/xml`). #555 hanya menutup dua URL itu: halaman publik tetap tanpa
`<meta name="robots">`, dan `og:image` tetap tidak ada.

### Halaman aplikasi yang belum menyatakan `noindex`

Sembilan berkas mendeklarasikan `metadata` tanpa `robots`:

- `apps/frontend/src/app/(workspace)/settings/memory/page.tsx`, `settings/model/page.tsx`, dan
  `settings/team/page.tsx` — `metadata` hanya berisi `title` dan `description`.
- `apps/frontend/src/app/login/page.tsx`, `signup/page.tsx`, `verify/page.tsx`,
  `forgot-password/page.tsx`, `onboarding/page.tsx`, dan `join/page.tsx` — `metadata` tanpa `robots`.

`apps/frontend/src/app/admin/page.tsx` hanya `redirect("/admin/dashboard")`; halaman tujuan redirect
itu punya `robots: { index: false }`. Konsekuensinya: `robots.txt` dari #555 men-disallow `/settings`
dan `/onboarding`, tetapi tidak menyebut `/login`, `/signup`, `/verify`, `/forgot-password`, maupun
`/join`; selama sembilan berkas di atas belum diberi `robots: { index: false }`, tidak ada deklarasi
di repo yang melarang perayap mengindeks lima halaman itu.

## Yang tidak bisa dikerjakan dari repo

| Pekerjaan | Kenapa tidak dari repo | Pemilik |
| --- | --- | --- |
| Verifikasi Google Search Console | butuh akun pemilik `timbolu.com`; nilai verifikasi (DNS TXT atau meta tag) keluar dari akun itu, bukan dari kode | Robby |
| Verifikasi Bing Webmaster Tools | sama: butuh akun pemilik domain dan nilainya tidak ada di repo | Robby |
| Mengirim `sitemap.xml` ke Google/Bing | butuh properti milik Robby yang sudah terverifikasi; `app/sitemap.ts` sudah `200` di produksi sejak 31 Oktober 2026, jadi yang kurang hanya akun pemilik domain | Robby |
| Memutuskan dan menyediakan gambar OG | `og:image` butuh aset gambar yang tidak ada di repo; setelah asetnya ada, perubahan kodenya adalah menambah `images` di `openGraphCard()`/`layout.tsx` dan menaikkan `twitter.card` dari `summary` | Robby (aset dan keputusan), lalu perubahan kode |

Verifikasi kepemilikan domain adalah pekerjaan Robby karena hanya pemilik akun yang bisa menghasilkan
nilainya. Menaruh nilai itu ke repo adalah perubahan kode terpisah yang baru bisa dikerjakan setelah
nilainya ada; dokumen ini tidak mengarang nilai placeholder.

## Cara memverifikasi sendiri

Perintah di bawah adalah yang dipakai untuk § Celah hari ini dan untuk memeriksa dua URL yang sudah
`200`.

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://timbolu.com/robots.txt
curl -sS -o /dev/null -w '%{http_code}\n' https://timbolu.com/sitemap.xml
curl -sS https://timbolu.com/ | grep -o '<meta name="robots"[^>]*>' || echo 'tanpa meta robots'
curl -sS https://timbolu.com/pricing | grep -o '<meta name="robots"[^>]*>' || echo 'tanpa meta robots'
cd apps/frontend && bun test src/app/seo-routes.test.ts
```

Hasil yang diharapkan hari ini: `200`, `200`, `tanpa meta robots`, `tanpa meta robots`. Dua URL
pertama `200` — bukan `404` seperti sebelum #555 — `/robots.txt` `text/plain`, `/sitemap.xml`
`application/xml`; dua hasil `tanpa meta robots` adalah dua celah yang masih terbuka. Uji
`seo-routes.test.ts` harus lulus 4 kasus dan tidak butuh jaringan.

Gambar OG diperiksa dengan pola yang sama:

```bash
curl -sS https://timbolu.com/ | grep -o '<meta property="og:image"[^>]*>' || echo 'tanpa og:image'
```

Hari ini diharapkan `tanpa og:image` karena `openGraphCard()` dan `layout.tsx` tidak punya field
`images`. Kalau perintah ini mengeluarkan tag, gambar OG sudah dipasang dan § Celah hari ini perlu
diperbarui.
