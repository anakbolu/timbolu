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

## Biaya per tugas (E14.5)

Angka yang diminta task itu diambil dari `usage_ledger`, bukan dari perkiraan: satu tugas
satu baris per panggilan model, dengan token dan biaya micro-rupiah.

| Yang diukur | Cara mengukurnya |
| --- | --- |
| Token & biaya per tugas | `SELECT task_id, sum(input_tokens), sum(output_tokens), sum(cost_micros) FROM usage_ledger WHERE workspace_id = ... GROUP BY task_id` |
| Biaya per pesanan | tugas yang sama, dijumlahkan per pesanan yang ia catat di `orders` |
| Draf disetujui tanpa revisi | `drafts` per lokasi: bandingkan jumlah `status = 'approved'` dengan jumlah `draft_revisions` |
| Latensi sampai draf siap | selisih `drafts.created_at` dan pesan pemicunya |

Satu jalan `make smoke-order` terhadap stack compose sudah diukur, dan angkanya di bawah ini adalah
token yang benar-benar tercatat di `usage_ledger` — bukan perkiraan. Yang **belum** bisa dipakai
sebagai dasar harga paket adalah kolom biayanya: stub model dihargai nyaris nol, jadi angka rupiah
yang bermakna hanya datang dari staging dengan provider sungguhan (yang diminta E14.5). Yang bisa
dipakai sekarang adalah bentuk tugasnya: berapa panggilan dan berapa token.

| Skenario | Panggilan model | Token masuk | Token keluar | Biaya tercatat |
| --- | --- | --- | --- | --- |
| Pesan pelanggan → pesanan tercatat (fase 1: `find_product`, `record_order`, jawaban) | 3 | 920 | 128 | 23 micro-rupiah (stub) |
| Pesan pelanggan → produk tidak ada (fase 2: `record_order` ditolak, jawaban) | 2 | 600 | 88 | 15 micro-rupiah (stub) |
| Pesan pelanggan → jumlahnya belum jelas (fase 3: `record_order` ditolak, jawaban) | 2 | 600 | 88 | 15 micro-rupiah (stub) |
| Pesan pelanggan → tagihan menjadi draf (fase 4: `read_orders`, `send` yang menunggu) | 2 | 640 | 80 | 16 micro-rupiah (stub) |
| Di luar tugas: ekstraksi memori (`purpose = extraction`, termasuk embedding-nya) | 2 | — | — | terpisah dari perhitungan di atas |

Yang terlihat dari bentuknya, dan layak jadi catatan tindak lanjut E14.5:

- **Prompt naik seiring umur percakapan.** Panggilan pertama satu tugas sudah 320 token masuk karena
  blok instruksi memuat persona, gaya bahasa, memori utama workspace, dan skema alat — dengan tiga
  alat `order.*` plus alat kanal keluar, skema itu sendiri bagian yang tumbuh. Percakapan yang
  panjang menambah riwayat di atasnya.
- **Round kedua menduplikasi seluruh percakapan.** Setiap panggilan membawa lagi instruksi,
  riwayat, dan hasil alat sebelumnya, jadi biaya satu tugas tumbuh kuadratik terhadap jumlah round —
  bukan linear. Itu alasan utama `TASK_MAX_STEPS` ada, dan alasan kedua untuk memangkas hasil alat
  yang panjang (`TASK_MAX_TOOL_RESULT_BYTES`) sebelum menaikkan batas step.
- **Fase yang gagal lebih murah, bukan lebih mahal.** Penolakan mengakhiri tugas di round kedua, jadi
  produk yang tidak ada adalah tugas 600 token, bukan tugas yang berputar mencari nama lain.

Angka biaya dalam rupiah dihitung dari tabel harga provider yang dipakai workspace — satu-satunya
tempat harga per token hidup — sehingga dokumen ini tidak menyalin tabel itu ke tempat kedua:
laporan penggunaan yang sudah menghitungnya, dan `scripts/check-usage.py` yang memeriksanya.
