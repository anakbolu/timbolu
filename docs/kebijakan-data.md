# Kebijakan data Bolu

Dokumen ini yang ditunjukkan ke pengguna: apa yang kami simpan, di mana, untuk apa, berapa lama, dan
bagaimana memintanya dihapus. Ia ditulis untuk dibaca pemilik usaha, bukan untuk dibaca pengacara —
dasar hukum lengkapnya ada di [ADR 0002](adr/0002-kepatuhan-uu-pdp.md).

Terakhir diperbarui: 2026-10-13.

## Apa yang kami simpan

| Data | Isinya | Untuk apa |
| --- | --- | --- |
| Akun | Nama, email, kata sandi (hash), waktu masuk | Masuk dan mengenali kamu |
| Workspace | Nama usaha, bidang, zona waktu, jam kerja | Menjalankan Bolu sesuai jam kerjamu |
| Percakapan | Pesanmu, jawaban Bolu, dan lampiran yang kamu kirim | Fitur intinya: Bolu bekerja dari percakapan |
| Memori & knowledge | Fakta yang kamu minta diingat, dan dokumen yang kamu indeks | Supaya Bolu tidak lupa dan bisa menjawab dari dokumenmu |
| Tugas | Judul, status, langkah, jumlah token, biaya | Riwayat kerja, laporan biaya, dan pemulihan kalau worker berhenti |
| Integrasi | Token OAuth penyedia yang kamu hubungkan (terenkripsi) | Menjalankan aksi atas namamu di akun itu |
| Audit | Siapa mengubah apa dan kapan: persetujuan, peran, kredensial | Menjawab "siapa yang melakukan ini" saat ada masalah |
| Pembayaran | Tagihan, status, referensi pembayaran | Menagih dan mencatat pembayaran |

Yang **tidak** kami simpan: isi pesanmu tidak disalin ke log atau ke trace telemetri. Yang tercatat
di sana hanya pengenal, jumlah, dan hasil (berhasil/gagal).

## Di mana datanya berada

- **Indonesia (Jakarta).** Database, cache, dan penyimpanan berkas — tempat seluruh data di atas
  berada.
- **Singapura.** Temporal (mesin yang menjalankan tugas) hanya menerima **pengenal** seperti
  `task_id`; isi pesan dibaca dari database di Jakarta.
- **Penyedia model yang kamu pilih.** Saat Bolu memanggil model, prompt dikirim ke penyedia yang kamu
  konfigurasi di Pengaturan → Model. Penyedia yang menerima prompt itu adalah pemroses atas namamu,
  dan kebijakan retensi mereka berlaku untuk salinan itu. Karena itu yang dikirim hanya yang
  dibutuhkan satu panggilan: pertanyaan, memori yang relevan, dan hasil alat yang terpakai.

## Berapa lama

| Data | Berapa lama | Setelah itu |
| --- | --- | --- |
| Isi mentah langkah tugas (input & output) | **30 hari** | Yang tersisa metadata: jenis langkah, alat, jumlah token, waktu. Ringkasan satu baris tugas tetap ada |
| Lampiran | Selama pesan yang membawanya | Kamu dapat menghapusnya lebih cepat lewat permintaan hapus data |
| Pesan, memori, knowledge | Selama workspace-mu ada | Dihapus saat kamu meminta penghapusan |
| Audit | Selama workspace-mu ada | Ikut terhapus saat workspace dihapus |
| Bukti penghapusan | Selamanya | Berisi id, nama, dan jumlah — bukan isi data |

Angka 30 hari di atas dapat diubah oleh tim kami per deployment; yang berlaku untuk workspace-mu
adalah yang tercantum di dokumen ini pada tanggal di atas.

## Cara meminta datamu dihapus

Penghapusan dilakukan oleh **owner workspace**, dan menghapus workspace beserta seluruh isinya: pesan,
memori, embedding, lampiran, tugas, rencana Bolu, dan jejak audit.

1. Masuk sebagai owner, buka **Pengaturan → Data**, lalu pilih **Hapus semua data workspace**.
2. Konfirmasi. Permintaan dijalankan seketika — tidak ada antrean dan tidak ada masa tunggu.
3. Kamu menerima bukti: id workspace, nama, waktu, jumlah baris dan berkas yang dihapus.

Konsekuensi yang perlu kamu tahu sebelum menekan tombol itu:

- **Tidak bisa dibatalkan.** Workspace dan seluruh isinya hilang.
- **Anggota lain kehilangan akses**, karena workspace-nya sudah tidak ada.
- **Satu workspace saja.** Kalau kamu punya beberapa workspace, yang lain tidak terpengaruh.
- **Salinan di penyedia model** mengikuti kebijakan penyedia tersebut; yang kami kendalikan adalah apa
  yang kami kirim, dan itu dibatasi pada satu panggilan.

## Hakmu

- **Tahu**: dokumen ini, dan jawaban atas pertanyaan "apakah data saya tersimpan di sini".
- **Akses**: ekspor pemakaian (token dan biaya) tersedia dari halaman Pemakaian, dan isi percakapan
  terlihat di aplikasi.
- **Perbaikan**: kamu dapat menyunting memori, mengganti dokumen, dan menghapus percakapan.
- **Penghapusan**: lewat langkah di atas, dengan bukti.
- **Keberatan**: hubungi kami lewat alamat di bawah; kami catat dan jawab.

## Menghubungi kami

Kirim email ke alamat dukungan yang tercantum di aplikasi. Untuk permintaan yang menyangkut data
pribadi, sebutkan nama workspace dan email akunmu, dan kami menjawab dengan status serta buktinya.
