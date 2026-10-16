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
| Akses ke daemon Docker | **Proxy socket dengan allowlist** sejak Revisi 2026-10-16 (b): hanya `vcomputer-socket-proxy` yang memegang `/var/run/docker.sock`; control plane bicara lewat `DOCKER_HOST` di jaringan internalnya sendiri |
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
`agent-worker` — sejak Revisi 2026-10-16 di bawah, activity-nya terdaftar di agent worker, tetapi
`AgentTaskWorkflow` dan katalog tool tetap tidak menyentuh sandbox.

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
  belum didaftarkan di `container`/`agent-worker` saat keputusan ini diambil, jadi `api` dan worker
  berperilaku persis seperti sebelumnya. Revisi 2026-10-16 mengubah bagian ini: activity-nya
  sekarang terdaftar di agent worker lewat `container.RegisterVComputerTasks`, sementara loop
  tugas agennya tetap tidak berubah.
- **Urutan yang tetap berlaku.** Gerbang 1 (E14.6, #118) tetap penentu apakah ini dilanjutkan:
  tanpa bukti tugas pelanggan yang gagal tanpa browser/terminal, Fase 2 tidak dimulai.

## 5. Dampak ke repositori
- `docs/adr/0003-virtual-computer.md` — dokumen ini.
- `apps/virtual-computer/README.md` — diagram arsitektur, daftar permukaan API + aturan token, tabel environment, cara menjalankan (uji modul dan `scripts/e2e.sh`), trade-off `docker.sock`, dan daftar hal yang masih di-stub.
- `apps/virtual-computer/sandbox/{Dockerfile,entrypoint.sh}` — image: Xvfb → fluxbox → x11vnc → noVNC → Chromium, plus `socat` yang mempublikasikan CDP loopback Chromium ke alamat container.
- `apps/virtual-computer/control/` — control plane (modul Go terpisah, hanya stdlib) plus `Dockerfile`-nya (`docker:27-cli`): `Runtime` dengan `InMemoryRuntime` (tanpa Docker, dipakai uji) dan `DockerRuntime` (nyata, lewat CLI docker). Sejak Revisi 2026-10-16 (b) container itu **tidak** me-mount `/var/run/docker.sock`; CLI-nya diarahkan `DOCKER_HOST` ke proxy di bawah, dan root yang tersisa di image itu tinggal warisan (prosesnya tidak menyentuh socket, port-nya bukan port privileged).
- `apps/virtual-computer/docker-compose.poc.yml` — service `vcomputer-control` (tiga jaringan, port hanya di loopback), `vcomputer-fixture` (`busybox httpd` di jaringan internal), `vcomputer-socket-proxy` (satu-satunya pemegang socket host, dengan allowlist per section Engine API), plus service `vcomputer-sandbox` sebagai target build di profil `build`. Sandbox per bot tidak ada di file ini: runtime yang membuatnya, dengan volume per bot.
- `apps/virtual-computer/fixture/index.html` — halaman uji yang bisa dibuka browser sandbox tanpa internet.
- `apps/virtual-computer/scripts/e2e.sh` — checklist end-to-end: build, naikkan stack, periksa, tulis bukti, cetak PASS/FAIL. Sejak Revisi 2026-10-16 (b) ia juga memeriksa pagar socket: tidak ada mount `docker.sock` di container control, proxy tidak mempublikasikan port, `vcomputer-dockerapi` internal, `403` untuk section yang tidak dipakai, dan tidak ada rute dari sandbox ke proxy.
- `apps/virtual-computer/demo/index.html` — halaman statis yang menyematkan noVNC asli lewat proxy control plane; tanpa mock desktop.
- `apps/backend/internal/modules/vcomputer/{domain,service,temporal,workflow}` — port `ComputerUseClient` (termasuk `Navigate`), klien HTTP dengan bearer token, activity-nya, dan `SmokeWorkflow`. **Terdaftar di agent worker** sejak Revisi 2026-10-16 (`internal/container/vcomputer.go`, `cmd/vcomputer-smoke`).
- `go.work` — memuat modul control supaya `go test` di dalamnya jalan dari checkout; modul produk tidak berubah.

## Revisi 2026-10-16 setelah QA PR #178

QA menolak PR #178 karena PoC-nya belum end-to-end: permukaan API-nya ada, mesinnya belum.
Revisi ini mencatat apa yang berubah; keputusan di atas tetap berlaku kecuali di bagian yang
disebut di sini.

- **`DockerRuntime` benar-benar diimplementasikan** lewat CLI docker (`volume create`,
  `run`, `exec`, `inspect`, `stop`), argv tanpa shell, binary dari `VCOMPUTER_DOCKER_BIN`.
  Tidak ada lagi jawaban "belum diimplementasikan": setiap rute dilayani setiap runtime.
- **Aksinya nyata**: `screenshot` menangkap Xvfb dengan `scrot` di dalam container,
  `click`/`type` lewat `xdotool`, `exec` menjalankan perintahnya dan mengembalikan
  stdout/stderr/exit code (`stub=false`), dan `navigate` memanggil `PUT /json/new` ke CDP.
- **CDP bisa dijangkau**: Chromium baru mengikat `--remote-debugging-port` ke `127.0.0.1`
  dan mengabaikan `--remote-debugging-address=0.0.0.0`, jadi entrypoint menjalankan
  `socat TCP-LISTEN:${CDP_PORT},fork,reuseaddr,bind=0.0.0.0` menuju `127.0.0.1:9222`, dan
  control plane menyapa IP container (Chromium menolak Host header non-IP). `cdp_url`
  karenanya `http://<ip>:<port>`, bukan `localhost`.
- **Jaringan internal**: `vcomputer-internal` (`internal: true`) hanya memuat sandbox dan
  satu service fixture — tanpa egress, tanpa rute dari host. Control plane satu-satunya
  anggota kedua jaringan (`vcomputer-internal` + `vcomputer-edge`).
- **noVNC lewat proxy**: `/v1/sessions/{bot}/novnc/...` diteruskan `httputil.ReverseProxy`
  ke `http://<ip>:<port-novnc>/` berikut upgrade websocket, sehingga browser hanya perlu
  menjangkau control plane.
- **Token wajib**: `VCOMPUTER_CONTROL_TOKEN` tidak punya default dan control plane menolak
  start tanpa itu; setiap rute `/v1/*` butuh bearer (perbandingan constant-time), dan URL
  noVNC boleh menukar `?token=` dengan cookie `vc_token` (`HttpOnly`, `SameSite=Strict`,
  `Path=/v1/sessions/`). `/health` tetap terbuka.
- **Port per sandbox, bukan `6080` hardcoded**: `VCOMPUTER_SANDBOX_NOVNC_PORT` /
  `VCOMPUTER_SANDBOX_CDP_PORT` dikirim ke setiap container. Sandbox tidak pernah di-`-p`,
  dan hanya control plane yang dipublikasikan, itupun di loopback.
- **Trade-off `/var/run/docker.sock` diterima untuk PoC**: container control me-mount socket
  host dan berjalan sebagai root, yang setara akses root di host; itu lubang terbesar di
  PoC ini dan dinyatakan terbuka di README. Jalur produksinya tetap klien Docker API di node
  sandbox — tidak pernah socket di dalam container (riset §4.2). **Digantikan Revisi
  2026-10-16 (b)**: container control tidak lagi memegang socket; proxy dengan allowlist
  yang memegangnya.
- **`go.work` memuat modul control**, jadi ujinya jalan dari checkout; modul produk
  tidak berubah.

Catatan supersesi: tabel di §3.2 masih menyebut CDP "disiapkan, belum dipakai" dan noVNC di
port `6080`; baris "Jaringan" di Ringkasan dan §3.6 menyebut control plane hanya di jaringan
internal dan tidak pernah dipublikasikan. Revisi ini yang berlaku untuk CDP, port per sandbox,
dan pembagian dua jaringan — control plane tetap hanya di `127.0.0.1` pada host.

## Revisi 2026-10-16: activity terdaftar di agent worker

PoC-nya sekarang punya jalur dari worker ke sandbox. Yang berubah, dan batasnya:

- **Activity-nya terdaftar.** `internal/container/vcomputer.go` membangun set activity
  (`NewVComputerTasks`) dan `RegisterVComputerTasks` mendaftarkannya di `RegisterAgentTasks`,
  **sebelum** cabang `c.Tasks == nil`: komputer bot tidak butuh database, jadi worker yang
  runtime tugasnya mati tetap bisa menjalankan sandbox. Queue-nya queue agent worker yang sama,
  tanpa proses kedua.
- **Konfigurasinya opsional.** `VCOMPUTER_CONTROL_URL`, `VCOMPUTER_CONTROL_TOKEN`,
  `VCOMPUTER_CONTROL_TIMEOUT` (`packages/platform/config`). Alamat kosong berarti tidak
  terkonfigurasi dan setiap activity menjawab `ErrNotConfigured` — bukan worker yang tidak tahu
  ada komputer. Token tidak pernah masuk log; apakah control plane terkonfigurasi dicatat sekali
  saat start.
- **Yang terdaftar hanya `SmokeWorkflow`** (`internal/modules/vcomputer/workflow/smoke.go`):
  `EnsureSession` → `Navigate` → `Screenshot` → `Click` → `Type` → `Screenshot` → `Exec`, dengan
  jeda di antara langkah-langkahnya, timeout 90 detik untuk `EnsureSession` dan 30 detik untuk
  sisanya, `MaximumAttempts: 3`, dan hasil tanpa byte PNG.
- **`cmd/vcomputer-smoke`** menjalankan satu smoke run dari checkout: worker sendiri di queue
  `vcomputer-smoke`, registrasi lewat `container.RegisterVComputerTasks`, hasil sebagai JSON, dan
  opsional satu tangkapan terakhir ke `after-temporal-final.png`.
- **Yang tetap belum**: `AgentTaskWorkflow` dan katalog tool tidak menyentuh sandbox. Tidak ada
  tool `click`/`type`/`exec` yang bisa dipanggil model, jadi tidak ada gerbang persetujuan untuk
  aksi sandbox, tidak ada pemetaan `bot_id` ↔ workspace, dan tidak ada *reaper* idle. PoC ini
  membuktikan sambungannya, bukan bahwa tugas pelanggan memakainya.

Batas §1 dan §4 tentang jalur produksi karena itu berlaku dengan satu pengecualian: yang
terdaftar adalah activity dan satu workflow smoke, bukan loop tugas agennya.

## Revisi 2026-10-16 (b): socket Docker lewat proxy dengan allowlist

Revisi ini menjawab isu #192. Keberatan yang diangkat sederhana dan benar: control plane
me-mount `/var/run/docker.sock` dan berjalan sebagai root, jadi container itu **setara root
di host**, dan permukaan HTTP-nya — sekuat apa pun token-nya — adalah pintu menuju
privilege itu. Yang berubah di bawah ini bukan bentuk control plane, melainkan **siapa yang
memegang socket**.

### Keputusan

Control plane tidak lagi memegang socket. Satu container tambahan,
`vcomputer-socket-proxy` (`tecnativa/docker-socket-proxy:v0.5.0`, tag dipin karena container
ini adalah batas keamanan dan karena `docker exec` butuh haproxy ≥ 3.0.6 untuk hijack
`Upgrade: tcp`), memegang `/var/run/docker.sock` **read-only** dan meneruskan Engine API di
port `2375` dengan allowlist per section. CLI docker di dalam `vcomputer-control` diarahkan
ke sana lewat `DOCKER_HOST=tcp://vcomputer-socket-proxy:2375`, dan tidak ada mount socket di
container itu sama sekali — kalau `DOCKER_HOST` hilang, setiap panggilan gagal keras alih-alih
diam-diam kembali ke root host.

Jaringan baru `vcomputer-dockerapi` (`internal: true`) memuat **hanya** proxy dan control
plane. Proxy tidak mempublikasikan port dan tidak ikut ke `vcomputer-internal`, jadi sandbox
— satu-satunya pihak yang benar-benar menjalankan kode tak tepercaya — tidak punya rute ke
daemon API, bahkan untuk `/_ping`.

### Allowlist, dan dari mana isinya datang

Isinya bukan tebakan: setiap baris di bawah adalah panggilan yang benar-benar dilakukan
`apps/virtual-computer/control/internal/sandbox/docker.go` (argv, tanpa shell).

| Section | Nilai | Panggilan yang memakainya |
| --- | --- | --- |
| `VOLUMES` | `1` | `docker volume create` — volume home per bot |
| `CONTAINERS` | `1` | `docker run`, `docker inspect`, `docker start`, `docker stop` |
| `EXEC` | `1` | `docker exec` untuk `screenshot` / `click` / `type` |
| `POST` | `1` | semua tulis di atas; juga yang meloloskan `HEAD /_ping` milik CLI |
| `ALLOW_START` / `ALLOW_STOP` | `1` | `docker start` / `docker stop` pada path-nya sendiri |
| `PING` | `1` | ping yang dilakukan CLI saat start; dari situ versi API dinegosiasikan |
| selain itu | `0` | tidak ada — `403` dari proxy |

`NETWORKS=0` aman karena `docker run --network <nama>` diselesaikan daemon, bukan lewat
panggilan API dari CLI; `VERSION=0` aman karena CLI mengambil versi dari ping, bukan
`/version`; `EVENTS=0` menimpa default image-nya. `IMAGES=0` adalah harga yang dibayar:
runtime tidak bisa menarik image, jadi `vcomputer-sandbox:poc` harus sudah dibangun di daemon
itu.

**Allowlist-nya per section, bukan per operasi — dan itu batasnya.** Ini diukur, bukan
disimpulkan dari dokumentasi: haproxy dari image v0.5.0 dijalankan di atas daemon tiruan, dan
hasilnya:

| Request | Jawaban | Kenapa |
| --- | --- | --- |
| `POST /containers/vcomputer-bot/restart`, `/kill`, `/pause` | **200** | rule `ALLOW_*` dievaluasi sebelum rule section, jadi `/containers` yang `1` sudah cukup — `ALLOW_RESTARTS=0` tidak menutupnya |
| `DELETE /containers/<id>` (`docker rm`) | **200** | path-nya di bawah `/containers` |
| `DELETE /volumes/<nama>` | **200** | path-nya di bawah `/volumes` |
| `PUT /containers/create` | **200** | `POST=1` mematikan gerbang method `deny unless METH_GET \|\| POST`, jadi method bukan batas lagi |
| `GET /images/json`, `GET /info`, `GET /version`, `GET /networks`, `GET /events`, `GET /swarm`, `GET /secrets`, `GET /plugins`, `POST /build`, `GET /system/df`, `POST /auth` | **403** | section-nya `0`; daemon tidak pernah melihat request-nya |
| `POST /exec/<id>/start` dengan `Upgrade: tcp` | **101 UPGRADED**, koneksi jadi tunnel | haproxy 3.4.2 di image v0.5.0; ini yang membuat `docker exec` bekerja |

`POST=1` dan `CONTAINERS=1` karena itu wajib, dan keduanya sekaligus melebarkan lebih dari
yang dibaca dari namanya. `ALLOW_START`/`ALLOW_STOP` ditulis `1` karena itu operasi yang
benar-benar dipakai runtime, tetapi selama `CONTAINERS=1` keduanya redundan; `ALLOW_RESTARTS`
`0` dipertahankan supaya niatnya tetap terbaca bila `CONTAINERS` kelak dimatikan.

### Batas yang tetap ada

- **`CONTAINERS` mengizinkan pembuatan container dengan HostConfig apa pun.** Proxy tidak
  memeriksa body request, dan daemon tidak membedakan HostConfig yang sopan dari yang tidak.
  Control plane yang diambil alih karena itu **masih bisa mencapai root host** lewat API yang
  memang diizinkan untuknya — container privileged, atau host root yang di-bind-mount. Yang
  hilang adalah jalur yang lebih langsung: tanpa tarik image, tanpa swarm/secret/plugin,
  tanpa akses ke konfigurasi daemon, dan tanpa `docker.sock` di container itu sendiri.
  Proxy ini mempersempit permukaan, bukan menghilangkan kelas risikonya — dan itu sebabnya
  jalur produksinya tetap bukan proxy.
- **Proxy adalah satu titik kegagalan** dan satu container tambahan yang harus dirawat
  (image-nya perlu diperbarui, tag-nya sengaja tidak `latest`).
- **Root yang tersisa di image control plane tinggal warisan**, bukan kebutuhan: prosesnya
  tidak membuka socket dan port-nya bukan port privileged. Ia tetap ditulis di
  `control/Dockerfile` supaya tidak terbaca sebagai jaminan.
- **Token dan port loopback tetap pagar utama** permukaan HTTP-nya. Proxy tidak menggantikan
  keduanya.

### Jalur produksi tidak berubah

Rekomendasi riset §4.2 tetap: klien Docker API di node sandbox yang terpisah dari node API,
atau gVisor + Kubernetes di Fase 2. Proxy ini membuat PoC hari ini berhenti memegang socket
host; ia tidak menjadikan PoC ini layak produksi, dan tidak mengubah urutan Gerbang 1.

### Bukti

`scripts/e2e.sh` memeriksa pagar itu di jalur yang sebenarnya: container control tidak
me-mount `docker.sock`, proxy tidak mempublikasikan port, `vcomputer-dockerapi` benar
`internal: true`, `GET /images/json` lewat proxy dijawab `403`, dan `curl` dari dalam sandbox
ke proxy gagal. Perintah `docker compose -f apps/virtual-computer/docker-compose.poc.yml config`
juga harus tetap hijau — di situlah allowlist dan `DOCKER_HOST` bisa dibaca tanpa menaikkan
stack.

Tabel "diukur" di atas datang dari harness sekali pakai, bukan dari skrip yang ikut masuk
repo: haproxy dari image v0.5.0 diekstrak, `haproxy.cfg` dirender dari template image dengan
env yang sama seperti compose, dan dijalankan di atas daemon tiruan (HTTP server di unix
socket) yang mencatat setiap request. Harness yang sama juga menjalankan **CLI docker
sesungguhnya** dengan `DOCKER_HOST` diarahkan ke proxy itu, dan hasilnya: `volume create`,
`run -d`, `inspect`, `start`, `stop` dan `exec` semuanya selesai dengan exit 0, sementara
`image ls`, `info`, `network ls`, `pull`, `version` dan `system df` ditolak `403`. Request yang
sampai ke daemon pada jalur itu: `GET /_ping` (plus `HEAD /_ping` sebelum setiap perintah —
dari situ versi API 1.52 dinegosiasikan), `POST /volumes/create`,
`POST /containers/create?name=…`, `GET /containers/<nama>/json`,
`POST /containers/<id>/wait?condition=next-exit`, `POST /containers/<nama>/start|stop|exec`,
`POST /exec/<id>/start` dengan `Upgrade: tcp`, dan `GET /exec/<id>/json` — semuanya di dalam
section yang diizinkan, tidak ada satu pun yang butuh `IMAGES`, `INFO`, `NETWORKS` atau
`VERSION`.

Harness itu tidak dijadikan skrip tetap: ia butuh `python3` sebagai daemon tiruan, dan e2e
sengaja tidak menambah dependensi seperti itu. Yang tersisa di repo adalah pemeriksaan `403` —
kasus yang memang harus bertahan — sementara baris yang melebar (`restart`, `DELETE`, `PUT`)
dicatat sebagai batas, dan tag image yang dipin membuatnya tidak bisa berubah diam-diam.

## Keputusan yang tetap terbuka

- **Cakupan akhir**: per bot (isu #4) atau per workspace (riset §6) — dan bagaimana `bot_id` dipetakan bila kelak per workspace.
- **Cara model mencapainya**: server MCP workspace-scoped (riset §5.1) atau adapter native; dan bagaimana `computer.exec` dilabeli — seluruhnya `write_external`, atau allowlist perintah `read`-only.
- **Layar sebagai sumber daya eksklusif**: lease (`computer:<id>:screen`, pola yang sudah ada di repo) belum dibangun; dengan satu sandbox per bot ia belum dibutuhkan.
- **On/off dan biaya**: reaper idle, snapshot, kuota per paket — tidak ada di PoC.
- **Isolasi**: kapan pindah ke gVisor + Kubernetes (riset Fase 2), dan node sandbox mana yang dipakai.
- **Persetujuan**: bentuk draf untuk aksi sandbox (tangkapan layar + aksi yang akan dikirim, bukan "jalankan perintah") — memperluas komponen draf yang sekarang hanya membawa `action_kind` + `payload`.
- **Filter per operasi, bukan per section**: `tecnativa/docker-socket-proxy` hanya bisa membuka atau menutup section, dan `CONTAINERS=1` membawa `restart`/`kill`/`rm` serta HostConfig apa pun. Kalau PoC ini bertahan lebih lama dari Gerbang 1, pilihannya: proxy kecil sendiri yang memeriksa path + body (bukan sekadar section), atau lompat ke jalur produksi (klien Docker API di node sandbox, gVisor/Kubernetes). Keduanya keputusan yang lebih besar dari PoC ini.
