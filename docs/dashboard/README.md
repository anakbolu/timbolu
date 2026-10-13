# Dasbor — screenshots

Tampilan setiap layar diambil dari build produksi (`docker compose up`, image
`timbolu-frontend`) dengan workspace yang punya kru dan riwayat tugas sungguhan,
bukan data contoh.

| Berkas | Layar |
| --- | --- |
| `01-dasbor.png` | Dasbor: sambutan, "Suruh Bolu", statistik, aktivitas langsung, tugas terakhir |
| `02-rutinitas.png` | Rutinitas: form cadence, jadwal tiap Bolu dengan waktu berikutnya, dan riwayat eksekusi |
| `03-kantor.png` | Kantor: posisi tiap Bolu dari `display_status` |
| `04-integrasi.png` | Integrasi: kartu WhatsApp dengan peringatan ToS sebelum penautan, lalu pencarian, kategori, kartu aplikasi, izin per Bolu |
| `05-pengaturan.png` | Pengaturan: profil, jam kerja, kebijakan persetujuan, gaya bahasa |
| `06-obrolan.png` | Obrolan: thread terbuka, balasan tersimpan |
| `07-grup.png` | Grup: daftar grup dan pembuatannya |
| `08-mobile.png` | Mobile: bilah atas menggantikan sidebar di bawah 760px |
| `09-mobile-menu.png` | Mobile: drawer terbuka |
| `10-memori.png` | Memori: memori utama, memori pribadi tiap Bolu, dan basis pengetahuan |
| `11-langganan.png` | Langganan: paket dan periodenya, pemakaian token, batas paket, tagihan yang menunggu dengan cara bayarnya, paket tambahan, paket yang tersedia, dan riwayat tagihan |
| `12-pemakaian.png` | Pemakaian: ringkasan token dan biaya, pemakaian per Bolu dan per tugas, dan daftar yang perlu perhatian dengan tautan ke Temporal |
| `13-data.png` | Data: berapa lama tiap jenis data disimpan, di mana datanya berada, dan permintaan penghapusan workspace |

Diambil 2026-10-12. Angka di layar adalah isi workspace uji, jadi nol di tempat
yang belum punya pekerjaan — itu memang perilakunya, bukan tampilan kosong.

`04-integrasi.png` diambil ulang setelah EPIC 9 mendarat, dengan kartu WhatsApp di
atas daftar aplikasi: peringatan jalur tidak resmi dengan tombol terimanya, pemilih
nomor dan Bolu yang menjawab, serta status tiap nomor. Yang belum ada di layar
adalah QR-nya, karena penautan butuh HP sungguhan untuk dipindai.

`11-langganan.png` diambil setelah EPIC 12 mendarat: paket Keluarga aktif dengan
periode dan batasnya, pemakaian 800 ribu dari 3,4 juta token (termasuk 1,4 juta
dari dua paket tambahan yang sudah dibayar), satu tagihan langganan yang menunggu
beserta virtual account-nya, kedua paket tambahan yang bisa dibeli, tiga paket
yang tersedia, dan riwayat tagihan lengkap. Angka-angkanya dari workspace uji yang
benar-benar melewati alur pembayaran, bukan data contoh.

`02-rutinitas.png` diambil setelah EPIC 11 mendarat, dengan tiga rutinitas yang
benar-benar tersimpan lewat API — satu harian, satu tiap jam dalam rentang, dan
satu berbasis kejadian — beserta dua eksekusi yang sudah tercatat. Ia
memperlihatkan apa yang jadi alasan layarnya berubah: jenis jadwal dan zona waktu
dipilih di form, waktu berikutnya dihitung di zona workspace (WITA di sini,
sementara peramban WIB), dan tiap baris riwayat membawa ringkasan hasil
eksekusinya.

`10-memori.png` diambil setelah EPIC 10 mendarat, dengan isi yang benar-benar
tersimpan lewat API: tiga memori utama (satu disematkan), satu memori pribadi
milik Oren, dan satu dokumen yang sudah dipecah lalu diindeks. Ia memperlihatkan
susunan yang jadi alasan layarnya ada — memori utama di atas, satu bagian per
Bolu, basis pengetahuan di bawah — dan bahwa tiap baris menyebut jenis, sumber,
serta tanggalnya.

`12-pemakaian.png` dan `13-data.png` diambil setelah EPIC 13 mendarat, dari workspace uji
yang benar-benar menjalankan alur pesanan EPIC 14 lewat stub model: empat tugas dengan
total 11 panggilan dan 3.864 token, dibaca dari `usage_ledger` — angka yang sama yang
dibaca `/api/llm/usage/report`, dan yang diperiksa `scripts/check-usage.py` terhadap
baris-barisnya. Baris **Perlu perhatian** memperlihatkan satu tugas gagal beserta
tautannya ke Temporal: compose lokal tidak punya kredensial Gmail, jadi executor
integrasinya menjawab "unknown tool: gmail.send" — persis perilaku yang diminta (tidak
ada yang ditandai terkirim kalau tidak benar-benar keluar), dan itulah gunanya daftar
itu.

`13-data.png` memperlihatkan halaman kebijakan: setiap jenis data beserta lamanya, dan
apa yang tersisa setelah jendelanya lewat — termasuk bahwa langkah tugas kehilangan
isinya tapi mempertahankan bentuknya, dan bahwa jejak audit ikut hilang bersama
workspace-nya.
