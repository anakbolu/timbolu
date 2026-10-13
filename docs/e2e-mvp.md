# E2E MVP — pesanan WhatsApp → catat → tagih (EPIC 14)

Dokumen ini adalah skenario yang diminta task E14.2 dan dipakai ulang oleh E14.3, E14.4, dan
E14.5. Isinya: apa yang dibuktikan, data uji yang dipakai, langkahnya beserta hasil yang
diharapkan, dan bentuk yang bisa dijalankan ulang.

Skenario ini **tidak menggantikan** `docs/testing.md`: itu daftar gate otomatis, ini satu alur
yang harus jalan utuh di satu lingkungan.

## Apa yang dibuktikan

Alur inti MVP, dan tiga janji produk yang menyertainya:

1. **Pesanan tercatat dari harga katalog, bukan dari model.** Nama produk dan jumlah datang dari
   pesan pelanggan; harga datang dari katalog workspace, diambil di dalam transaksi yang menulis
   pesanannya. Schema alat pencatatannya tidak punya field harga, jadi angka yang dikarang model
   tidak punya tempat untuk masuk ke tagihan.
2. **Pesanan yang tidak bisa diambil tidak diambil sebagian.** Produk yang tidak ada di katalog
   menolak seluruh pesanan, menyebutkan apa yang katalog punya, dan tidak menulis apa pun — bukan
   menulis baris yang berhasil lalu menagih sebagian.
3. **Tidak ada yang keluar tanpa persetujuan.** Tagihan yang disusun Bolu menjadi draf; draf itu
   memuat penerima, kode pesanan, dan totalnya, dan tidak ada pesan yang sampai ke pelanggan
   sebelum seseorang menyetujuinya.

## Prasyarat

- `make up` — Postgres+pgvector, Redis, Temporal, API, kedua worker, dan frontend.
- Satu provider model yang menjawab, atau stub `scripts/smoke-order.sh` untuk dijalankan ulang
  tanpa biaya token (lihat *Bentuk yang bisa dijalankan*).
- Satu kanal keluar untuk tagihan: nomor WhatsApp yang sudah ditautkan, atau akun Gmail yang
  disambungkan lewat `/api/integrations/gmail/connect`. Alur di atas batas kirim sama untuk
  keduanya — yang berbeda hanya alat `*.send`/`*.send_message` yang ditawarkan ke model.

Worker compose memakai task queue yang sama dengan smoke, jadi hentikan dulu sebelum menjalankan
ulang skenario secara manual:

```bash
docker stop bolu-api bolu-agent-worker bolu-integration-worker
docker start bolu-api bolu-agent-worker bolu-integration-worker   # setelah selesai
```

## Data uji

Semua data dibuat per jalan, di workspace baru, jadi tidak ada yang menyentuh data yang sudah ada:
pendaftaran memakai email ber-timestamp (`order-<epoch>@example.com`), dan katalognya diisi lewat
API seperti orang mengisinya di layar.

### Katalog

| SKU | Nama | Harga | Satuan |
| --- | --- | --- | --- |
| `KOPI-1` | Kopi Arabika | Rp 15.000 | kg |
| `SUSU-1` | Kopi Susu | Rp 20.000 | pcs |

Harga dikirim dan dibaca dalam rupiah (`price_idr`), disimpan dalam micro-rupiah — unit yang sama
dengan `plans`, `usage_ledger`, dan laporan biaya, supaya total tagihan dan biaya token adalah
angka dari barang yang sama.

### Memori utama workspace

Ditulis lewat `POST /api/memories` (lapisan utama, `agent_id` kosong), karena itu yang dibaca
sebagai blok instruksi di setiap panggilan model:

- `Nama usaha: Kopi Sinar. Sapaan pelanggan: "Bu/Pak" diikuti namanya. Jam buka 08.00-17.00 WIB.`
- `Format tagihan: "Terima kasih <nama>, pesanan <kode> sudah kami catat: <daftar baris>. Total
  <total rupiah>. Mohon dikonfirmasi ya."`

Template tagihan sengaja hidup di memori, bukan di kode: mengganti kalimat tagihan adalah mengubah
data workspace, bukan menunggu rilis.

### Bolu

Template `oren` (peran penjualan) dipakai apa adanya — ia sudah membawa alat dan persona yang
mengarahkan alurnya (`order.find_product`, `order.record_order`, `order.read_orders`, plus alat
kanal keluar). Membuat Bolu dari template ini adalah satu panggilan `POST /api/agents` dengan
`template_key: "oren"`, yang juga bukti bahwa menambah Bolu tidak butuh kode baru.

## Langkah dan hasil yang diharapkan

| # | Langkah | Hasil yang diharapkan |
| --- | --- | --- |
| 1 | Pelanggan menulis: "Saya mau pesan 2 kg Kopi Arabika ya, kirim ke Jalan Melati 3." | Satu tugas dibuka dari pesan itu; Bolu memanggil `order.find_product` lalu `order.record_order` |
| 2 | — | `GET /api/orders` berisi satu pesanan: kode `P-XXXXXX`, `customer_phone` nomor pengirim, baris `Kopi Arabika`, satuan `kg`, jumlah `2`, harga satuan `15000`, total baris `30000`, total pesanan `30000`; `agent_id` dan `task_id` terisi |
| 3 | `GET /api/tasks/:id/steps` | Rounds terekam berurutan: think, tool_call, tool_result, think, tool_call, tool_result, think, final. Argumen `order.record_order` hanya memuat nama produk dan jumlah — tidak ada harga |
| 4 | Pelanggan menulis produk yang tidak ada: "Tambah 1 Kopi Arabika Gayo 1 kg ya." | Panggilan `order.record_order` ditolak; hasilnya menyebut produk yang diminta dan apa yang katalog punya (`Kopi Arabika`); **jumlah pesanan tetap satu** dan baris pelanggan tidak bertambah |
| 5 | Pelanggan menulis: "Pesan Kopi Arabika ya, jumlahnya nyusul." | Panggilan `order.record_order` ditolak karena jumlahnya nol; hasilnya menyebut barisnya dan bahwa jumlahnya harus lebih dari nol; **jumlah pesanan tetap satu** |
| 6 | Pelanggan menulis: "Tolong susun tagihannya ya." | Bolu membaca pesanan (`order.read_orders`) lalu memanggil alat kirim; muncul **satu draf** dengan status `pending` |
| 7 | `GET /api/drafts/:id` | `action_kind` berakhir `.send`/`.send_message`, `payload.to` terisi, payload memuat kode pesanan dan total `30000`, `sent_at` kosong |
| 8 | — | Tidak ada pesan yang sampai ke penyedia/kanal keluar: gate adalah yang berdiri antara Bolu menyusun pesan dan pelanggan menerimanya |
| 9 | `GET /api/tasks/:id` tugas tagihan | Status `waiting_approval`, `health` `waiting`, `waiting_reason` terisi |
| 10 | Klik setujui di layar, atau `POST /api/drafts/:id/approve` | Draf menjadi `sent`/`approved` dan tugas selesai — hanya kalau aksinya benar-benar dikerjakan |
| 11 | `GET /api/audit?limit=10` | `draft.created` dan `draft.approved` ada, masing-masing sekali per draf, dengan `actor_user_id` pada persetujuannya |

Langkah 1-9 dijalankan otomatis oleh smoke (fase 1 sampai 4). Langkah 10-11 dijalankan otomatis juga
(fase 5), terhadap penyedia yang di-stub — jadi yang dibuktikan adalah mekanikanya: keputusan itu
dibawa ke executor, executor menjalankan aksinya, dan hasilnya dicatat sebagai terkirim beserta
jejaknya. Yang belum terbukti adalah sisi penyedia yang sungguhan, dan itu bagian staging.

### Kasus yang harus tetap benar

- **Pesanan tidak lengkap.** Pelanggan menyebut produk tanpa jumlah: Bolu bertanya, tidak mencatat
  jumlah yang dikarangnya. Kalau ia mencatat, langkah 2 akan menunjukkan baris dengan jumlah yang
  tidak pernah disebut pelanggan.
- **Harga tidak ditemukan.** Produk ada di katalog dengan harga 0: pesanannya tetap tercatat apa
  adanya, dan harga 0 itu berasal dari katalog — bukan dari model yang menebak.
- **Kontak baru.** Nomor yang belum pernah memesan tetap tercatat (satu baris `customers` baru
  untuk nomor itu), dan pesanan kedua dari nomor yang sama menunjuk baris pelanggan yang sama.

## Bentuk yang bisa dijalankan

`make smoke-order` menjalankan langkah 1-11 di atas terhadap stack compose, dengan stub model dan
stub penyedia email, dan menghentikan langkahnya di titik yang sama setiap kali:

- fase 1 — pesanan tercatat dengan harga katalog (assertion per field terhadap katalog, termasuk
  membaca kembali argumen yang dikirim model untuk membuktikan tidak ada harga di dalamnya);
- fase 2 — produk yang tidak ada menolak seluruh pesanan dan jumlah pesanan tidak bertambah;
- fase 3 — pesanan yang jumlahnya belum jelas ditolak dan ditanyakan, dan tidak ada pesanan yang
  tertulis sebagian;
- fase 4 — tagihan menjadi draf dengan total dan kode pesanan, muncul sebagai kartu di thread, tugas
  parkir, dan penyedia belum menerima apa pun;
- fase 5 — draf disetujui, tugasnya selesai sendiri, drafnya menjadi `sent`, dan penyedia menerima
  **tepat satu** pesan yang memuat kode pesanan, barisnya, dan totalnya — dengan keputusan itu di
  jejak audit beserta pelakunya.

Skenario ini bisa dijalankan ulang oleh siapa pun tanpa bertanya: `make smoke-order` menghasilkan
workspace baru setiap kali, dan data ujinya tidak bercampur dengan data lain karena seluruhnya
berada di workspace itu.

## Kualitas keluaran (E14.4)

Template `oren` menyebut alurnya secara eksplisit, dan tiga hal di dalamnya adalah janji produk
bukan selera gaya: harga selalu dari katalog, tagihan keluar sebagai pesan yang menunggu
persetujuan, dan pesanan yang tidak lengkap atau produknya tidak dikenal ditanyakan alih-alih
ditebak. Kalimat itu tinggal di `agent_templates`, jadi workspace bisa menggantinya tanpa rilis —
dan karena itu pula kualitas keluarannya diukur dari data, bukan dari kode:

| Metrik | Definisi | Ambang awal |
| --- | --- | --- |
| Draf disetujui tanpa revisi | draf dengan `status = 'approved'` dan `revision = 0`, dibagi seluruh draf yang sudah diputuskan | ≥ 70% |
| Pesanan yang gagal dicatat | tugas `order.record_order` yang berakhir gagal, dibagi tugas yang memanggilnya | ≤ 5% |
| Harga yang tidak berasal dari katalog | baris yang `unit_price_micros`-nya tidak sama dengan harga produk saat itu | 0 (dijamin skema, diperiksa sebagai keselamatan) |

Angka pertama dan kedua dibaca dari tabel yang sudah ada — `drafts` dan `task_steps` — jadi tidak ada
metrik baru yang perlu ditulis untuk mengukurnya. Ambangnya sengaja ditaruh di sini sebagai angka
awal yang bisa diperdebatkan, bukan sebagai target yang dijanjikan ke pelanggan.

Yang sudah tercatat di compose sesudah beberapa jalan `make smoke-order`: **2 dari 2 draf yang sudah
diputuskan disetujui tanpa revisi** (`revision = 0`). Angka itu terlalu kecil untuk berarti apa-apa —
tiap jalan smoke hanya memutuskan satu draf, dan yang menyetujuinya adalah skripnya sendiri — jadi ia
dicatat sebagai "metriknya bisa dibaca", bukan sebagai kualitas. Yang bisa dibaca dari lima draf di
database itu: dua `sent`, tiga masih `pending` karena jalan yang menghasilkannya berhenti sebelum fase
5, dan salah satu dari yang `pending` memuat `None` pada daftar barisnya — bug stub yang sudah
diperbaiki, dan alasan kenapa barisnya sekarang dibaca dari kunci yang benar
(`jumlah`/`satuan`/`produk`, nama yang dipakai adapter, bukan `quantity`/`unit`/`description`).

## Contoh percakapan (E14.4)

Transkrip di bawah ini dikutip apa adanya dari satu jalan `make smoke-order` terhadap stack compose:
satu workspace baru, template `oren` dipasang lewat API, lalu alurnya dijalankan. Yang perlu
diketahui sebelum membacanya sebagai acuan: **yang menyusun kalimat balasannya adalah stub, bukan
model** — jadi yang jadi acuan di sini adalah *alurnya* (kapan alat dipanggil, urutan round, dan apa
yang harus dimuat setiap jawaban), bukan pilihan katanya. Kalimatnya sendiri akan berbeda di staging,
dan itu justru yang diukur E14.4.

```
Pelanggan   Saya mau pesan 2 kg Kopi Arabika ya, kirim ke Jalan Melati 3.
Oren        Baik Bu Sari, pesanannya sudah kami catat: 2 kg Kopi Arabika, total Rp 30000. Terima kasih.
Pelanggan   Tambah 1 Kopi Arabika Gayo 1 kg ya.
Oren        Produk itu belum ada di katalog kami, Bu. Mau saya catat produk lain?
Pelanggan   Pesan Kopi Arabika ya, jumlahnya nyusul.
Oren        Maaf Bu, jumlahnya berapa ya? Saya catat setelah jelas.
Pelanggan   Tolong susun tagihannya ya.
Oren        [kartu] GMAIL.SEND · Tagihan pesanan P-EBD33D → Bu Sari, tagihan pesanan P-EBD33D:
            2 kg Kopi Arabika - total Rp 30000. Mohon dikonfirmasi ya.
            [Setujui] [Minta revisi] [Batalkan]
```

Yang dibuktikan setiap balasan, dan karena itu yang harus tetap benar kalau promptnya diganti:

| Balasan | Yang harus benar | Diperiksa oleh |
| --- | --- | --- |
| Kalimat pertama | totalnya datang dari katalog, bukan dari model | fase 1: total pesanan `30000` dan harga satuan `15000` di-assert terhadap baris katalog |
| Kalimat kedua | produk yang tidak ada **dinyatakan** tidak ada, dan yang ditawarkan adalah yang katalog punya | fase 2: hasilnya memuat "Kopi Arabika" dan kode pesanan tidak bertambah |
| Kalimat ketiga | jumlah yang belum jelas **ditanyakan**, bukan ditebak | fase 3: panggilan `record_order` ditolak karena jumlahnya nol, dan jumlah pesanan tetap satu |
| Kartu terakhir | tagihannya memuat kode pesanan dan total, dan menunggu keputusan | fase 4: drafnya `pending`, payload memuat keduanya, dan penyedia belum menerima apa pun |

Satu kualitas yang **tidak** dijaga alurnya: kalimatnya. Stub bisa menulis apa saja, dan model
sungguhan bisa menulis tagihan yang membingungkan meski angkanya benar — itulah yang diukur tingkat
persetujuan tanpa revisi di staging, dan tidak ada test di repo ini yang bisa menggantikannya.

## Gerbang 1: apa yang diputuskan (E14.6)

Issue #118 meminta keputusan yang bersandar pada data, dengan tiga kriteria yang sudah disepakati:
draf disetujui tanpa revisi, biaya per tugas, dan tugas gagal per integrasi. Angka-angka itu ada di
tabel yang sudah dipakai produk ini, jadi `make gate-report` (`scripts/gate-report.sh`) membacanya
dan mencetaknya beserta ambangnya masing-masing:

| Yang dibaca | Sumber | Ambang |
| --- | --- | --- |
| Tugas selesai / gagal / dibatalkan / masih hidup | `tasks` | — |
| Tugas gagal per integrasi | `task_steps` (`kind = 'tool_result'`, hasilnya memuat `error`) | ≤ 5% dari panggilan alat integrasi itu |
| Draf disetujui tanpa revisi | `drafts` dan `draft_revisions` | ≥ 70% dari yang sudah diputuskan |
| Biaya & token, per tugas dan per pesanan | `usage_ledger`, `tasks`, `orders` | — (bahan untuk harga paket) |

Skripnya **melaporkan**, bukan menggagalkan: ini laporan untuk sebuah keputusan. Yang ia jaga adalah
definisinya — rumus tiap angka tinggal di satu tempat, supaya versi pertama dan versi keempat dari
pertanyaan yang sama tidak menghasilkan angka yang berbeda.

Dijalankan terhadap stack demo (provider stub) sesudah satu alur penuh, ia mencetak:

```
Tugas
  total 4  ·  selesai 3  ·  gagal 0  ·  dibatalkan 0  ·  masih hidup 1
Tugas gagal per integrasi  (ambang: ≤ 5% dari panggilan alatnya)
  order      6 panggilan  ·  0 gagal  ·  0.0%  ->  lolos
Draf disetujui tanpa revisi  (ambang: ≥ 70% dari yang sudah diputuskan)
  diputuskan 0  ·  bersih 0  ·  revisi 0  ·  0%  ->  belum ada yang diputuskan
Biaya & token
  ledger: 17 panggilan (10 agen, 7 lain)  ·  3472 token masuk  ·  464 token keluar  ·  85 micro-rupiah
  per tugas rata-rata: 2.7 panggilan  ·  928 token  ·  0 micro-rupiah
  per pesanan tercatat: 2 pesanan  ·  2096 token  ·  0 micro-rupiah
Pesanan
  2 pesanan  ·  2 pelanggan  ·  total 42000 rupiah
```

Yang perlu dibaca dari situ, dan yang tidak: **tugas gagal per integrasi lolos dengan 0%** pada enam
panggilan `order.*`; **draf belum ada yang diputuskan**, karena satu-satunya draf di run itu masih
menunggu dan yang menyetujui di smoke adalah skripnya sendiri; dan **kolom rupiahnya belum berarti**,
karena angka itu tabel harga platform dikalikan token stub.

**Keputusannya sendiri belum diambil.** Itu milik pemilik produk, dan #118 menulis bahwa epic fase
berikutnya hanya dibuat setelah keputusan ini. Yang bisa disiapkan dari sini sudah disiapkan:
angkanya bisa dibaca, definisinya terkunci, dan yang tidak bisa dijawab di lokal disebut apa adanya.

## Biaya per tugas (E14.5)

Angka yang diminta task itu diambil dari `usage_ledger`, bukan dari perkiraan: satu tugas
satu baris per panggilan model, dengan token dan biaya micro-rupiah.

| Yang diukur | Cara mengukurnya |
| --- | --- |
| Token & biaya per tugas | `SELECT task_id, sum(input_tokens), sum(output_tokens), sum(cost_micros) FROM usage_ledger WHERE workspace_id = ... GROUP BY task_id` |
| Biaya per pesanan | tugas yang sama, dijumlahkan per pesanan yang ia catat di `orders` |
| Draf disetujui tanpa revisi | `drafts` per lokasi: bandingkan jumlah `status = 'approved'` dengan jumlah `draft_revisions` |
| Latensi sampai draf siap | selisih `drafts.created_at` dan pesan pemicunya |

Dua angka terakhir diukur oleh `make measure-orders`, dan ia membandingkan separuh pertama dengan
separuh kedua dari jalan yang sama: harga per tugas yang naik seiring jumlah tugas akan terlihat di
sana sebelum ia menjadi tagihan yang salah.

Dua pengukuran sudah dijalankan terhadap stack compose, dan angkanya dibaca dari `usage_ledger`
lewat laporan produk sendiri — bukan perkiraan. Yang **belum** bisa dipakai sebagai dasar harga paket
adalah kolom rupiahnya: stub model dihargai nyaris nol, jadi angka uang yang bermakna hanya datang
dari staging dengan provider sungguhan. Yang bisa dipakai sekarang adalah bentuk tugasnya — berapa
panggilan, berapa token, dan apakah biayanya tumbuh seiring jumlah pesanan.

**Satu alur, satu jalan** (`make smoke-order`, satu pesanan sampai draf tagihan):

| Skenario | Panggilan model | Token masuk | Token keluar | Biaya tercatat |
| --- | --- | --- | --- | --- |
| Pesan pelanggan → pesanan tercatat (fase 1: `find_product`, `record_order`, jawaban) | 3 | 920 | 128 | 23 micro-rupiah (stub) |
| Pesan pelanggan → produk tidak ada (fase 2: `record_order` ditolak, jawaban) | 2 | 600 | 88 | 15 micro-rupiah (stub) |
| Pesan pelanggan → jumlahnya belum jelas (fase 3: `record_order` ditolak, jawaban) | 2 | 600 | 88 | 15 micro-rupiah (stub) |
| Pesan pelanggan → tagihan menjadi draf (fase 4: `read_orders`, `send` yang menunggu) | 2 | 640 | 80 | 16 micro-rupiah (stub) |
| Di luar tugas: ekstraksi memori (`purpose = extraction`, termasuk embedding-nya) | 2 | — | — | terpisah dari perhitungan di atas |

**Dua puluh pesanan berturut-turut** (`make measure-orders 20`, satu per satu sampai masing-masing
tercatat, lalu dibaca per tugas):

| Yang diukur | Hasil |
| --- | --- |
| Pesanan tercatat | 20 dari 20, tanpa intervensi manual |
| Panggilan model per pesanan | 3 (`find_product`, `record_order`, jawaban) — mode, tanpa sebaran |
| Token per pesanan | 1.048 — seluruh dua puluh sama |
| Total 20 pesanan | 60 panggilan, 20.960 token |
| Biaya tercatat | 23 micro-rupiah per pesanan (stub) |
| Latensi | 24 detik untuk 20 pesanan, **1,2 detik per pesanan** ujung ke ujung |
| Akumulasi | separuh kedua 100% dari separuh pertama — tidak ada yang menumpuk per pesanan |

Angka token yang identik itu bukan kebetulan yang bagus melainkan sifat stubbingnya: stub membalas
dengan jumlah token yang sama setiap kali, jadi yang diukur skrip ini adalah **bentuk** alurnya —
tiga panggilan, urutan round, dan tidak adanya akumulasi. Kalau history atau memori menumpuk per
tugas, separuh kedua akan lebih mahal daripada separuh pertama; skripnya membandingkan itu, dan
gagal kalau pertumbuhannya lebih dari 1,5×. Yang tidak diukur skrip ini adalah variasi keluaran model
sungguhan, dan itulah yang dibawa staging: prompt yang sama bisa memakan token yang berbeda, dan hanya
provider sungguhan yang tahu berapa.

Yang terlihat dari bentuknya, dan layak jadi catatan tindak lanjut E14.5:

- **Prompt naik seiring umur percakapan.** Panggilan pertama satu tugas sudah ~320 token masuk karena
  blok instruksi memuat persona, gaya bahasa, memori utama workspace, dan skema alat — dengan tiga
  alat `order.*` plus alat kanal keluar, skema itu sendiri bagian yang tumbuh. Percakapan yang
  panjang menambah riwayat di atasnya; pengukuran dua puluh pesanan di atas memakai percakapan yang
  sama untuk semuanya, dan tokennya tidak naik, karena setiap pesan membuka tugas baru alih-alih
  menambah ke percakapan tugas sebelumnya.
- **Round kedua menduplikasi seluruh percakapan.** Setiap panggilan membawa lagi instruksi,
  riwayat, dan hasil alat sebelumnya, jadi biaya satu tugas tumbuh kuadratik terhadap jumlah round —
  bukan linear. Itu alasan utama `TASK_MAX_STEPS` ada, dan alasan kedua untuk memangkas hasil alat
  yang panjang (`TASK_MAX_TOOL_RESULT_BYTES`) sebelum menaikkan batas step.
- **Fase yang gagal lebih murah, bukan lebih mahal.** Penolakan mengakhiri tugas di round kedua, jadi
  produk yang tidak ada adalah tugas 600 token, bukan tugas yang berputar mencari nama lain.

Angka biaya dalam rupiah dihitung dari tabel harga provider yang dipakai workspace — satu-satunya
tempat harga per token hidup — sehingga dokumen ini tidak menyalin tabel itu ke tempat kedua:
laporan penggunaan yang sudah menghitungnya, dan `scripts/check-usage.py` yang memeriksanya.
