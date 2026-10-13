# ADR 0002 — Kepatuhan UU PDP: lokasi data, dasar pemrosesan, dan permintaan hapus

- Status: **Diterima**
- Tanggal: 2026-10-13
- Task: E13.6 (`#111`)
- Bergantung pada: [ADR 0001](0001-temporal-dan-region.md) (region penyimpanan) dan E13.2 (`#107`, retensi)

## Ringkasan keputusan

| Pertanyaan | Keputusan |
| --- | --- |
| Region data pelanggan | **Jakarta** (AWS `ap-southeast-3`) untuk Postgres, Redis, dan object storage |
| Data yang keluar region | Hanya **referensi (ID)**, bukan isi, lewat Temporal Cloud (Singapore) |
| Pemroses (processor) pihak ketiga | Penyedia LLM pilihan workspace, Temporal Cloud, penyedia email transaksional, penyedia pembayaran (Duitku) |
| Dasar pemrosesan | **Pelaksanaan kontrak** (menyediakan layanan yang diminta) + **persetujuan** untuk integrasi yang workspace hubungkan sendiri |
| Retensi | Kebijakan berjangka, dipublikasikan: [kebijakan-data.md](kebijakan-data.md) |
| Permintaan hapus | **Satu endpoint owner**, dieksekusi menyeluruh, dengan bukti yang disimpan melewati data itu sendiri |

## 1. Di mana data disimpan, dan siapa yang memprosesnya

**Fakta yang mengikat.** UU No. 27/2022 tentang Pelindungan Data Pribadi tidak mewajibkan penyimpanan
di dalam negeri. Yang diwajibkan adalah transparansi (siapa memproses, di mana, atas dasar apa),
retensi terbatas, dan pemenuhan hak subjek data — termasuk hak penghapusan.

**Keputusan.** Data pelanggan (pesan, memori, lampiran, task, audit) disimpan di **Jakarta**, sesuai
[ADR 0001](0001-temporal-dan-region.md). Yang diproses di luar Jakarta hanya **referensi**:

| Komponen | Lokasi | Apa yang dilihatnya |
| --- | --- | --- |
| Postgres (+`pgvector`), Redis, object storage | Jakarta | Seluruh isi data pelanggan |
| Temporal Cloud | Singapore (`aws-ap-southeast-1`) | Hanya ID: `task_id`, `draft_id`, `message_id`. Isi dibaca worker dari Postgres |
| Penyedia LLM (OpenAI/Anthropic/kompatibel) | Menurut penyedianya | Prompt pada saat panggilan, termasuk memori yang disematkan sebagai bagian prompt |
| Email transaksional (SMTP) | Menurut penyedianya | Alamat email dan isi email undangan/verifikasi |
| Duitku (pembayaran) | Indonesia | Nama, email, jumlah tagihan |

**Alasan.**

- Claim check pada workflow (ADR 0001) sudah memenuhi syarat "data keluar region seminimal mungkin"
  tanpa mengorbankan durability: riwayat Temporal tetap bisa diputar ulang karena isi sesungguhnya
  ada di Postgres Jakarta.
- Penyedia LLM adalah pilihan workspace sendiri, dan kuncinya milik workspace. Platform hanya
  menyediakan jalur; pemroses atas nama workspace adalah penyedia yang workspace pilih.
- Data pelanggan dapat dihapus dari penyedia LLM hanya sesuai kebijakan penyedia tersebut. Karena itu
  yang dikirim ke penyedia dibatasi: **hanya apa yang dibutuhkan satu panggilan**, bukan seluruh
  arsip workspace.

**Konsekuensi.**

- Sebuah panggilan model mengirim prompt + memori yang relevan + hasil alat yang terpakai. Itu tidak
  dapat dihindari untuk produk ini; yang dapat dihindari adalah mengirim lebih banyak dari itu, dan
  itulah yang dilakukan (memori dibatasi token, riwayat dibatasi jumlah pesan, hasil alat dipotong).
- Log dan trace tidak memuat isi pelanggan sama sekali: span OTel hanya membawa identifier, jumlah,
  dan hasil (lihat `internal/platform/telemetry`), dan log request meredaksi query string.
- Deletion di penyedia LLM mengikuti kebijakan penyedia; ini dicatat di kebijakan data.

## 2. Alur permintaan hapus data

**Keputusan.** Satu endpoint: `POST /api/workspaces/current/erasure` (owner workspace, langganan
aktif). Tidak ada "jadwalkan" dan tidak ada "batalkan" — hak subjek data adalah **permintaan** yang
jawabannya "selesai" atau "inilah alasannya", bukan antrean yang menahan data setelah diminta
dihapus.

**Cakupan.** Semuanya, karena tiga lapisan penghapusan bekerja bersama:

1. **Object storage lebih dulu.** Lampiran dan dokumen HTML sandbox hidup di bucket, bukan di
   Postgres. Dua prefix milik modul chat (`attachments`, `content`) di-walk per workspace. Urutan ini
   yang membuat penghapusan tetap bisa dijalankan: kalau baris `workspaces` dihapus lebih dulu,
   baris lampiran ikut ter-cascade, kunci storage-nya hilang, dan byte-nya menjadi objek yang tidak
   bisa ditemukan query apa pun.
2. **Baris kedua.** `DELETE FROM workspaces` menghapus tenant dan seluruh yang mereferensikannya.
   Semua tabel ber-`workspace_id` memakai `ON DELETE CASCADE`, jadi tidak ada daftar tabel yang harus
   dirawat — dan tabel yang lupa mendeklarasikan referensi adalah satu-satunya cara data tertinggal.
3. **Bukti ketiga.** Sebuah baris di `data_erasures` — **tanpa foreign key**, karena baris itu harus
   hidup lebih lama daripada workspace yang dinamainya. Ia menyimpan id, nama workspace, siapa yang
   meminta, kapan, dan berapa banyak yang dihapus.

**Ini adalah cara "permintaan hapus dieksekusi menyeluruh" diverifikasi**, dan verifikasinya ada di
`test/integration/erasure_test.go`: satu workspace diisi lewat modul-modul asli (percakapan, pesan,
lampiran, task + langkah, memori bervektor, draf + kebijakan), lalu dihapus, lalu **dihitung ulang
dari skema**: tidak ada baris tersisa di tabel mana pun yang memakai `workspace_id`, tidak ada objek
tersisa di kedua prefix, baris `workspaces` sendiri hilang (bukan ditandai), dan baris bukti tetap ada
beserta namanya.

**Konsekuensi.**

- Permintaan yang gagal separuh **tidak dicoba ulang otomatis**. Ia dicatat dengan alasannya, dan
  urutannya (objek lalu baris) berarti kegagalan paling berbahaya — byte yang tidak bisa dihapus —
  meninggalkan workspace tetap utuh supaya permintaan bisa diulang. Ini keputusan sadar: percobaan
  ulang otomatis atas data pelanggan adalah deployment memutuskan sendiri untuk mencoba lagi.
- Endpoint hanya untuk owner. Bukan karena anggota lain tidak boleh tahu, tapi karena tindakan yang
  tidak bisa dibatalkan harus punya satu lapisan izin yang jelas.
- Bukti dapat dibaca kembali (`GET /api/workspaces/current/erasure`) selama workspace masih ada, dan
  lewat tabel `data_erasures` untuk operator setelahnya.

## 3. Retensi yang dipublikasikan

Kebijakan berjangka ada di [kebijakan-data.md](kebijakan-data.md), dan ringkasnya:

- Isi mentah `task_step` (input dan output mentah): **30 hari**, lalu yang tersisa adalah metadata —
  jenis langkah, alat, jumlah token, waktu.
- Lampiran: **selama pesan yang membawanya**, kecuali deployment menetapkan jendela sendiri.
- Pesan, memori, dan knowledge: selama workspace ada; permintaan hapus menghapusnya lebih cepat.
- Audit trail: **selama workspace ada**, karena ia yang menjawab "siapa melakukan apa".
- Bukti penghapusan: **selamanya**, karena ia yang menjawab "apakah data saya sudah dihapus".

## 4. Dampak ke repositori

- `docs/kebijakan-data.md` — kebijakan yang dapat ditunjukkan ke pengguna.
- `docs/adr/0002-kepatuhan-uu-pdp.md` — dokumen ini.
- `migrations/000020_data_erasures.*` — tabel bukti, tanpa foreign key.
- `internal/modules/retention/{domain/erasure.go,service/erasure.go,handler/handler.go}` — alur
  permintaan hapus.
- `internal/modules/chat/service/service.go` (`EraseObjects`) — walk bucket per workspace.
- `internal/modules/workspace/repository` (`EraseWorkspace`, `WorkspaceName`, `RequesterEmail`) —
  penghapusan tenant dan dua nama untuk bukti.
- `internal/platform/storage` (`Store.List`) — enumerasi objek, yang dibutuhkan driver mana pun untuk
  memenuhi janji "hapus semua milik tenant ini".

## Keputusan yang tetap terbuka

- **Lokasi penyedia LLM.** Sebuah workspace dapat memilih penyedia yang memproses di luar Indonesia;
  itu pilihannya dan tercatat di layar Pengaturan → Model. Bila kelak produk menjual ke instansi yang
  mensyaratkan pemrosesan dalam negeri, jalurnya adalah menyediakan penyedia dalam negeri sebagai
  pilihan — bukan membatasi pilihan workspace.
- **Region cadangan (backup).** Backup Postgres mengikuti region primernya di Jakarta; kebijakan
  retensi backup dan cara menghapus satu workspace dari backup yang sudah dibuat belum ditetapkan, dan
  itu pekerjaan operasional yang harus diselesaikan sebelum pelanggan pertama menandatangani kontrak
  yang menyebutnya.
