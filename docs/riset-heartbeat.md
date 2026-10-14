# Riset — Heartbeat: bagaimana Bolu tahu sebuah tugas masih hidup

Dokumen ini menjawab satu pertanyaan: **apa yang sebenarnya "heartbeat" di Bolu, di mana ia
ditulis, siapa yang membacanya, dan di mana ia berbohong?** Ia ditulis untuk dibaca sebelum
menyentuh apa pun yang berhubungan dengan liveness — ambang "terhenti", probe worker, atau
pemulihan tugas yang ditinggalkan worker yang mati.

Terakhir diperbarui: 2026-10-14.

## Ringkasan

1. **Bolu punya lima detak yang berbeda, bukan satu.** Detak tugas (`tasks.heartbeat_at`), probe
   proses (`/health`, `/ready`), ping worker (`WorkerPingWorkflow`), detak nomor WhatsApp
   (`whatsapp_numbers.last_seen_at`), dan keep-alive stream realtime (25 detik). Empat yang pertama
   menjawab "apakah ini hidup", yang terakhir hanya menjaga koneksi tidak ditutup proxy. Menyamakan
   kelimanya adalah kesalahan yang paling mudah terjadi.
2. **Detak tugas ditulis sekali per ronde, tetapi ambang "terhenti" lebih pendek daripada satu
   ronde yang sah.** `stuckAfter` = 5 menit, sementara satu panggilan model boleh berjalan sampai
   `modelTimeout` = 5 menit dan detak baru ditulis *setelah* ronde selesai. Sebuah tugas yang sehat
   sedang menunggu jawaban model yang lambat akan terbaca `stuck` sebelum ronde itu selesai.
3. **Yang memulihkan tugas bukan detak, melainkan Temporal.** Detak hanya dipakai UI untuk
   membedakan "sedang bekerja" dari "worker-nya mati". Pemulihannya sendiri datang dari retry
   activity Temporal (4 percobaan untuk panggilan model), jadi label `stuck` biasanya sementara —
   kecuali percobaannya sudah habis atau tidak ada workflow yang pernah dimulai.
4. **Ada lubang yang tidak tertutup: tugas `queued` tanpa workflow.** Constraint
   `tasks_started_has_workflow` hanya mengikat status `running`/`waiting_approval`, jadi proses yang
   mati antara `CreateTask` dan `StartWorkflow` meninggalkan tugas `queued` tanpa workflow — dan
   tidak ada apa pun yang menyapu, memulai ulang, atau menggagalkannya.
5. **"Terhenti" tidak pernah jadi sinyal operasional.** Ia dihitung saat dibaca (`Health(now)`),
   tidak disimpan, dan tidak ada metriknya. Workspace yang tidak dibuka tidak akan pernah memberi
   tahu bahwa tugasnya terhenti.

## Metode dan batasnya

Riset ini adalah pembacaan kode di repositori, bukan riset pasar: setiap klaim di bawah menunjuk
berkas dan baris yang bisa diperiksa ulang. Yang dibaca:

- `apps/backend/internal/modules/task/domain/task.go` — `stuckAfter`, `Health`, `Live`.
- `apps/backend/internal/modules/task/workflow/workflow.go` — timeout per aktivitas, `DefaultRetry`,
  `ToolRetry`, `beat`, dan tempat `beat` dipanggil.
- `apps/backend/internal/modules/task/activity/activity.go` — `BeatTask`.
- `apps/backend/internal/modules/task/repository/queries/task.sql` — di mana `heartbeat_at` ditulis.
- `apps/backend/internal/modules/task/handler/attention.go` — daftar "perlu perhatian".
- `apps/backend/internal/container/ops_server.go` — `/health`, `/ready`, `/metrics`.
- `apps/backend/internal/platform/temporal/ping.go` — `WorkerPingWorkflow`.
- `apps/backend/internal/modules/whatsapp/repository/queries/whatsapp.sql` — `last_seen_at`.
- `apps/backend/internal/modules/chat/domain/event.go` — `Heartbeat` stream.
- `apps/frontend/src/lib/dashboard-view.ts`, `docs/api.md` — bagaimana `health` dibaca manusia.

Batasnya: dokumen ini **tidak** mengukur perilaku di produksi. Semua angka ambang diambil dari
konstanta kode, bukan dari data nyata berapa lama panggilan model sebenarnya berlangsung. Bila
penyedia model sering memakai 3–5 menit untuk satu jawaban, temuan nomor 2 berubah dari teoretis
menjadi harian — dan itu perlu diukur, bukan diasumsikan.

## 1. Apa yang sudah ada

| Detak | Ditulis oleh | Irama | Dibaca oleh | Yang dibuktikan |
| --- | --- | --- | --- | --- |
| `tasks.heartbeat_at` | `StartTask`, `ProgressTask`, `ParkTask`, `ResumeTask`, `FinishTask` | sekali per ronde | `Task.Health`, `/tasks/attention`, dashboard | tugas ini bergerak, bukan ditinggalkan |
| `/health`, `/ready` | proses itu sendiri | tiap probe | orkestrator | proses hidup (`/health`) dan siap kerja (`/ready`) |
| `WorkerPingWorkflow` | pemanggilnya (verifikasi deploy, tes) | atas permintaan | EPIC 14, testsuite | worker ini punya task queue yang hidup |
| `whatsapp_numbers.last_seen_at` | gateway lewat `Touch` | tiap laporan status | handler Integrasi | nomor ini masih terlihat gateway |
| Keep-alive stream | `chat/handler` | tiap 25 detik | klien realtime, proxy | koneksi ini belum ditutup |

### 1.1 Detak tugas

`heartbeat_at` lahir di migrasi `000006_task_runtime.up.sql`, dan alasan kolomnya ada ditulis di
sana: status saja tidak bisa membedakan tugas yang bekerja dari tugas yang worker-nya mati, karena
keduanya berbunyi `running`.

Yang menulisnya:

- `StartTask` — saat tugas mulai berjalan, dan saat `started_at` pertama kali diisi.
- `ProgressTask` — inilah "detak"-nya. Komentarnya di `task.sql` menyebut ia sengaja ditulis
  bersama total berjalan (token, biaya, langkah), bukan sendirian.
- `ParkTask` / `ResumeTask` — saat tugas diparkir menunggu manusia dan saat dilanjutkan.
- `FinishTask` — supaya tugas yang selesai tidak pernah terlihat basi.

Yang membaca:

- `Task.Health(now)` di `domain/task.go`:
  `!Live()` → `done`; `waiting_approval` → `waiting`; `heartbeat_at` kosong atau lebih tua dari
  `stuckAfter` → `stuck` (kecuali `queued` yang masih di dalam `stuckAfter` dari `created_at`,
  yang tetap `running`); selain itu `running`.
- `GET /tasks/attention` — satu pertanyaan ("apa yang menunggu saya?"), menggabungkan `failed`,
  `waiting`, dan `stuck`, dan hanya untuk `stuck` ia menambahkan tautan Temporal UI supaya bisa
  didiagnosis.
- Dashboard (`dashboard-view.ts`) memetakan `stuck` menjadi label "Terhenti" dengan catatan
  "detaknya berhenti; mungkin worker-nya mati".
- Indeks parsial `tasks_live_idx` pada `(workspace_id, heartbeat_at)` ada persis untuk pembacaan ini.

`stuckAfter` adalah `5 * time.Minute`, dan itu satu-satunya tempat ambangnya didefinisikan —
`Health` tidak menerima parameter ambang.

### 1.2 Probe proses

`ops_server.go` menegaskan pemisahan yang benar dan layak dipertahankan:

- `/health` adalah liveness: tidak pernah menyentuh dependensi, dan menjawab selama proses hidup —
  pertanyaan yang dipakai untuk memutuskan restart.
- `/ready` adalah readiness: memeriksa setiap dependensi yang dikonfigurasi dan menjawab 503 saat
  ada yang mati — pertanyaan yang dipakai load balancer atau rollout. Komentarnya menyebut alasannya
  secara eksplisit: worker yang tidak bisa menjangkau Postgres tidak boleh diberi tahu bahwa ia siap.
- `/metrics` hanya ada bila deployment menamai exporter.

Listener ini hanya hidup bila `Http.OpsAddress` diisi, jadi deployment yang tidak memakai probe
tidak berubah.

### 1.3 Ping worker

`WorkerPingWorkflow` + `WorkerPingActivity` menjalankan satu aktivitas di task queue worker itu
sendiri dan mengembalikan identitasnya ("`<name> pong`"). Ia terdaftar di `agent-worker` dan
`integration-worker`, dan komentarnya menjelaskan kenapa ia tetap ada: verifikasi deployment (EPIC
14, `#112`) bisa membuktikan sebuah worker terjangkau tanpa membuka tugas nyata — lebih murah
daripada membuat task hanya untuk mencari tahu.

Ini **bukan** detak berkala. Ia hanya berbunyi saat ada yang memanggilnya.

### 1.4 Detak nomor WhatsApp

`TouchWhatsAppNumber` menaikkan `last_seen_at` dengan `GREATEST(COALESCE(last_seen_at, ...), $at)`,
dan komentarnya menyebut alasannya: laporan yang terlambat dari gateway yang sudah digantikan tidak
boleh memundurkan "apakah ia hidup". Nilai ini diekspos handler Integrasi sebagai `last_seen_at`,
"which is what tells a person whether the connection is actually alive".

### 1.5 Keep-alive stream realtime

`chat/domain/event.go` mendefinisikan `Heartbeat = 25 * time.Second`: "how often a live stream sends
a keep-alive, so an idle connection is not closed by a proxy between the client and the API".
Handler chat memasang ticker itu. Yang penting dibedakan: ini menjaga *koneksi*, bukan membuktikan
*tugas* hidup.

Selain itu ada detak tingkat aplikasi yang lewat jalur yang sama: setiap `BeatTask` memancarkan
`task.running`, dan komentarnya menyebut ia "is what moves a Bolu in the dashboard without the
client polling". Jadi dashboard yang hidup digerakkan detak tugas, bukan timer sendiri.

## 2. Celah yang ditemukan

### F1. Ambang "terhenti" lebih pendek daripada satu ronde yang sah (paling penting)

`beat` dipanggil sekali per ronde, *setelah* `oneRound` selesai — dan satu ronde berisi panggilan
model (`modelTimeout` = 5 menit), lalu tiap tool (`toolTimeout` = 2 menit), lalu penulisan langkah.
Artinya jarak antara dua detak bisa melebihi `stuckAfter` = 5 menit pada tugas yang sama sekali
sehat. Sebuah tugas yang sedang menunggu jawaban model selama 4 menit 50 detik, lalu model butuh
20 detik lagi, sudah melewati ambang sebelum ronde berikutnya sempat menulis detak.

Akibatnya bukan sekadar salah label: dashboard berkata "detaknya berhenti; mungkin worker-nya mati"
tentang tugas yang worker-nya hidup dan sedang bekerja. Dua konstanta yang saling mengikat
(`stuckAfter` dan `modelTimeout`) didefinisikan di berkas yang berbeda, jadi tidak ada apa pun yang
memaksa keduanya tetap selaras saat salah satunya diubah.

### F2. Yang memulihkan adalah Temporal, bukan detak — dan hanya sampai percobaannya habis

Ini bukan bug, tetapi penting agar dokumen ini tidak menyesatkan pembacanya. Bila worker mati di
tengah panggilan model, Temporal menunggu `StartToCloseTimeout` lalu menjadwalkan ulang aktivitas
itu; `DefaultRetry` memberi 4 percobaan dengan backoff 1→30 detik. Jadi label `stuck` untuk
kematian worker biasanya sembuh sendiri ketika ada worker yang hidup lagi. Yang tidak sembuh:

- percobaan habis (4×) — aktivitas gagal, dan `state.fail` menutup tugas dengan `failed`;
- tidak ada workflow sama sekali (lihat F4).

Konsekuensinya: `stuck` adalah **kebenaran UI**, bukan mekanisme pemulihan. Setiap perubahan pada
detak harus berhenti menganggap ia bisa "menyelamatkan" tugas.

### F3. "Terhenti" tidak pernah menjadi sinyal operasional

`Health` dihitung saat dibaca dan tidak disimpan. Metrik yang ada (`platform/metrics/recorder.go`)
mencakup `task_finished`, token, biaya, durasi, `task_dispatched`, dan `task_settled` — tidak ada
metrik atau gauge untuk tugas yang terhenti, dan tidak ada ambang alert. Workspace yang tidak
dibuka tidak akan pernah memberi tahu bahwa ada tugas yang detaknya berhenti; satu-satunya jalan
adalah seseorang membuka dashboard atau `/tasks/attention`.

### F4. Tugas `queued` tanpa workflow tidak punya penyapu

Constraint `tasks_started_has_workflow` hanya berlaku untuk `running` dan `waiting_approval`. Tugas
`queued` boleh — dan memang wajar — belum punya workflow. Tetapi tidak ada apa pun yang memeriksa
tugas `queued` yang sudah lama. Bila API mati di antara `CreateTask` dan `StartWorkflow`, tugas itu
tinggal `queued` selamanya: `Health` akan menyebutnya `stuck` setelah 5 menit (karena
`created_at`-nya sudah lewat ambang), tetapi tidak ada reaper yang memulai workflow-nya, tidak ada
yang menggagalkannya dengan alasan yang dibaca pengguna, dan tidak ada yang memberitahu.

### F5. Tugas yang terhenti menahan operasi lain

`CountLiveTasksForAgent` menghitung tugas dengan status `queued`/`running`/`waiting_approval`, dan
registry membacanya sebelum menghapus sebuah Bolu. Tugas yang terhenti ikut terhitung, jadi Bolu
yang punya satu tugas terhenti tidak bisa dihapus sampai seseorang menyelesaikan tugas itu lebih
dulu. Ini dampak yang wajar dari definisi "hidup", tetapi layak diketahui sebelum orang menyangka
penghapusan Bolunya rusak.

### F6. `waiting_approval` sengaja tidak pernah basi — dan itu harus dijaga

`Health` memeriksa `waiting_approval` sebelum memeriksa umur detak, jadi tugas yang menunggu
keputusan manusia tidak pernah dianggap terhenti. Itu benar: tugas yang diparkir belum berhenti.
Tetapi ada dua hal yang mengikutinya:

- detaknya memang sengaja tua (ditulis saat park), jadi setiap penyapu "detak basi" di masa depan
  **wajib** mengecualikan `waiting_approval`, kalau tidak ia akan membunuh tugas yang justru sedang
  menunggu orang;
- tidak ada pengingat atau kedaluwarsa untuk tunggu yang panjang. Satu-satunya permukaan yang
  menyebutnya adalah daftar perhatian.

### F7. Nomor WhatsApp tidak punya "health" turunan

`last_seen_at` bergerak maju saja (itu benar), tetapi `status` hanya berubah bila gateway melapor.
Gateway yang mati diam-diam meninggalkan status `connected` dengan `last_seen_at` yang tua, dan
tidak ada pembacaan turunan yang menyebutnya "tidak sehat" — berbeda dari tugas, yang punya
`Health` turunan dari detak. Angkanya ada, tetapi tidak ada yang menerjemahkannya menjadi keadaan.

### F8. Ping worker tidak berkala, jadi kematian satu armada tidak terlihat sebagai kematian armada

Karena `WorkerPingWorkflow` hanya berbunyi saat dipanggil, tidak ada catatan berkelanjutan "worker
ini hidup". Worker yang mati hanya bisa disimpulkan dari detak tugas yang menua — yang berarti
sebuah deployment yang seluruh worker-nya mati tidak terlihat sampai ada tugas yang berjalan. Selama
tidak ada tugas, tidak ada sinyal. (Untuk beban yang sedang berjalan, `/ready` menutup sebagian
celah ini, tetapi ia menjawab per-proses, bukan "apakah ada worker yang mengambil pekerjaan".)

### F9. Angka keep-alive belum diikatkan ke batas proxy deployment

25 detik aman untuk proxy dengan idle timeout 60 detik, tetapi konstanta itu hidup di kode tanpa
catatan berapa idle timeout yang diharapkan dari deployment (nginx, Cloudflare, load balancer
Easypanel). Sama seperti F1, tidak ada yang mengikat kedua angka itu.

## 3. Rekomendasi

Berurut dari yang paling murah dan paling menahan.

**P0 — selaraskan irama detak dengan ambangnya.** Entah tulis detak di awal ronde (sebelum
panggilan model) *juga*, atau turunkan `stuckAfter` dari timeout workflow itu sendiri sehingga
keduanya tidak bisa berbeda. Yang paling sederhana dan paling jujur: definisikan ambang sebagai
`modelTimeout + toolTimeout + margin` di satu tempat, dan biarkan `Health` menerimanya. Ini menutup
F1 tanpa menambah satu pun tulisan ke database.

**P0 — sapu tugas `queued` tanpa workflow.** Sebuah reaper kecil (irama loop biasa, seperti billing
dan retention yang sudah ada) yang, untuk tugas `queued` tanpa `workflow_id` yang lebih tua dari
ambang, memilih: memulai workflow-nya, atau menutupnya `failed` dengan alasan yang dibaca pengguna.
Ini menutup F4, satu-satunya lubang yang tidak akan pernah sembuh sendiri.

**P1 — jadikan "terhenti" terlihat tanpa harus membuka dashboard.** Metrik sudah punya kosakatanya;
tambahkan gauge seperti `bolu_tasks_stuck` (atau histogram umur detak) dan satu alert di atasnya.
Ini menutup F3, dan merupakan prasyarat sebelum percaya pada klaim apa pun soal ketahanan.

**P1 — tetapkan kebijakan pemulihan, dan tulis di dokumen.** Hari ini jawabannya adalah "manusia
lewat `/tasks/attention` dan Temporal UI". Itu jawaban yang sah untuk sekarang, tetapi ia harus
ditulis sebagai keputusan, bukan sebagai kebetulan — karena begitu metrik F3 ada, pertanyaan
pertamanya adalah "lalu siapa yang bertindak?".

**P1 — jaga `waiting_approval` tetap di luar setiap penyapu detak, dan tambahkan pengingat tunggu.**
Bila kelak ada penyapu, ia wajib melewati `waiting_approval` (F6). Pengingat "menunggu keputusan
sejak X" lebih berguna daripada kedaluwarsa yang mematikan.

**P2 — beri nomor WhatsApp `health` turunan.** `last_seen_at` sudah ada; yang hilang hanya
penerjemahan yang sama seperti tugas (F7). Satu fungsi turunan + satu label di kartu Integrasi.

**P2 — catat harapan idle timeout di samping konstanta keep-alive.** Satu komentar di
`chat/domain/event.go` yang menyebut batas proxy yang diharapkan menutup F9 tanpa kode baru.

**P2 — pertimbangkan presence worker.** Bila kematian armada ingin terlihat sebelum ada tugas, satu
metrik "worker terakhir mengambil pekerjaan pada X" (dari poller Temporal, bukan ping buatan sendiri)
menutup F8.

## 4. Keputusan yang tetap terbuka

- **Pemulihan otomatis atau manual?** Bila otomatis: Temporal *reset*, pengiriman ulang, atau
  `failed` + notifikasi? Dan siapa yang boleh memicunya — orang, atau platform atas namanya?
- **`stuck` itu keadaan pengguna atau keadaan operator?** Hari ini ia tampil di dashboard sebagai
  bahasa manusia ("mungkin worker-nya mati"). Bila ia juga menjadi sinyal alert, kedua pembacanya
  mungkin butuh ambang yang berbeda.
- **Irama detak: per ronde atau lebih halus?** Per ronde berarti paling banyak 12 tulisan per tugas
  (batas `TASK_MAX_STEPS`), murah tetapi kasar. Detak per tool atau per potongan streaming lebih
  halus dan menghapus F1 sepenuhnya, dengan harga tulisan yang lebih banyak. Keputusan ini
  bergantung pada berapa lama panggilan model sebenarnya — yang belum diukur.
- **Apakah presence worker pantas punya tabel sendiri?** Sebuah baris "worker terakhir terlihat"
  akan membuat F8 bisa dijawab tanpa tugas, tetapi juga berarti state runtime baru untuk dirawat.

## Referensi

Internal:

- `apps/backend/migrations/000006_task_runtime.up.sql` — `heartbeat_at`, `tasks_live_idx`,
  `tasks_started_has_workflow`.
- `apps/backend/internal/modules/task/domain/task.go` — `stuckAfter`, `Health`, `Live`.
- `apps/backend/internal/modules/task/workflow/workflow.go` — `modelTimeout`, `toolTimeout`,
  `beatTimeout`, `DefaultRetry`, `beat`.
- `apps/backend/internal/modules/task/activity/activity.go` — `BeatTask`.
- `apps/backend/internal/modules/task/repository/queries/task.sql` — `ProgressTask`, `ParkTask`,
  `CountLiveTasksForAgent`.
- `apps/backend/internal/modules/task/handler/attention.go` — `GET /tasks/attention`.
- `apps/backend/internal/container/ops_server.go` — liveness vs readiness.
- `apps/backend/internal/platform/temporal/ping.go` — `WorkerPingWorkflow`.
- `apps/backend/internal/modules/whatsapp/repository/queries/whatsapp.sql` — `TouchWhatsAppNumber`.
- `apps/backend/internal/modules/chat/domain/event.go` — `Heartbeat`, `EventTaskRunning`.
- `apps/backend/internal/platform/metrics/recorder.go` — metrik tugas yang sudah ada.
- `apps/frontend/src/lib/dashboard-view.ts` — `taskState` dan label "Terhenti".
- `docs/api.md` — tabel `health`; `docs/testing.md` — uji schema & detak nomor WhatsApp;
  `docs/adr/0001-temporal-dan-region.md` — mengapa Temporal Cloud.

Eksternal (untuk keputusan yang belum diambil):

- Temporal — Activity heartbeats dan retry policy: irama detak aktivitas yang berbeda dari detak
  aplikasi, dan cara kerja `StartToCloseTimeout` terhadap worker yang hilang.
- Kubernetes — perbedaan liveness dan readiness probe, yang sudah diikuti `ops_server.go`.
- Dokumentasi proxy/edge yang dipakai deployment (nginx, Cloudflare) — idle timeout yang menjadi
  alasan angka 25 detik ada.
