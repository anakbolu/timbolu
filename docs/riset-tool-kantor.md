# Riset — Tool kantor yang biasa dipakai, dan mana yang layak disambungkan

Dokumen ini menjawab satu pertanyaan yang muncul berulang kali waktu menyusun katalog
Integrasi: **aplikasi apa saja yang benar-benar dipakai kantor sehari-hari, dan dari
situ mana yang paling masuk akal jadi adapter berikutnya?** Ia ditulis untuk dibaca
sebelum menambah kartu di layar Integrasi — supaya yang dibangun bukan daftar
aplikasi populer secara umum, melainkan yang benar-benar menahan pekerjaan kantor
di Indonesia.

Terakhir diperbarui: 2026-10-14.

## Ringkasan

Tiga hal yang paling menentukan arah katalog:

1. **Kantor tidak punya satu tool, ia punya tujuh jenis.** Surel, obrolan, dokumen,
   proyek, keuangan & pajak, SDM, dan jualan. Setiap jenis punya satu-dua pemain
   dominan global, tetapi di Indonesia isinya bergeser.
2. **Pergeseran terbesar ada di tiga tempat: WhatsApp, marketplace, dan pembukuan
   lokal.** Di pasar global, Microsoft 365 dan Google Workspace mendominasi dan
   Slack/Zoom menyusul. Di Indonesia, kanal kerja utama UMKM adalah **WhatsApp**,
   penjualannya lewat **Tokopedia/Shopee/TikTok Shop**, dan pembukuannya lewat
   **Mekari Jurnal atau Accurate** — bukan QuickBooks atau Xero.
3. **Yang paling bernilai diotomasi adalah pekerjaan yang berulang dan lintas
   aplikasi**: menagih, mencatat pesanan, menjawab pembeli, membuat faktur dan
   laporan pajak. Itu tepatnya pekerjaan yang Bolu klaim bisa kerjakan, jadi
   katalog integrasi adalah jalan produk, bukan pelengkap.

## Metode dan batasnya

Riset ini menggabungkan dua jenis sumber:

- **Data pasar dan adopsi** dari laporan industri (Statista, Cledara, Torii, Moss,
  survei T3, Work Trend Index Microsoft, Deloitte) untuk peta global.
- **Sumber pasar Indonesia** (halaman produk vendor, dokumentasi API resmi, riset
  akademik UMKM, dan pemberitaan) untuk peta lokal.

Batas yang perlu diakui: angka pangsa pasar dari penyedia berbeda tidak selalu
sebanding (ada yang menghitung jumlah perusahaan, ada yang menghitung belanja, ada
yang menghitung waktu pakai), jadi angka di sini dipakai untuk menunjukkan **arah
dan urutan**, bukan untuk dikutip sebagai satu kebenaran. Harga vendor berubah
cepat; angka rupiah/dolar di bawah adalah titik awal, bukan janji.

## 1. Peta global: tujuh jenis tool kantor

Ini kerangka yang dipakai dokumen ini. Kategorinya sengaja dibuat sejajar dengan
kategori yang sudah ada di layar Integrasi, supaya hasil riset bisa langsung
dipetakan ke kartu.

| Jenis | Yang dikerjakan | Pemain dominan (global, 2026) |
| --- | --- | --- |
| Surel & kalender | Korespondensi, undangan, jadwal | Microsoft Outlook/365, Google Workspace (Gmail) |
| Obrolan & rapat | Pesan tim, panggilan video | Slack, Microsoft Teams, Zoom |
| Dokumen & file | Berkas, catatan, tanda tangan | Google Drive/Docs, Microsoft 365, Notion, Dropbox, Box |
| Proyek & developer | Tugas, sprint, isu, kode | Jira, Asana, monday.com, ClickUp, Linear, Trello, GitHub |
| Keuangan & pajak | Pembukuan, faktur, laporan | (global) QuickBooks, Xero, Zoho |
| SDM & penggajian | Payroll, absensi, kontrak | (global) BambooHR, Rippling, Gusto |
| Jualan & CRM | Pipeline, pelanggan, toko | Salesforce, HubSpot, Pipedrive; (toko) Shopify, Amazon |

Beberapa angka yang menopang urutan itu:

- **Microsoft 365 masih memimpin** pangsa pasar perangkat lunak produktivitas
  kantor per Maret 2026, dan **Office Suites adalah subkategori belanja terbesar**
  (7,5%) dalam data pembelian nyata 754 produk yang dilacak Cledara. Tool yang
  paling banyak dipakai di sana: Microsoft 365, Google/Gemini, dan Slack.
- Di kategori komunikasi & kolaborasi, **Microsoft mengambil 62,6% belanja
  kategori** dalam data belanja 20.064 vendor (Januari 2024–Mei 2026). Survei T3
  menempatkan **Microsoft Teams** di pangsa 35,13% untuk konferensi video.
- Di perusahaan kecil (≤50 karyawan) urutannya bergeser ke tool yang ringan:
  **Zoom, Slack, dan Miro** paling banyak dipakai (runsonwhat, Oktober 2026).
- **Aplikasi AI sekarang ikut jadi "tool kantor".** Business Insider mencatat
  Anthropic memimpin pertumbuhan pelanggan korporat (Juni 2022–Juni 2026), diikuti
  OpenAI dan Cursor; sekaligus Google Workspace, Microsoft 365, Slack, Notion,
  Canva, dan Zoom termasuk aplikasi AI yang tumbuh tercepat — artinya kantor
  **menambah** tool AI baru tanpa membuang tool lama.

**Yang perlu dicatat dari sisi tata kelola.** Survei 2026 Akamai menemukan 47%
interaksi AI di tempat kerja terjadi lewat identitas pribadi, bukan akun yang
dikelola perusahaan; PagerDuty menemukan 66% pekerja kantor memakai tool AI yang
tidak diizinkan kebijakan. Untuk produk seperti Bolu, ini argumen kuat: nilai
sebuah hub integrasi yang terpusat adalah **membuat pekerjaan itu terlihat dan
lewat persetujuan**, bukan menambah tool baru yang menambah bayangan.

## 2. Peta Indonesia: tiga hal yang membedakan

Kategori globalnya tetap relevan, tetapi isi tiap kategori bergeser. Ini yang
paling penting untuk katalog Bolu.

### a. WhatsApp adalah kanal kerja utama, bukan pelengkap

Riset akademik UMKM Indonesia (2021–2025) berulang kali menemukan WhatsApp Business
sebagai **media komunikasi utama** pelaku usaha, termasuk di B2B, sementara kanal
lain hanya diketahui atau tidak pernah jadi platform utama. Konsekuensi praktisnya:
kalau Bolu ingin "mengerjakan pekerjaan manusia" di kantor Indonesia, WhatsApp
bukan opsi tambahan — ia kanal pertama.

Catatan penting: **WhatsApp tidak punya API resmi yang murah dan terbuka untuk
sembarang UMKM.** Jalur resmi (WhatsApp Business Platform/Cloud API) berbiaya dan
perlu verifikasi bisnis, sedangkan jalur tidak resmi melanggar ToS dan berisiko
nomor diblokir. Repo ini sudah menyikapinya dengan benar: kartu WhatsApp memuat
peringatan ToS sebelum penautan. Riset ini menguatkan bahwa sikap itu bukan
kehati-hatian berlebihan, melainkan syarat.

### b. Penjualan lewat marketplace, dan mereka tidak saling bicara

Pemain utamanya **Tokopedia (kini satu Seller Center dengan TikTok Shop),
Shopee, Lazada, dan Blibli**. Titik penting: tiap marketplace punya seller
center-nya sendiri dan **tidak saling terhubung otomatis** — itu alasan munculnya
lapisan agregator pihak ketiga (Sellizy, OmniToko, Izaaja, Onorder) yang
menyatukan pesanan, stok, dan laporan ke satu dasbor lewat API resmi.

Ada juga perubahan regulasi yang membuat pekerjaan ini makin berbobot: per Juli
2026 otoritas pajak meminta Tokopedia, Shopee, Lazada, dan Blibli memungut pajak
penghasilan dari penjual, berlaku sejak Agustus. Artinya **rekonsiliasi penjualan
dengan kewajiban pajak** naik jadi pekerjaan rutin baru — kandidat otomasi yang
jelas.

### c. Pembukuan dan pajak memakai produk lokal

Untuk UMKM, pilihan realistisnya ada di antara **Mekari Jurnal** dan **Accurate
Online**, dengan **Kledo** dan **Beecloud** sebagai alternatif hemat, dan **Odoo**
untuk usaha yang butuh sistem lebih luas. **Accurate** kuat di kalangan akuntan dan
konsultan pajak; **Mekari** kuat di ekosistem (payroll, e-Faktur via Klikpajak,
e-commerce, POS). Kepatuhan pajak lokal — **PPN, e-Faktur, Coretax** — adalah fitur
yang wajib ada dan tidak dimiliki tool global.

Untuk SDM: **Mekari Talenta** dan **Gadjian** mendominasi penggajian, dengan format
e-filing, SPT Masa, dan BPJS langsung tersedia. Untuk tanda tangan digital:
**Privy** dan **Mekari Sign**. Untuk CRM lokal: **Qontak**.

Untuk pembayaran, penyedia yang lazim adalah **Midtrans, Xendit, dan Duitku** —
dan repo ini sudah memakai Duitku untuk langganan, jadi jalur pembayarannya sudah
kenal ekosistem lokal.

## 3. Tabel keputusan: apa yang layak, dan apa yang bisa dikerjakan bot

Tabel ini adalah inti dokumen. Kolom "Bisa dikerjakan Bolu" menyebut pekerjaan
konkret yang punya nilai kalau disambungkan.

| Jenis | Tool yang relevan di Indonesia | Bisa dikerjakan Bolu |
| --- | --- | --- |
| Obrolan | WhatsApp Business | Jawab pembeli, kirim pengingat tagihan, rangkum percakapan |
| Surel & kalender | Gmail, Outlook | Susun draf balasan, jadwalkan, rangkum utas |
| Dokumen & file | Google Drive, Notion, Dropbox | Cari & rangkum dokumen, rapikan catatan, basis pengetahuan |
| Proyek & developer | Jira, Trello, GitHub, Linear | Buat/perbarui isu, laporkan status sprint, rangkum PR |
| Keuangan & pajak | Mekari Jurnal, Accurate, Kledo, Klikpajak | Catat transaksi, buat faktur, siapkan berkas e-Faktur/PPN |
| SDM & penggajian | Mekari Talenta, Gadjian, Privy | Kumpulkan data absensi, siapkan berkas payroll, kirim kontrak |
| Jualan & toko | Tokopedia, Shopee, Lazada, Blibli, TikTok Shop | Tarik pesanan, sinkronkan stok, rekonsiliasi penjualan & pajak |
| Pembayaran | Midtrans, Xendit, Duitku | Catat pembayaran masuk, cocokkan tagihan, tandai lunas |

## 4. Petakan ke katalog yang sudah ada

Keadaan sekarang, dibaca dari kode:

- **Adapter yang benar-benar terpasang** (`integrationAdapters`): Order (katalog
  workspace sendiri), Checklist (rencana milik Bolu), WhatsApp (kalau gateway
  terkonfigurasi), dan Gmail (kalau kredensial OAuth ada). Adapter Gmail ada di
  `internal/modules/integration/adapters/gmail`; Order dan Checklist sengaja bukan
  akun pihak ketiga.
- **Kartu di layar Integrasi** sudah menyiapkan tujuh tile: Gmail, WhatsApp
  Business, Google Drive, Jira, GitHub, Notion, dan Tokopedia — dengan kategori
  *Email & kalender*, *Chat*, *Dokumen & file*, *Proyek & developer*, dan *Toko
  online*.

Jadi desainnya sudah menebak arah yang sama dengan riset ini. Yang belum ada di
kode tetapi muncul kuat di riset adalah **keuangan & pajak**, **SDM & penggajian**,
dan **marketplace selain Tokopedia** — tiga kategori yang paling membedakan kantor
Indonesia dari kantor global.

## 5. Urutan yang diusulkan

Disusun dari nilai tertinggi dibagi biaya terendah. Fase 1 adalah yang paling
dekat dengan MVP dan sudah separuh jalan di kode.

**Fase 1 — yang sudah dirancang, diselesaikan.** Gmail (ada) dan Google Drive.
Alasannya: OAuth standar, sudah ada kerangka adapter, dan dokumen adalah bahan
kerja bot yang paling langsung.

**Fase 2 — yang membedakan di Indonesia.** WhatsApp (ada kerangkanya) dan satu
marketplace dengan API resmi (Tokopedia/TikTok Shop atau Shopee). Alasannya:
dua-duanya kanal kerja utama UMKM, dan keduanya sudah punya kartu di desain.

**Fase 3 — pekerjaan yang berulang dan bernilai.** Satu tool keuangan lokal
(Mekari Jurnal atau Accurate) plus Klikpajak, dan satu agregator marketplace.
Alasannya: di sinilah otomasi menghemat kerja paling banyak, tetapi juga paling
butuh ketelitian — ini pekerjaan yang menyentuh uang dan pajak.

**Fase 4 — tim.** Jira/Notion/GitHub untuk kantor yang punya tim produk, dan
Talenta/Gadjian untuk yang punya HR.

## 6. Risiko dan catatan kepatuhan

- **ToS dan pemblokiran nomor.** WhatsApp lewat jalur tidak resmi melanggar ToS;
  ini sudah diperingatkan di UI. Jangan dihaluskan demi kenyamanan.
- **Uang dan pajak tidak boleh salah.** Integrasi keuangan harus lewat persetujuan
  manusia (default draf, sesuai prinsip "manusia memegang pintu keluar") dan
  meninggalkan jejak audit. Selaras dengan [kebijakan-data.md](kebijakan-data.md)
  dan [ADR 0002](adr/0002-kepatuhan-uu-pdp.md).
- **Token penyedia adalah kredensial sensitif.** Ia sudah disegel lewat key ring
  (`internal/container/integration.go`); adapter baru harus memakai jalur yang sama,
  bukan menyimpan token sendiri.
- **Data pelanggan tetap di Jakarta.** Karena itu yang dikirim ke penyedia model
  hanya yang dibutuhkan satu panggilan — integrasi baru tidak boleh mengubah
  aturan itu.
- **Bayangan integrasi pribadi.** Kalau Bolu tidak menyediakan jalur yang
  disetujui, orang akan memakai akun pribadinya (47% menurut Akamai). Menyediakan
  jalur resmi yang mudah adalah kontrol, bukan sekadar fitur.

## 7. Yang belum terjawab

- Biaya sebenarnya jalur resmi WhatsApp Business Platform untuk skala UMKM, dan
  apakah cukup banyak workspace yang mau menanggungnya.
- Mana yang lebih dulu: Tokopedia/TikTok Shop atau Shopee, dilihat dari kemudahan
  akses API resmi untuk pihak ketiga.
- Apakah cukup menyambung satu tool keuangan, atau pelanggan tersebar cukup rata
  antara Mekari dan Accurate sehingga keduanya perlu ada.

## Referensi

Pasar global:

- Statista — *Market share of major office productivity software worldwide* (Maret 2026).
- Cledara — *Productivity Software Market Share 2026: 754 Tools Ranked*.
- Torii — *Adoption Index* (Maret–Agustus 2026).
- Moss — *Best communication and collaboration tools in 2026, ranked by spend*.
- T3 — *2026 Inside Information Software Survey* (pangsa konferensi video).
- runsonwhat — *Most popular collaboration tools, ranked by companies using them* (Oktober 2026).
- Business Insider — *The AI apps winning over corporate America* (Agustus 2026).
- Microsoft — *2026 Work Trend Index*; Deloitte — survei 25.000 karyawan (lewat FT).
- Akamai — *2026 Enterprise AI Usage Risk Report*; PagerDuty — *Shadow IT survey*.
- DeskTime — pangsa waktu AI di tempat kerja (lewat TechRadar, 2026).

Pasar Indonesia:

- Mekari Jurnal — *Program & Aplikasi Akuntansi untuk UMKM*; *Contoh Aplikasi Pendukung untuk UMKM*.
- HashMicro — *Accurate vs Jurnal: Review Fitur dan Biaya*.
- Sparta — *15 Software Akuntansi Terbaik di Indonesia 2026*; *Harga Software Akuntansi di Indonesia 2026*.
- Mekari Talenta — *Software & Aplikasi Payroll Karyawan Terbaik di Indonesia*; *Fitur Software HRIS*.
- Privy — *Tanda Tangan & Identitas Digital untuk HR*; Mekari Sign — integrasi Talenta.
- Onorder — *Aplikasi Integrasi Marketplace Terbaik untuk Penjual Online* (2026).
- Lazada Open Platform — *Instructions on calling APIs* (endpoint Indonesia).
- TikTok Newsroom — *Tokopedia & TikTok Shop Seller Center Resmi Diluncurkan* (Juni 2025).
- Sellizy, OmniToko, Izaaja, Kasaba — dokumentasi integrasi marketplace (2026).
- Reuters — *Indonesia asks four e-commerce platforms to collect income tax from sellers* (Juli 2026).
- Garuda/Kemdiktisaintek — kajian literatur *WhatsApp Business pada UMKM* (2021–2025).
- Cherry Tree, Pixa — perbandingan *Google Workspace vs Microsoft 365* untuk bisnis Indonesia.
