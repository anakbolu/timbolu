# Admin — screenshots

Tampilan setiap layar diambil dari build produksi (`next build` + `next start`)
dengan backend yang migrasinya sudah sampai `000024_platform_admin`, dan dengan
akun yang benar-benar dibuat lewat `/api/auth/register` — bukan data contoh.

| Berkas | Layar |
| --- | --- |
| `01-ringkasan.png` | Ringkasan: jumlah akun, berapa yang admin, berapa yang baru masuk, dan keadaan akun (terverifikasi, onboarding, Google) |
| `02-pengguna.png` | Pengguna: daftar akun dengan perannya, pencarian alamat, filter peran, dan satu tombol per baris |

Diambil 2026-10-16 dari database uji berisi lima akun. Angkanya adalah isi
database itu apa adanya: 5 akun, **1 admin**, 4 pengguna biasa, 5 terverifikasi,
4 sudah onboarding, 0 akun Google, 5 dibuat dalam tujuh hari terakhir.
`admin@bolu.local` diangkat lewat satu statement `UPDATE`, karena itulah
satu-satunya cara admin pertama dibuat — tidak ada layar yang memberikannya.
`sari@bolu.local` dan `budi@bolu.local` sudah punya workspace, `dewi@bolu.local`
belum, dan `backfill@bolu.local` adalah akun yang sudah ada di database sebelum
migrasi ini berjalan: ia `user`, yang memang janji kolomnya.

`02-pengguna.png` memperlihatkan dua hal yang jadi alasan layarnya ada. Baris
`admin@bolu.local` tombolnya **mati**: seorang admin tidak bisa menurunkan
perannya sendiri, karena keputusan itu tentang orang lain dan salah klik tidak
boleh mengakhiri sesi yang membuatnya. Kolom **Workspace** berisi angka, bukan
daftar: seorang operator perlu tahu apakah sebuah akun dipakai, sementara
workspace mana yang diikutinya adalah urusan pelanggannya — dan itu batas yang
dipegang seluruh modul ini, yang membaca tabel `users` dan satu `count` ke
`members`, tidak lebih.
