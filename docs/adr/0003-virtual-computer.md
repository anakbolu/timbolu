# ADR 0003 — Virtual Computer: satu sandbox Docker per bot untuk PoC

- Status: **Diterima (PoC)**
- Tanggal: 2026-10-16
- Isu: #4 — *Virtual computer per bot: persistent session, terminal, browser*
- Riset: [riset-microvm.md](../riset-microvm.md) — rekomendasinya "jangan sekarang"; bila ditempuh, **gVisor + Kubernetes**
- Terkait: [ADR 0001](0001-temporal-dan-region.md) (region Jakarta, *claim check*), [ADR 0002](0002-kepatuhan-uu-pdp.md) (rahasia & data pelanggan), EPIC 7 (#59) approval & draf, EPIC 8 (#66) integration hub

## Ringkasan keputusan

| Pertanyaan | Keputusan |
| --- | --- |
| Cakupan PoC | **Satu sandbox Docker per bot (agent)**, sesuai kata-kata isu #4. Produksi kemungkinan konvergen ke **satu sandbox per workspace** (riset §6) — perbedaan itu dicatat, bukan diputuskan di sini |
| Runtime | **Container Docker (runc) di EasyPanel** — bukan microVM, bukan `--privileged`, tanpa `/dev/kvm` |
| Isi sandbox | Chromium + Xvfb + x11vnc/noVNC (tampilan desktop). CDP `9222` disiapkan di image, belum dipakai |
| Kendali | **Control plane internal** (`apps/virtual-computer/control`): screenshot / click / type / exec, dipanggil activity `agent-worker` |
| Persistensi sesi | **Volume bernama per bot** (`/home/bolu`: profil browser + berkas) |
| Rahasia | Tinggal di host/KMS; **tidak pernah** dibakar ke image sandbox; disuntik saat runtime oleh control plane hanya bila perlu |
| Jaringan | Control API hanya di jaringan Docker internal — tidak dipublikasikan, tidak ada di internet publik |
| Aksi keluar | `write_external` dari sandbox tetap lewat draf + persetujuan; PoC **men-stub** jalur ini |
| Bukan PoC | Kubernetes/gVisor, snapshot, lease layar, satu komputer per workspace |

## 1. Konteks

**Fakta yang mengikat.** [riset-microvm.md](../riset-microvm.md) sudah memetakan lanskapnya dan
menyimpulkan: (a) isu #4 pernah ditutup dengan alasan arsitektur, (b) yang mahal bukan browsernya
melainkan **isolasi** dan **RAM 24/7**, (c) EasyPanel berjalan di atas Docker Swarm pada satu server
Ubuntu dan **tidak punya** `--privileged`, `--device`, atau `--security-opt` — jadi microVM
(Firecracker/Kata/microsandbox) **mati di sana**, (d) cloud sandbox populer (E2B, Cloudflare, Modal)
tidak punya region Jakarta, dan itu berbenturan dengan ADR 0001/0002, (e) rekomendasi riset adalah
**tunggu Gerbang 1 (E14.6, #118)** lalu, bila memang perlu, lompat ke gVisor + Kubernetes.

**Kenapa tetap ada PoC.** Rekomendasi itu menjawab "jangan bangun produksinya sekarang", bukan
"jangan pernah membuktikan jalurnya". Yang belum pernah dibuktikan di repo ini adalah hal yang paling
murah dibuktikan: **apakah satu container Docker bisa memegang sesi desktop + browser yang bertahan,
dan apakah worker Temporal bisa mengendalikannya lewat satu API kecil**. Kalau jawabannya tidak,
Fase 2 riset (gVisor + Kubernetes) tidak akan menolong — runtime-nya berbeda, tetapi bentuk sesi,
control plane, dan pembagian tugas antara worker dan sandbox tetap sama.

**Batas PoC ini dinyatakan di depan.** Ia tidak menilai isolasi (container biasa berbagi kernel host,
jadi ini **bukan** sandbox untuk kode yang benar-benar tak tepercaya — riset §4.1), tidak menyelesaikan
biaya (satu sandbox per bot berarti RAM per bot, bukan per workspace), dan tidak mengubah satu pun
jalur produksi: paket backend yang ditambahkan **belum di-wire** ke `container` maupun ke
`agent-worker`.

## 2. Opsi

| Opsi | Apa itu | Kenapa |
| --- | --- | --- |
| **A. Cloud sandbox** (E2B/Cloudflare/Modal) | Sandbox terkelola lewat API | **Ditolak.** Tidak ada region Jakarta; memproses data pelanggan di luar negeri menuntut dasar kepatuhan tersendiri (ADR 0002), dan itu keputusan yang tidak perlu diambil untuk sebuah PoC |
| **B. Docker + Xvfb + noVNC di EasyPanel** | Satu container biasa yang dikeraskan, dikendalikan satu control plane | **Dipilih untuk PoC.** Satu-satunya bentuk yang berjalan di host hari ini tanpa perubahan tingkat host (riset §4.1) |
| **C. gVisor + Kubernetes** | RuntimeClass per Pod, on/off KEDA | **Target produksi** bila Gerbang 1 membuktikan kebutuhan. Bukan sekarang: menambah control plane yang harus dirawat adalah beban tetap yang justru dihindari saat memilih Temporal Cloud (ADR 0001) |

## 3. Keputusan

### 3.1 Cakupan: satu sandbox per bot — untuk PoC

**Keputusan.** PoC memakai **satu sandbox per bot (agent)**, persis kata-kata isu #4, supaya yang
dibuktikan cocok dengan yang diminta isu itu.

**Catatan konvergensi.** Riset §6 memilih **satu komputer per workspace** untuk produksi, dan
alasannya masih berlaku: workspace adalah unit yang bertahan lama, punya data, dan cocok dengan jalur
MCP yang memang workspace-scoped; satu sandbox per bot menggandakan RAM per jumlah bot. PoC ini
karena itu memakai `bot_id` sebagai kunci sesi — kunci yang bisa dipetakan ke `workspace_id` tanpa
mengubah control plane, dan itulah bentuk yang diharapkan bertahan.

### 3.2 Stack: Chromium + Xvfb + noVNC + control plane

**Keputusan.** Isi sandbox adalah proses yang bisa dibaca daftarnya, bukan satu "komputer" opaque:

| Lapisan | Proses | Port |
| --- | --- | --- |
| Layar | `Xvfb` (`:99`, `1280x800x24`) | — |
| Desktop | `fluxbox` (opsional, agar jendela punya bingkai) | — |
| VNC | `x11vnc` (localhost saja) | `5900` |
| Tampilan di browser | `websockify` + noVNC | `6080` |
| Browser | Chromium dengan profil persisten + CDP | `9222` (disiapkan, belum dipakai) |

Yang mengendalikannya adalah **control plane** kecil (`apps/virtual-computer/control`) dengan satu
permukaan: `sessions` (ensure/status/stop) dan aksi `screenshot` / `click` / `type` / `exec`.
Activity `agent-worker` memanggilnya sebagai HTTP biasa.

**Kenapa bukan Fiber, padahal backend memakainya.** Control plane sengaja hanya memakai standard
library, dan itu satu-satunya penyimpangan dari pola HTTP di repo ini. Alasannya: modul ini harus bisa
dihapus tanpa jejak bila Gerbang 1 memutuskan Fase 2 tidak dimulai — tanpa dependensi, tidak ada
`go.sum` baru, tidak ada paket yang ikut ke-*vendor*, dan image-nya cukup `distroless/static`. Ia juga
tidak butuh apa pun yang Fiber berikan: sembilan rute, tanpa middleware, tanpa rendering, dan
permukaan yang dipakai mesin, bukan manusia. Bila control plane ini bertahan melewati PoC, ia masuk ke
`apps/backend` (atau memakai Fiber seperti proses lain) saat itu — bukan sekarang.

**Kenapa control plane dulu, bukan langsung MCP.** Riset §5.1 memutuskan bentuk akhirnya: alat
sandbox dipecah menjadi alat berlabel (`computer.browse` = `read`, `computer.click`/`computer.type` =
`write_external`, `computer.exec` = `write_external`) dan diekspos lewat server MCP workspace-scoped.
PoC ini **tidak** memutuskan sebaliknya: ia membangun lapisan eksekusinya lebih dulu, karena lapisan
itu yang sama untuk MCP, untuk adapter native, maupun untuk pemanggilan dari activity. Label, grant,
dan gerbangnya tetap urusan jalur MCP/EPIC 8, dan itu belum disentuh di sini.

### 3.3 Isolasi: container biasa yang dikeraskan, tanpa KVM

**Keputusan.** Sandbox adalah container Docker biasa (runc) di EasyPanel, dijalankan non-root,
`cap_drop: ALL`, tanpa `docker.sock`, tanpa `/dev/kvm`, tanpa `--privileged`, dengan limit RAM/CPU,
dan profil browser di volume bernama. Node sandbox sebaiknya dipisah dari node API (riset §4.2), dan
itu keputusan deployment, bukan keputusan kode.

**Konsekuensi yang dinyatakan, bukan disembunyikan.** Ini **lemah** untuk kode yang benar-benar tak
tepercaya: container berbagi kernel host. PoC karena itu hanya cocok untuk aksi yang sudah dibatasi
labelnya, dan bukan pengganti isolasi yang diminta riset Fase 2.

### 3.4 Rahasia tidak masuk sandbox

**Keputusan.** Image sandbox tidak memuat satu pun kredensial — tidak ada token, tidak ada kunci,
tidak ada `~/.ssh`/`~/.aws` yang di-mount. Rahasia tetap di host/KMS (ADR 0002) dan, bila kelak
sebuah aksi benar-benar butuh, ia **disuntik saat runtime oleh control plane** untuk satu panggilan
itu saja.

**Alasan.** Model yang memegang terminal dan browser bisa membaca apa pun yang ada di dalam sandbox
(riset §7). Karena itu yang tidak boleh ada di sana adalah hal yang tidak bisa diambil kembali.
Penyuntikan per panggilan juga membuat "kredensial mana yang dipakai Bolu mana" bisa diaudit di satu
tempat, bukan tersebar di dalam image.

### 3.5 Sesi persisten: volume bernama per bot

**Keputusan.** `/home/bolu` (profil Chromium + berkas) hidup di **volume Docker bernama per bot**,
bukan di dalam container. Container boleh mati; sesi tidak hilang.

**Alasan.** Ini yang membuat "login sekali, lanjut besok" mungkin tanpa menjaga proses tetap hidup
(riset §2.3), dan ini juga yang membuat restart worker tidak menyentuh sandbox — sejalan dengan
aturan *claim check* ADR 0001: workflow hanya membawa pengenal, state hidup di tempat lain.

### 3.6 Control API internal, tidak pernah publik

**Keputusan.** Control plane hanya mendengarkan di **jaringan Docker internal**; `agent-worker`
mencapainya lewat nama service, dan port-nya tidak dipublikasikan. Ia bicara untuk semua tenant
sekaligus, jadi ia tidak boleh punya pintu dari internet.

### 3.7 Aksi keluar tetap lewat gerbang persetujuan

**Keputusan.** Apa pun yang dilakukan sandbox dan menyeberang keluar (`click`/`type` ke situs pihak
ketiga, `exec`) tetap berlabel `write_external`: draf, tugas parkir `waiting_approval`, keputusan
manusia (riset §5.1). **PoC men-stub jalur ini** — control plane hanya mencatat aksinya, dan
penyambungan ke gerbang approval adalah pekerjaan berikutnya, bukan sesuatu yang dianggap selesai.

## 4. Konsekuensi

- **Bukan sandbox untuk kode tak tepercaya.** Prompt injection dari halaman yang dibaca browser tetap
  kelas risiko baru (riset §7), dan pertahanannya tetap gerbang persetujuan, bukan container ini.
- **Biaya mengikuti jumlah bot, bukan aktivitas.** Satu sandbox per bot berarti RAM 24/7 per bot
  sampai ada *reaper* idle + snapshot; PoC tidak punya keduanya, dan itu sebabnya ia tidak boleh
  dibawa apa adanya ke produksi.
- **Desktop (VNC) adalah lapisan termahal dan paling tidak perlu.** Riset menempatkannya di Fase 4;
  ia ada di PoC karena isu #4 meminta "komputer" yang bisa dilihat, dan karena tangkapan layar
  adalah bukti visual termurah untuk PR.
- **Tidak ada perubahan pada jalur produksi.** Paket `internal/modules/vcomputer` dan control plane
  belum didaftarkan di `container`/`agent-worker`, jadi `api` dan worker berperilaku persis seperti
  sebelum ADR ini.
- **Urutan yang tetap berlaku.** Gerbang 1 (E14.6, #118) tetap penentu apakah ini dilanjutkan:
  tanpa bukti tugas pelanggan yang gagal tanpa browser/terminal, Fase 2 tidak dimulai.

## 5. Dampak ke repositori

- `docs/adr/0003-virtual-computer.md` — dokumen ini.
- `apps/virtual-computer/README.md` — diagram arsitektur, daftar permukaan API, cara menjalankan PoC, dan daftar hal yang masih di-stub.
- `apps/virtual-computer/sandbox/{Dockerfile,entrypoint.sh}` — image: Xvfb → fluxbox → x11vnc → noVNC → Chromium.
- `apps/virtual-computer/control/` — control plane (modul Go terpisah, hanya stdlib) plus `Dockerfile`-nya: `Runtime` dengan `InMemoryRuntime` (stub, dipakai uji) dan `DockerRuntime` (kerangka, TODO).
- `apps/virtual-computer/docker-compose.poc.yml` — dua service PoC: `vcomputer-control` + `vcomputer-sandbox-example`, volume `bot-session-demo`.
- `apps/virtual-computer/demo/index.html` — halaman status statis untuk bukti visual.
- `apps/backend/internal/modules/vcomputer/{domain,service,temporal}` — port `ComputerUseClient`, klien HTTP, activity stub. **Belum di-wire.**
- `go.work` — menambahkan modul control supaya `go test` di dalamnya jalan; modul backend tidak berubah.

## Keputusan yang tetap terbuka

- **Cakupan akhir**: per bot (isu #4) atau per workspace (riset §6) — dan bagaimana `bot_id` dipetakan bila kelak per workspace.
- **Cara model mencapainya**: server MCP workspace-scoped (riset §5.1) atau adapter native; dan bagaimana `computer.exec` dilabeli — seluruhnya `write_external`, atau allowlist perintah `read`-only.
- **Layar sebagai sumber daya eksklusif**: lease (`computer:<id>:screen`, pola yang sudah ada di repo) belum dibangun; dengan satu sandbox per bot ia belum dibutuhkan.
- **On/off dan biaya**: reaper idle, snapshot, kuota per paket — tidak ada di PoC.
- **Isolasi**: kapan pindah ke gVisor + Kubernetes (riset Fase 2), dan node sandbox mana yang dipakai.
- **Persetujuan**: bentuk draf untuk aksi sandbox (tangkapan layar + aksi yang akan dikirim, bukan "jalankan perintah") — memperluas komponen draf yang sekarang hanya membawa `action_kind` + `payload`.
