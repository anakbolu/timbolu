# Riset: komputer sandbox dengan browser, berkas, dan terminal untuk Bolu

- Status: **Riset** — belum keputusan, tidak mengubah kode
- Tanggal: 2026-10-14
- Konteks: meninjau ulang isu #4 *"Virtual computer per bot: persistent session, terminal, browser"* dengan teknologi sandbox yang sekarang tersedia
- Terkait: EPIC 8 (#66) integration hub, EPIC 7 (#59) approval & draf, EPIC 6 task runtime, [ADR 0001](adr/0001-temporal-dan-region.md), [ADR 0002](adr/0002-kepatuhan-uu-pdp.md)

## Pertanyaan riset

Kalau Bolu diberi sebuah **komputer sandbox** yang punya **browser**, **berkas**, dan **terminal** —
seperti komputer virtual pada isu #4 dan seperti produk "bot dengan komputer" yang sudah ada di pasar
(mis. Grok Bot/Sand) — apakah itu cocok dengan arsitektur Bolu hari ini, apa syaratnya, apa risikonya,
dan berapa biayanya? Jawabannya berbeda menurut **di mana** ia dijalankan (§4).

Cakupan yang dituju adalah **satu komputer per workspace**, dipakai bersama beberapa Bolu sekaligus
— bukan satu komputer per Bolu seperti isu #4 asli. Perbedaan itu punya konsekuensi konkurensi
sendiri dan dibahas di §6.

Karena isolasi hanya sekuat runtime-nya, dokumen ini juga membandingkan **Easypanel (Docker) dan
Kubernetes**, serta bagaimana masing-masing menyalakan/mematikan sandbox agar hemat resource (§4),
sebelum membahas arsitekturnya.

Dokumen ini mengumpulkan fakta dan memberi rekomendasi; keputusan (dan ADR-nya) menyusul bila
memang mau ditempuh.

## Ringkasan

| Pertanyaan | Jawaban singkat |
| --- | --- |
| Mungkin secara teknis? | **Ya.** Chromium headless + `exec` + volume persisten sudah lazim dan murah — microVM hanya **tidak bisa di Easypanel** (§4). |
| Cocok dengan arsitektur sekarang? | **Hanya bila dibungkus sebagai alat berlabel** lewat jalur MCP (EPIC 8) — bukan sebagai "satu komputer" opaque yang melewati gerbang persetujuan. |
| Boleh pakai cloud sandbox populer? | **Sulit.** Data pelanggan harus di Jakarta (ADR 0001); E2B, Cloudflare, dan Modal tidak punya region Jakarta. |
| Aman mana, dan di mana? | **Easypanel** = Docker Swarm tanpa `--device`/`--privileged`, jadi microVM mati; paling kuat di sana adalah **gVisor** sebagai `default-runtime` host. **Kubernetes** memilih runtime **per Pod** (RuntimeClass), sehingga **gVisor** bisa berdampingan dengan container biasa tanpa KVM (§4.3–4.4). |
| Bisa on/off supaya hemat? | **Ya, di Kubernetes.** Pod sandbox per workspace dengan **KEDA `minReplicaCount: 0`**, state di PVC/snapshot, dan node ikut menyusut — biaya sebanding aktivitas, bukan jumlah workspace (§4.5). |
| Cakupan yang dipilih | **Satu komputer per *workspace***, dipakai bersama beberapa Bolu — §6. |
| Berapa biayanya? | Sandbox **persisten** per Bolu berarti RAM 24/7. Dengan satu komputer per workspace + on/off (§4.5) + snapshot, biaya sebanding aktivitas, bukan jumlah Bolu. |
| Rekomendasi | **Jangan sekarang.** Tunggu Gerbang 1 (E14.6, #118) membuktikan ada tugas nyata yang butuh browser/terminal. Bila ya: **gVisor + Kubernetes** (Opsi C, §4.6) — satu sandbox per workspace, on/off lewat KEDA, dibungkus satu MCP server, semua aksi keluar default draf. |

## 1. Fakta yang mengikat di Bolu hari ini

Riset ini tidak mulai dari nol: keputusan-keputusan berikut sudah mengunci sebagian jawaban.

- **Agen hanya memakai alat yang di-grant.** Sebuah alat mencapai model hanya bila (a) ia ada di
  `tool_catalog`, (b) sesuatu — adapter atau server MCP — bisa menjalankannya, dan (c) Bolu itu
  memang diberi grant. Registry menerapkan kedua saringan, sehingga alat yang tak ada pelaksananya
  tidak pernah ditawarkan.
- **Setiap alat punya label, dan label itulah gerbangnya.** `read` dan `write_internal` berjalan
  tanpa tanya; `write_external` **tidak pernah dijalankan langsung** — ia menjadi draf, tugas
  parkir `waiting_approval`, dan menunggu keputusan manusia tanpa memegang slot worker. Sebuah
  deployment tanpa gerbang persetujuan **menolak** aksinya, bukan menjalankannya.
- **Ada jalur MCP per workspace.** Sebuah server MCP yang ditambahkan workspace memunculkan
  alat-alatnya sebagai `app.tool` dengan label, grant, dan persetujuan yang sama seperti adapter
  native. Ini jalur yang sudah ada untuk memperluas kemampuan tanpa menulis adapter baru.
- **Runtime tugas punya batas keras.** `TASK_MAX_STEPS=12`, `TASK_MAX_TOKENS=120000`,
  `TASK_MAX_TOOL_RESULT_BYTES=16384`, plus batas biaya harian. Setiap ronde, tiap panggilan alat,
  dan tiap hasil menjadi `task_step`. Isi mentah dipangkas retensi 30 hari; ringkasan tetap.
- **Temporal hanya membawa pengenal (claim check).** Isi pekerjaan dibaca dari Postgres di Jakarta,
  bukan disimpan di riwayat workflow. State apa pun yang hidup lama harus punya rumah selain
  Temporal.
- **Region.** Semua yang menyimpan data pelanggan berada di **Jakarta (`ap-southeast-3`)**; hanya
  Temporal Cloud yang di Singapura, dan itu pun tanpa isi.
- **Deployment.** Lima image dari satu repo, dijalankan lewat Easypanel (webhook per service) di
  atas host; `docker-compose.yml` **tidak** memetakan `/dev/kvm`, `--privileged`, atau device apa pun.
- **Isu #4 sudah ditutup sebagai "tidak".** Alasan yang tercatat: arsitektur yang jadi tidak
  memuat konsep komputer virtual; penggantinya adalah "agen memakai alat dari integrasi yang
  di-grant" + "rahasia tersimpan terenkripsi per workspace". Jadi riset ini bukan menghidupkan
  kembali sesuatu yang lupa dikerjakan, melainkan menimbang apakah alasan itu masih berlaku
  sekarang ada microVM yang murah.

## 2. Lanskap teknologi

### 2.1 Lapisan isolasi

| Opsi | Apa itu | Isolasi | Butuh `/dev/kvm`? | Catatan |
| --- | --- | --- | --- | --- |
| **Firecracker** | VMM microVM yang memakai KVM; dipakai AWS Lambda/Fargate | Perangkat keras | **Ya** | Apache-2.0; boot cepat; dipakai juga oleh Kata Containers |
| **Kata Containers** | Container yang dibungkus microVM (Firecracker/Cloud Hypervisor) | Perangkat keras | **Ya** | Apache-2.0; "rasa container, isolasi VM" |
| **microsandbox** | microVM lokal untuk beban tak tepercaya; CLI + SDK (Go/Rust/Python/TS) + MCP | Perangkat keras | **Ya** (Linux); Apple Silicon di macOS | Apache-2.0; boot <100 ms; fork & snapshot; rahasia tak masuk VM; OCI |
| **gVisor** | *Application kernel* di userspace (`runsc`), bukan VM | Kuat, bukan HW | Tidak | Bukan penyaring syscall, bukan VM biasa; integrasi Docker/K8s |
| **Docker + seccomp** | Container biasa | Lemah untuk kode tak tepercaya | Tidak | Bukan pilihan bila isinya benar-benar tak tepercaya |

Intinya: **microVM butuh KVM**. Ini yang pertama harus dicek pada host deployment, karena tidak ada
opsi microVM yang berjalan tanpanya.

### 2.2 Sandbox terkelola (cloud)

| Opsi | Model | Region Jakarta? | Catatan |
| --- | --- | --- | --- |
| **E2B** | Sandbox cloud terkelola, SDK JS/Python | Tidak | Fokus menjalankan kode hasil AI; bagus, tapi di luar Jakarta |
| **Cloudflare Sandbox SDK** | Sandbox = Durable Object + Container | Tidak | Ada `Files`, `S3Mount` (kredensial tak masuk sandbox), `DirectoryBackup`, dan contoh "terminal di browser"; **container, bukan microVM** |
| **Daytona** | "Komputer penuh" + snapshot untuk agen | Tidak | Repo inti **tak lagi dirawat sejak Juni 2026** — risiko adopsi |

Ketiganya memproses data di luar Indonesia, dan itu berbenturan dengan ADR 0001/0002. Opsi ini
realistis **hanya** bila diperlakukan sebagai pemroses dengan dasar yang sama seperti penyedia model —
dan itu keputusan kepatuhan, bukan keputusan teknis.

### 2.3 Lapisan browser, terminal, dan berkas

Yang sebenarnya dibutuhkan "komputer" itu bisa dipisah menjadi tiga, dan tiap-tiapnya punya
jawaban yang sudah matang:

- **Browser.** Chromium headless di dalam sandbox, dikendalikan Playwright (atau CDP langsung).
  **Persistensi sesi** datang dari `storageState` / persistent context di dalam **volume** yang
  melekat pada Bolu — bukan dari proses yang hidup terus. Ini yang membuat "login sekali, lanjut
  besok" mungkin tanpa menjaga tab hidup.
- **Terminal.** `exec` + PTY di dalam sandbox (mis. `kubectl exec`, `msb exec`, atau
  `container.exec`). Ada contoh
  publik "buka shell di browser" (Cloudflare) untuk sisi UI.
- **Berkas.** Volume bernama persisten (bertahan antar-restart) dan/atau mount bucket S3. Di
  microsandbox, volume bernama bertahan lintas sandbox; di Cloudflare, `S3Mount` menandatangani
  tiap permintaan sehingga kredensial tidak masuk sandbox. Untuk Bolu, bucket-nya adalah object
  storage Jakarta yang sudah ada.

## 3. Perbandingan untuk konteks Bolu

| Kriteria | microVM self-host (microsandbox/Firecracker) | Cloud sandbox (E2B/Cloudflare) | gVisor di Kubernetes |
| --- | --- | --- | --- |
| Isolasi | Perangkat keras | Perangkat keras/container | Kuat, bukan HW |
| Data di Jakarta | **Bisa** (host Jakarta) | Umumnya tidak | Bisa |
| Butuh KVM | **Ya** | Tidak | Tidak |
| Persistensi | Volume + snapshot | Backup/mount | PVC + snapshot |
| Kesesuaian arsitektur | Perlu host baru + wrapper alat | Perlu integrasi + keputusan kepatuhan | Paling mudah ditempel, per Pod |
| Biaya | RAM host 24/7 (VM persisten) | Per-detik, tapi keluar region | Murah, dan bisa on/off |
| Kematangan | Sedang (microsandbox muda) | Tinggi | Tinggi |

Rincian host dan runtime ada di §4.

## 4. Di mana ia bisa jalan

Dua hal menentukan apa yang bisa dipakai: **runtime isolasinya** dan **di mana ia dijalankan**.
Host produksi Bolu hari ini adalah Easypanel, dan itu membatasi lebih keras daripada teknologinya
sendiri (§4.1). Kubernetes menghapus batasan itu (§4.3). Fakta berikut diverifikasi dari dokumentasi
Easypanel, Kubernetes, dan referensi Docker (2026-10-14):

| Fakta | Konsekuensi untuk "komputer" |
| --- | --- |
| Easypanel berjalan di atas **Docker Swarm** pada satu server Ubuntu; service dibuat dari panel (App, Compose, Box, database). | Service adalah objek **panel berumur panjang**, bukan container sekali pakai per tenant. |
| Service App menyediakan Resources (limit CPU/RAM), Deploy settings (replicas, **cap_add/cap_drop**, sysctl, supplemental groups), dan Storage (volume/mount). | Limit per-tenant dan hardening dasar **tersedia** dari panel. |
| Tidak ada opsi `--privileged`, `--device`, atau `--security-opt` pada service App. | **`/dev/kvm` tidak bisa dipetakan** → microVM mati di Easypanel. |
| Referensi `docker service create` tidak punya `--device`, `--privileged`, `--runtime`, atau `--security-opt`. | Ini batasan **Swarm**, bukan sekadar UI Easypanel. |
| GPU pun menuntut `daemon.json` + restart Docker di host. | Apa pun yang butuh runtime/device kustom adalah perubahan **tingkat host**, bukan tingkat service. |
| Ada **CLI** dan **MCP server** Easypanel (API key per user). | Control plane bisa membuat service secara programatik — tapi lihat §4.2 soal skala. |

### 4.1 Easypanel (host hari ini)

| Opsi | Jalan di Easypanel? | Cara | Isolasi |
| --- | --- | --- | --- |
| **Container Docker (runc)** | **Ya, native** | Service App/Compose biasa; `cap_drop: ALL`, pengguna non-root, rootfs read-only, limit RAM/CPU | Lemah–sedang; cukup bila isinya belum benar-benar tak tepercaya |
| **gVisor (`runsc`)** | **Bersyarat** | `runsc install` → `default-runtime: runsc` di `daemon.json` host + restart Docker. Swarm tak punya `--runtime` per-service, jadi ini berlaku **host-wide** | Kuat (userspace kernel), tanpa KVM |
| **microVM (Firecracker/Kata/microsandbox)** | **Tidak** | Butuh `--device /dev/kvm` | — |
| **Cloud sandbox (E2B, dll.)** | Ya, tapi di luar Easypanel | Panggilan API dari backend | Tinggi, tetapi keluar Jakarta |

Kesimpulannya: di Easypanel pilihan realistis adalah **container Docker yang dikeraskan**, dan — bila
isolasi perlu naik — **gVisor sebagai `default-runtime` pada node khusus**.

### 4.2 Banyak user mengubah bentuknya

**Satu service Easypanel per workspace tidak skala.** Ratusan atau ribuan workspace berarti ratusan
atau ribuan service di panel, masing-masing dengan domain, riwayat deployment, dan log sendiri.
Easypanel dirancang untuk sejumlah kecil service berumur panjang, bukan container per-tenant yang
lahir dan mati terus-menerus.

Yang skala:

1. **Easypanel tetap meng-host service berumur panjang** — `api`, worker, frontend, dan satu service
   baru **`sandbox-runner`**.
2. **`sandbox-runner` yang mengurus container per workspace** lewat Docker API di **node sandbox
   terpisah**: buat, batasi, hentikan saat idle, hapus. Tidak lewat UI Easypanel.
3. **Jangan pernah** me-mount `docker.sock` ke dalam container tenant — itu akar host bagi siapa pun
   yang berhasil lolos.
4. **Node sandbox dipisah** dari node Easypanel agar ledakan radius dan tekanan disk tidak menyentuh
   API.

Yang harus dibangun sendiri dan **tidak** diberikan Easypanel: siklus hidup (cold start, image pull,
reaper idle), kuota per paket, isolasi jaringan antar-tenant, dan pemulihan state (snapshot/volume
per workspace).

### 4.3 Kubernetes: RuntimeClass mengubah aturannya

Kalau host pindah ke Kubernetes, batasan yang membunuh microVM di Easypanel **hilang**. Sebabnya satu
fitur: **RuntimeClass** memilih runtime *per Pod*, bukan per host.

| Fakta | Konsekuensi untuk "komputer" |
| --- | --- |
| **RuntimeClass** menunjuk runtime OCI lewat handler containerd, dan dipilih **per Pod**. | Pod sandbox bisa memakai gVisor/Kata sementara `api` dan worker tetap `runc`, **di klaster yang sama**. Ini yang tidak bisa dilakukan Docker Swarm. |
| **gVisor** di Kubernetes = RuntimeClass (`runsc`/`gvisor`); **tidak butuh KVM**. | Isolasi kuat tanpa perangkat keras khusus — jalan di VPS biasa. |
| **GKE Sandbox** menyediakan gVisor bawaan (`runtimeClassName: gvisor`). | Di GKE Jakarta, isolasi kuat **tanpa merawat node runtime sendiri**. |
| **Kata Containers** di Kubernetes = RuntimeClass `kata`; **butuh KVM** (`vmx`/`svm`, nested virt bila host-nya VM). | Isolasi level-VM, tetapi tetap menuntut perangkat keras yang sama seperti §4.1. |
| Deployment **tidak** punya *scale-to-zero* bawaan; HPA berhenti di 1 replika. | On/off butuh KEDA (`minReplicaCount: 0`) atau Knative (§4.5). |
| Node bisa ikut menyusut saat Pod habis (cluster-autoscaler/Karpenter). | Penghematan yang nyata ada di **level node**, bukan cuma Pod. |
| Jakarta tersedia: AWS `ap-southeast-3`, GCP `asia-southeast2`, Azure Indonesia Central. | Kepatuhan ADR 0001 tetap terpenuhi. |

### 4.4 Aman mana di Kubernetes?

| Opsi | RuntimeClass | Butuh KVM? | Isolasi | Dingin-start | Catatan |
| --- | --- | --- | --- | --- | --- |
| **gVisor** | `runsc` / `gvisor` | **Tidak** | Kuat (kernel userspace) | Detik (tanpa boot VM) | Paling praktis; dipakai GKE Sandbox, Cloud Run, Cloud Functions |
| **Kata Containers** | `kata` | **Ya** | Terkuat (VM) | Ratusan ms (boot VM) | Perlu node ber-nested virt |
| **Firecracker** (via Kata/containerd) | khusus | **Ya** | Terkuat (microVM) | Ratusan ms | Paling rumit dirawat |
| **runc** (bawaan) | default | Tidak | Lemah | Tercepat | Hanya untuk kode tepercaya |

**Untuk Bolu, gVisor adalah titik seimbangnya.** Ia memberi isolasi yang jauh lebih kuat daripada
container biasa **tanpa** KVM, dipilih per Pod, dan dingin-start-nya cepat — dua hal yang persis
dibutuhkan on/off yang sering. Kata/Firecracker baru masuk akal bila kelak ada tugas yang benar-benar
butuh kernel sendiri; di GKE, gVisor bahkan tidak perlu dirawat sendiri.

Yang **tetap** harus dikerjakan siapa pun runtime-nya: node sandbox dipisah (taint/toleration),
NetworkPolicy antar-tenant, tanpa `docker.sock`/`containerd.sock` di dalam sandbox, dan batas
CPU/RAM per Pod.

### 4.5 On/off supaya hemat resource

Ini yang membuat biaya sebanding aktivitas, bukan jumlah workspace:

1. **Pod sandbox per workspace, dengan `minReplicaCount: 0`** (KEDA) — saat tak ada tugas, Pod
   benar-benar hilang, bukan sekadar idle.
2. **State tinggal di luar Pod**: PVC per workspace (profil browser, berkas) dan/atau snapshot ke
   object storage Jakarta. Pod boleh mati; pekerjaan tidak hilang.
3. **Node ikut turun** lewat cluster-autoscaler/Karpenter saat Pod-nya nol — di sinilah RAM-nya
   benar-benar berhenti dibayar.
4. **Bangun lagi saat dipakai**: permintaan pertama memicu Pod baru; gVisor/runc siap dalam detik.

Dua catatan jujur: **PVC tetap menagih storage** walau Pod mati (snapshot ke object storage lebih
murah untuk idle lama), dan **dingin-start Kata/Firecracker lebih lambat** karena harus boot VM —
alasan lain gVisor lebih cocok untuk pola on/off.

### 4.6 Pilihan arsitektur

| Opsi | Di mana | Isolasi | On/off | Kerumitan | Cocok bila |
| --- | --- | --- | --- | --- | --- |
| **A. Container di Easypanel** | node Easypanel | Lemah–sedang | Manual | Terendah | Tahap 1: browser + berkas, kode belum tak tepercaya |
| **B. gVisor di Easypanel** | node sandbox, `default-runtime: runsc` (host-wide) | Kuat | Manual | Sedang | Tetap di Easypanel tetapi butuh isolasi lebih kuat |
| **C. gVisor di Kubernetes** | RuntimeClass per Pod (mis. GKE Sandbox) | Kuat | **KEDA, per Pod + node** | Sedang–tinggi | **Rekomendasi** — isolasi kuat, tanpa KVM, hemat saat idle |
| **D. Kata/Firecracker di Kubernetes** | RuntimeClass `kata` di node ber-KVM | Terkuat | KEDA (boot VM) | Tertinggi | Ada tugas yang benar-benar butuh kernel sendiri |

Urutan yang masuk akal: **A → C**, dan naik ke **D** hanya bila terbukti perlu. **B** hanyalah jalan
tengah bila tetap ingin di Easypanel; ia mengganti runtime **seluruh node**, jadi tidak bisa
dipisah per tenant seperti **C**.

## 5. Cara menempelkannya ke arsitektur Bolu

Yang **tidak** boleh terjadi: sebuah alat tunggal bernama `computer.run` yang bisa melakukan apa
saja. Itu menghapus granularitas yang jadi inti gerbang persetujuan — model bisa memakai satu
panggilan untuk membuka browser, mengetik, dan menekan "kirim", dan tak ada satu pun titik di mana
manusia ditanya.

### 5.1 Rekomendasi bentuk: MCP server yang membungkus sandbox

Alat-alat sandbox dipecah menjadi alat diskret berlabel, lalu diekspos sebagai **server MCP** yang
ditambahkan workspace — persis jalur EPIC 8, tanpa modul core baru:

| Alat | Label | Kenapa |
| --- | --- | --- |
| `computer.browse` (navigate, screenshot, baca DOM) | `read` | Melihat tidak mengubah apa pun |
| `computer.file_read` | `read` | Membaca berkas di dalam sandbox |
| `computer.file_write` | `write_internal` | Menulis ke berkas milik Bolu, tidak keluar |
| `computer.type` / `computer.click` | `write_external` | Mengetik di situs pihak ketiga **bisa** mengirim data |
| `computer.submit` | `write_external` | Aksi yang menyeberang ke luar |
| `computer.exec` | **`write_external`** | Satu perintah shell bisa apa saja; perlakukan sebagai paling berbahaya |

Dua konsekuensi penting:

1. **`computer.exec` tidak bisa dilabeli per-perintah.** Perintah shell tidak punya bentuk yang
   bisa dibaca mesin seperti `payload.amount` pada aksi tagihan. Karena itu, label paling aman
   adalah `write_external` (selalu draf) — artinya **setiap** perintah shell butuh persetujuan
   manusia. Itu benar secara keamanan, tapi membuat terminal nyaris tak berguna untuk kerja otonom.
   Alternatif yang lebih ramah tapi lebih lemah: allowlist perintah yang jelas-jelas aman
   (`read`-only) dan sisanya draf. Keputusan ini perlu didesain, bukan diasumsikan.
2. **Persetujuan harus menampilkan apa yang akan dilakukan**, bukan "jalankan perintah". Untuk
   browser, tangkapan layar + aksi yang akan dikirim; untuk terminal, perintah persisnya. Ini
   memperluas komponen draf yang sekarang hanya membawa `action_kind` + `payload`.

### 5.2 Persistensi tidak boleh lewat Temporal

Sandbox hidup lama; workflow tidak. State sandbox (profil browser, berkas) tinggal di
**volume/PVC + snapshot**, dan workflow hanya membawa **pengenal sandbox** — konsisten dengan aturan
claim check di ADR 0001. Restart worker atau reconnect chat tidak boleh mematikan sandbox.

### 5.3 Alternatif: modul core baru

Menulis adapter native `computer` (seperti `gmail`) memberi kontrol penuh atas label, grant, dan
siklus hidup, tetapi menambah permukaan yang harus dirawat di backend Go dan tidak memakai kembali
jalur MCP. Ini masuk akal hanya bila MCP terbukti terlalu longgar untuk kebutuhan ini.

## 6. Satu komputer per workspace, dipakai bersama beberapa Bolu

**Keputusan cakupan: satu komputer per *workspace*.** Bolu hidup di dalam workspace, tim
(Tim Bolu / Tim Hore) mengelompokkannya, dan grup obrolan bersifat sementara — jadi unit yang
bertahan lama dan punya data adalah workspace. Satu komputer per workspace juga langsung cocok
dengan jalur MCP, yang memang workspace-scoped: **satu server MCP, satu sandbox, banyak Bolu**.

| Cakupan | Komputer per workspace | Isolasi | Biaya | Kapan dipilih |
| --- | --- | --- | --- | --- |
| **Per workspace** | 1 | Antar-Bolu berbagi berkas & mesin | Paling murah | **Dipilih** — unit yang bertahan lama, punya data, dan cocok dengan jalur MCP |
| Per tim (Tim Bolu / Tim Hore) | 2 (satu per tim) | Terpisah antar-tim | 2× | **Ditolak** — lihat alasan di bawah |
| Per Bolu (isu #4 asli) | N | Paling ketat | N× RAM | Hanya bila memang wajib terpisah |

**Kenapa per tim ditolak.** Tim bukan batas yang nyata di Bolu: hanya ada dua tim tetap (Tim Bolu
dan Tim Hore) dan keduanya dibuat otomatis, tidak ada endpoint untuk membuat tim sendiri. RLS
dikunci ke `workspace_id`, bukan `team_id`, jadi tim bukan batas data maupun keamanan. Bolu dari
tim berbeda pun bisa berada dalam satu grup obrolan. Menjadikannya cakupan komputer hanya akan
**menggandakan biaya RAM** tanpa menambah pemisahan yang berarti.

### 6.1 Apa yang dibagi, apa yang tidak

Ini inti dari "beberapa Bolu di satu komputer": tiap sumber daya punya aturan berbagi sendiri.

| Sumber daya | Model berbagi | Mekanisme |
| --- | --- | --- |
| **Berkas** | Dibagi, dengan rumah per Bolu | `/home/<bolu>/` per Bolu + `/shared/` untuk kerja bersama |
| **Terminal** | Satu sesi PTY per Bolu | Sesi bernama per Bolu; tak ada dua Bolu menulis ke satu PTY |
| **Browser** | Satu Chromium, satu *context* per Bolu | Playwright `newContext()` memisahkan cookie & storage per Bolu |
| **Layar / GUI (VNC)** | Eksklusif — rebutan | **Lease**: hanya satu Bolu memegang layar pada satu waktu |

Dua hal penting di tabel itu:

- **Browser: satu proses, banyak context.** Playwright memisahkan cookie, storage, dan cache per
  context, jadi "Bolu A login di satu situs" tidak otomatis menjadi milik Bolu B — meski keduanya
  berbagi satu Chromium. Ini yang membuat berbagi aman tanpa harus satu browser per Bolu.
- **Layar GUI tidak bisa dibagi.** Bila kelak ada mode desktop (VNC), hanya satu Bolu boleh
  mengendalikan layar pada satu waktu, dan itu butuh lease.

### 6.2 Lease untuk sumber daya eksklusif

Pola lease-nya sudah ada di repo (`internal/platform/whatsapp/lease.go`, Redis + skrip Lua) dan
dipakai untuk "satu nomor, satu gateway". Bentuk yang sama dipakai di sini:

- **Kunci**: `computer:<computer_id>:<resource>` (mis. `…:screen`, `…:browser:<host>`).
- **Pemilik**: `task_id` — supaya lease yang bocor bisa ditelusuri ke tugas yang memegangnya.
- **Ambil / perpanjang / lepas** saat tugas mulai, berjalan, dan selesai; lease kedaluwarsa bila
  worker mati, persis seperti pola yang sudah ada.

Yang **tidak** butuh lease: berkas (rumah terpisah), terminal (sesi terpisah), browser (context
terpisah). Lease hanya untuk sumber daya yang memang satu dan tak terbagi.

### 6.3 Atribusi & audit tetap utuh

Berbagi satu mesin **tidak** mengaburkan siapa melakukan apa: setiap panggilan alat membawa
`agent_id`, jadi `task_steps` dan `audit_log` tetap mencatat Bolu mana yang menjalankan perintah
mana. Persetujuan pun tetap teratribusi — draf menyimpan `agent_id`, jadi "Bolu A minta mengirim"
tetap terbaca meski komputernya bersama.

### 6.4 Risiko baru karena berbagi

- **Kebocoran antar-Bolu dalam satu workspace.** Bolu A bisa membaca berkas atau cookie Bolu B.
  Untuk tenant yang sama ini mungkin memang yang diinginkan (kerja bersama), tetapi harus
  **dinyatakan** di kebijakan data, bukan diam-diam.
- **Satu Bolu merusak pekerjaan Bolu lain.** Menghapus berkas di `/shared/` berdampak ke semua.
  Rumah per Bolu membatasi, tidak menghilangkan.
- **Rebutan sumber daya.** Satu browser untuk banyak Bolu berarti antre; tugas bisa menunggu.
- **Ledakan radius.** Satu Bolu yang ditembus prompt injection menyentuh seluruh workspace, bukan
  hanya dirinya.
- **Mitigasi**: rumah per Bolu, context browser per Bolu, lease untuk yang eksklusif, `/shared/`
  hanya lewat `write_internal`, egress allowlist, dan audit tiap aksi.

### 6.5 Siklus hidup: malas, idle, snapshot

Berbagi satu komputer baru hemat kalau sandbox-nya tidak hidup terus: **provision malas** saat
panggilan alat pertama, **idle timeout** yang menyimpan snapshot, lalu **restore** saat dipakai lagi.
Mekanisme on/off dan penghematan node-nya ada di §4.5. Efeknya: RAM yang dibayar sebanding
**aktivitas**, bukan jumlah Bolu — sepuluh Bolu di satu workspace tetap satu sandbox.

### 6.6 Model data (garis besar)

```sql
CREATE TABLE computers (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    state         TEXT NOT NULL CHECK (state IN ('stopped', 'running', 'error')),
    runtime       TEXT NOT NULL DEFAULT '',  -- runc | gvisor | kata
    image         TEXT NOT NULL DEFAULT '',
    snapshot_ref  TEXT NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX computers_workspace_key ON computers (workspace_id);
```

Dengan RLS seperti tabel tenant lain (`workspace_id = app_current_workspace_id()`). Lease-nya
**tidak** disimpan di sini: seperti lease WhatsApp, itu liveness dan tinggal di Redis.

### 6.7 Alat MCP-nya

Satu server MCP per workspace, dengan `bolu_id` sebagai argumen. Server memetakannya ke rumah dan
context browser Bolu itu. Label tetap sama seperti §5.1 — `computer.exec` tetap `write_external`.

## 7. Risiko

- **Prompt injection dari konten yang dibaca browser.** Bolu akan membaca halaman web, email, dan
  dokumen yang isinya bisa memuat instruksi jahat. Ini kelas risiko baru yang belum ada pada
  integrasi sekarang, dan pertahanan utamanya adalah gerbang persetujuan — bukan penyaring teks.
- **Eksfiltrasi data.** Browser bisa mengirim data pelanggan ke mana saja. Mitigasi: egress
  allowlist (microsandbox mendukung `--net-default deny` + `--net-rule`), tanpa kredensial asli di
  dalam sandbox (placeholder substitution), dan audit tiap aksi keluar.
- **Kredensial.** Model yang memegang terminal bisa membaca apa pun yang ada di dalam sandbox.
  Aturan ADR 0002 (rahasia tak bocor ke log/UI) harus diperluas ke dalam sandbox: jangan pernah
  me-mount
  `~/.ssh`, `~/.aws`, atau token host.
- **Kepatuhan.** Bila sandbox memproses data pelanggan, ia harus di Jakarta. Cloud sandbox di luar
  negeri memerlukan dasar pemrosesan setara penyedia model — keputusan kepatuhan.
- **Biaya.** Sandbox persisten = RAM 24/7 untuk setiap workspace. Ini bisa menjadi biaya terbesar
  kedua setelah token. Harus *lazy on first use* + on/off (§4.5) + snapshot, dan dibatasi kuota per
  paket.
- **Operasional.** Tetap di Easypanel berarti tetap di dalam Docker: **tidak ada microVM**, dan
  gVisor hanya bisa lewat `default-runtime` yang mengganti **seluruh node**. Kubernetes membuka
  RuntimeClass per Pod, tetapi menambah control plane yang harus dirawat — beban tetap yang tidak
  menghasilkan fitur. Ini juga yang dulu dihindari saat memilih Temporal Cloud.
- **Ledakan radius.** Satu sandbox bisa melakukan hampir semua yang bisa dilakukan browser dan shell.
  Ini memperluas permukaan jauh melampaui integrasi saat ini, yang tiap alatnya sempit dan
  berlabel. Pada Easypanel ini lebih tajam: container berbagi kernel host, jadi container biasa
  (**Opsi A**) bukan sandbox untuk kode yang benar-benar tak tepercaya.
- **Skala banyak user.** Ratusan workspace berarti ratusan container dinamis. Bila ini dipaksakan
  sebagai service Easypanel per workspace, panel dan DNS-nya yang tumbang lebih dulu (§4.2). Butuh
  `sandbox-runner`, kuota per paket, dan reaper idle sejak PoC — bukan ditambahkan belakangan.

## 8. Rekomendasi

**Bertahap, dan gerbangnya bukan teknologi melainkan bukti kebutuhan.**

- **Fase 0 — sekarang: jangan bangun microVM.** Perkuat jalur integrasi + MCP yang sudah ada, dan
  selesaikan Gerbang 1 (E14.6, #118) supaya ada angka pemakaian & biaya nyata. Tanpa bukti bahwa
  tugas pelanggan memang butuh browser/terminal, investasi ini mendahului kebutuhan.
- **Fase 1 — PoC di Easypanel (Opsi A, §4.6), tanpa KVM.** **Satu container sandbox per workspace**
  yang dipakai beberapa Bolu, dikeraskan (pengguna non-root, `cap_drop: ALL`, rootfs read-only, tanpa
  `docker.sock`, limit RAM/CPU) dan dikelola service baru **`sandbox-runner`** di node sandbox
  terpisah. Chromium headless + Playwright (satu *context* per Bolu, profil di volume), rumah berkas
  per Bolu + `/shared/`, dan `exec` dengan sesi PTY per Bolu. Diekspos sebagai **satu MCP server
  workspace-scoped** dengan alat diskret berlabel (§5.1), `bolu_id` sebagai argumen, egress allowlist,
  rahasia via placeholder, dan **semua** aksi keluar default draf.
- **Fase 2 — pindah ke Kubernetes + gVisor (Opsi C, §4.6) bila jumlah workspace tumbuh.** Ini yang
  memenuhi dua syarat sekaligus: isolasi kuat **tanpa KVM**, dan **on/off per Pod + per node** (§4.5)
  sehingga biaya ikut aktivitas. Bila tetap ingin di Easypanel, jalan tengahnya gVisor host-wide
  (Opsi B) — dengan konsekuensi seluruh node berganti runtime.
- **Fase 3 — microVM (Kata/Firecracker) hanya bila ada tugas yang benar-benar butuh kernel sendiri.**
  Itu menuntut node ber-KVM dan kembali ke beban operasional yang dihindari di §4.1.
- **Fase 4 — desktop/GUI** (VNC/noVNC) hanya bila ada tugas nyata yang tak bisa diselesaikan
  headless. Ini kemungkinan besar tidak perlu.

Kriteria "PoC lolos": (a) ada tugas pelanggan nyata yang gagal tanpa browser/terminal, (b) biaya
sandbox per tugas masih di bawah porsi tokennya, (c) gerbang persetujuan tetap utuh — tak ada aksi
keluar yang lolos tanpa manusia.

## 9. Keputusan yang tetap terbuka

- Kubernetes: dikelola sendiri, atau memakai layanan terkelola (mis. GKE Jakarta yang sudah punya
  GKE Sandbox) — dan apa implikasinya ke biaya serta beban ops?
- Node sandbox: khusus sejak PoC, atau berbagi dengan node Easypanel; dan di Kubernetes, node pool
  mana yang dipakai (taint-nya, dan runtime apa yang jadi default di sana)?
- `computer.exec`: seluruhnya `write_external`, atau allowlist perintah `read`-only?
- Seberapa ketat pemisahan antar-Bolu di dalam satu komputer (rumah terpisah saja, atau context
  browser & PTY juga wajib terpisah)?
- On/off: cukup KEDA `minReplicaCount: 0`, atau perlu node juga menyusut (cluster-autoscaler/
  Karpenter) supaya RAM benar-benar berhenti dibayar?
- Bila kelak butuh microVM: host Jakarta mana yang ber-`/dev/kvm`, dan siapa yang merawatnya?
- Batas kuota sandbox per paket, dan apakah sandbox termasuk biaya yang ditagih ke pelanggan.
- Bila kelak memakai cloud sandbox: dasar pemrosesan & region apa (menyentuh ADR 0002).

## Sumber

- Isu #4 — *Virtual computer per bot* (dan komentar penutupnya): <https://github.com/anakbolu/timbolu/issues/4>
- Firecracker (microVM berbasis KVM): <https://github.com/firecracker-microvm/firecracker>
- Kata Containers (container dalam microVM): <https://github.com/kata-containers/kata-containers>
- microsandbox (microVM lokal, snapshot, rahasia, MCP): <https://github.com/superradcompany/microsandbox> dan <https://docs.microsandbox.dev>
- gVisor (application kernel userspace): <https://github.com/google/gvisor>
- E2B (sandbox cloud): <https://github.com/e2b-dev/E2B>
- Cloudflare Sandbox SDK (Durable Object + Container, Files/S3Mount, terminal di browser): <https://github.com/cloudflare/sandbox-sdk>
- Daytona (repo inti tak lagi dirawat sejak Juni 2026): <https://github.com/daytonaio/daytona>
- Easypanel (Docker Swarm, service App/Compose/Box, Resources & Deploy settings): <https://easypanel.io/docs/services/app> dan <https://easypanel.io/docs/services/compose>
- Easypanel GPU Support (perubahan `daemon.json` di host): <https://easypanel.io/docs/guides/gpu-support>
- Easypanel CLI & MCP server (control plane programatik): <https://easypanel.io/docs/cli> dan <https://easypanel.io/docs/mcp>
- Referensi `docker service create` (tanpa `--device`, `--privileged`, `--runtime`, `--security-opt`): <https://docs.docker.com/reference/cli/docker/service/create/>
- gVisor + Docker (`runsc install`, `--runtime=runsc`): <https://gvisor.dev/docs/user_guide/quick_start/docker/>
- Kubernetes RuntimeClass (runtime per Pod): <https://kubernetes.io/docs/concepts/containers/runtime-class/>
- gVisor di Kubernetes (RuntimeClass `runsc`, tanpa KVM): <https://gvisor.dev/docs/user_guide/kubernetes/>
- GKE Sandbox (gVisor bawaan): <https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods>
- Kata Containers — butuh virtualisasi perangkat keras/nested virt: <https://github.com/kata-containers/kata-containers/blob/main/docs/install/minikube-installation-guide.md>
- KEDA — scale to zero (`minReplicaCount: 0`): <https://keda.sh/docs/2.16/concepts/scaling-deployments/>
- Region Jakarta: AWS `ap-southeast-3` <https://aws.amazon.com/about-aws/global-infrastructure/regions_az/>; GCP Indonesia <https://cloud.google.com/about/locations>
