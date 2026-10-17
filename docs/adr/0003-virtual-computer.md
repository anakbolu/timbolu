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
| Isi sandbox | Chromium + Xvfb + x11vnc/noVNC (tampilan desktop). Chromium memakai CDP `9222` di loopback; `vcomputer-gate` satu-satunya yang mendengarkan di alamat jaringan sandbox |
| Kendali | **Control plane internal** (`apps/virtual-computer/control`): screenshot / click / type / exec, dipanggil activity `agent-worker` |
| Persistensi sesi | **Volume bernama per bot** (`/home/bolu`: profil browser + berkas) |
| Rahasia | Tinggal di host/KMS; **tidak pernah** dibakar ke image sandbox; disuntik saat runtime oleh control plane hanya bila perlu. Satu pengecualian yang dinyatakan di §3.4: `SANDBOX_SECRET`, rahasia akses gate per sandbox |
| Jaringan | Control API hanya di jaringan Docker internal — tidak dipublikasikan, tidak ada di internet publik. Sisi sandbox sebaliknya: yang melindungi CDP dan noVNC adalah **gate**, bukan topologi jaringan |
| Permukaan sandbox | Semua proses mengikat loopback; satu-satunya listener di alamat container adalah `vcomputer-gate` di `GATE_PORT` (`8443`), dan setiap rutenya menuntut rahasia per sandbox (§3.8) |
| Aksi keluar | `write_external` dari sandbox tetap lewat draf + persetujuan; PoC **men-stub** jalur ini |
| Teardown | Hanya eksplisit: SIGTERM bukan teardown, jadi control plane **tidak** menghapus sandboxnya saat berhenti (opt-in lewat `VCOMPUTER_REAP_ON_SHUTDOWN=1`), dan teardown yang didokumentasikan adalah `scripts/cleanup.sh --running --volumes` dengan `STACK` disebut (§3.9); `scripts/e2e.sh` membuktikan kedua arahnya — `restart-*` dan `TEARDOWN=1` |
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
| Tampilan di browser | `websockify` + noVNC, diikat ke loopback sandbox | `6080` |
| Browser | Chromium dengan profil persisten + CDP | `9222` (loopback) |
| Gerbang | `vcomputer-gate` — modul Go sendiri, hanya stdlib, dibangun di stage image sandbox | `8443` (`GATE_PORT`) di alamat container |

Yang mengendalikannya adalah **control plane** kecil (`apps/virtual-computer/control`) dengan satu
permukaan: `sessions` (ensure/status/stop, plus `purge` untuk membuang volumenya dan `cleanup` untuk
membuang sisa container/volume), aksi `screenshot` / `click` / `type` / `navigate` / `exec`, dan dua
proxy — `/novnc/...` untuk orang yang mau melihat desktop, `/cdp/...` untuk apa pun yang mau bicara
CDP. Activity `agent-worker` memanggilnya sebagai HTTP biasa.

**Kenapa bukan Fiber, padahal backend memakainya.** Control plane sengaja hanya memakai standard
library, dan itu satu-satunya penyimpangan dari pola HTTP di repo ini. Alasannya: modul ini harus bisa
dihapus tanpa jejak bila Gerbang 1 memutuskan Fase 2 tidak dimulai — tanpa dependensi, tidak ada
`go.sum` baru, tidak ada paket yang ikut ke-*vendor*, dan image-nya cukup `distroless/static`. Ia juga
tidak butuh apa pun yang Fiber berikan: belasan rute, tanpa middleware, tanpa rendering, dan
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

**Pengecualian yang dinyatakan: `SANDBOX_SECRET`.** Sejak gate ditambahkan (§3.8) ada tepat satu
rahasia yang memang masuk ke sandbox: rahasia akses gate, 32 karakter hex dari `crypto/rand`,
di-generate control plane per sandbox dan diteruskan sebagai `-e SANDBOX_SECRET=`. Ia bukan kredensial
pelanggan dan tidak membuka apa pun di luar sandbox itu: yang dibelinya adalah hak memakai CDP dan
noVNC sandbox *itu*, yang sudah dipegang oleh kode yang berjalan di dalamnya. Karena itu ia juga tidak
melanggar alasan di atas — model yang membaca env sandbox tidak mendapat apa pun yang belum ia punya,
sedangkan kredensial yang benar-benar berharga tetap tidak pernah masuk. Yang perlu dicatat jujur:
rahasia ini bisa dibaca dari env container oleh siapa pun yang punya akses docker, jadi ia pagar
terhadap jalur jaringan, bukan terhadap operator host (§3.8).

### 3.5 Sesi persisten: volume bernama per bot

**Keputusan.** `/home/bolu` (profil Chromium + berkas) hidup di **volume Docker bernama per bot**,
bukan di dalam container. Container boleh mati; sesi tidak hilang. Namanya `<prefix>-<bot>-home`
dengan `prefix` dari `VCOMPUTER_SANDBOX_NAME_PREFIX` (default `vcomputer`), dan volumenya dibuat
dengan dua label: `vcomputer.bot=<bot id>` (ini volume milik sebuah bot) dan
`vcomputer.stack=<prefix>` (ini volume milik stack itu). Label kedua yang membuat pembersihan sisa
menargetkan volume satu stack **secara tepat**, bukan lewat awalan nama: awalan `vcomputer` juga
cocok dengan sandbox stack bernama `vcomputer-qa` (#263).

**Volumenya bertahan melewati restart, bukan hanya melewati container.** SIGTERM ke control plane
adalah hal yang biasa — `compose restart`, `up -d --force-recreate` saat naik versi, daemon docker
yang di-restart — dan bukan keputusan bahwa sesinya selesai: control plane karena itu **tidak**
menghapus sandbox apa pun saat berhenti, dan `DELETE /v1/sessions/<bot>` biasa juga tidak menyentuh
volumenya (§3.9). Yang menghapusnya hanya dua jalur eksplisit: `DELETE ?purge=true` untuk satu bot,
dan `scripts/cleanup.sh --running --volumes` (atau `VCOMPUTER_REAP_ON_SHUTDOWN=1`) untuk teardown
sebuah stack.

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

### 3.8 Permukaan jaringan sandbox: satu listener, satu rahasia

**Keputusan.** Di dalam sandbox, semua yang mendengarkan mengikat **loopback**: x11vnc dengan
`-localhost`, `websockify`/noVNC di `127.0.0.1:${NOVNC_PORT:-6080}`, dan Chromium dengan
`--remote-debugging-port=9222` (Chromium mengabaikan `--remote-debugging-address=0.0.0.0`, jadi CDP
tidak bisa dipublikasikan dari dalam). Satu-satunya proses yang mengikat alamat jaringan container
adalah **`vcomputer-gate`** di `0.0.0.0:${GATE_PORT:-8443}`: `/cdp/*` diteruskan ke
`127.0.0.1:9222`, `/novnc/*` ke `127.0.0.1:${NOVNC_PORT}`, Host header ditulis ulang menjadi
`localhost` (endpoint DevTools menolak nilai lain dengan 500), upgrade websocket diteruskan apa
adanya, dan setiap rute kecuali `/healthz` menuntut header `X-Sandbox-Secret` yang dibandingkan
constant-time dengan `SANDBOX_SECRET`. Control plane satu-satunya yang memegang rahasia itu, jadi ia
pula satu-satunya yang bisa menyapa gate; `Session.cdp_url` dan `Session.novnc_url` karena itu berupa
path control plane (`/v1/sessions/<bot>/cdp`, `/v1/sessions/<bot>/novnc`), bukan alamat container.

**Alasan, dan koreksi yang menyertainya.** Versi pertama dokumen ini menyatakan host "tidak punya rute"
ke port sandbox. Itu **salah**: host tetap bisa menyapa IP container di jaringan bridge buatan Docker.
Yang dilakukan `internal: true` adalah memblokir egress *dari dalam* container, bukan host →
container. Jadi yang benar-benar melindungi CDP dan noVNC adalah gate beserta rahasianya, bukan
bentuk jaringannya. Pernyataan yang keliru itu dihapus dari README dan dari sini, karena dokumen yang
menyebut keamanan karena kebetulan topologi akan menyesatkan pembaca berikutnya begitu topologinya
berubah.

**Yang dibeli, dan yang tidak.** Yang dibeli: apa pun yang bisa menjangkau alamat sandbox di jaringan
itu — container lain di jaringan yang sama, atau host — tidak bisa memakai CDP maupun noVNC tanpa
rahasia. Port yang terbuka berhenti menjadi izin. Yang **tidak** dibeli: siapa pun yang bisa membaca
env container (yaitu siapa pun yang punya akses docker di host) memegang rahasia itu, dan akses
docker sudah setara root di host; begitu pula kode yang berjalan di dalam sandbox, yang memang sudah
memegang browser itu. Gate ini mempersempit jalur dari jaringan, bukan membuat sandbox tahan terhadap
operatornya sendiri. Konsekuensi yang mengikuti: rahasianya harus bisa dibaca kembali dari container
setelah control plane restart, karena itu ia diambil dari
`docker inspect --format '{{json .Config.Env}}'` saat record sesi dibangun ulang.

### 3.9 Teardown: hanya eksplisit, karena sandbox bukan milik compose

**Keputusan.** Control plane **tidak** menghapus sandboxnya saat prosesnya berhenti. SIGTERM/SIGINT
adalah restart: setelah `server.Shutdown` selesai (`-shutdown-grace`, default `5s`) proses berhenti
dan sandbox yang ia buat — berikut volumenya — tetap ada. Penghapusan itu **opt-in**, lewat
`VCOMPUTER_REAP_ON_SHUTDOWN=1` (hanya `1`/`true`; selain itu berarti simpan): dengan itu saja ia
menjalankan `Runtime.Reap` (`-reap-grace`, default `60s`), yang membuang setiap container berlabel
`vcomputer.bot` **dan** `vcomputer.stack=<prefix>` miliknya — container yang sedang jalan termasuk —
beserta volumenya, dan mencatat kegagalannya sebagai log (`reaping the sandboxes failed: …`, satu
baris per sisa yang gagal), bukan sebagai exit code.

Teardown yang didokumentasikan karena itu adalah jalur eksplisit: `scripts/cleanup.sh --running
--volumes` dengan `STACK` yang disebut, yang bekerja terlepas dari prosesnya sempat menerima sinyal
atau tidak; `docker compose down` sesudahnya hanya membuang service dan jaringan compose. Compose
memberi `vcomputer-control` `stop_grace_period: 90s`, dan itu hanya relevan di jalur opt-in itu:
proses yang menyimpan sandboxnya berhenti jauh di bawah satu detik, sementara yang diminta
menghapusnya punya satu container untuk di-stop dan satu volume untuk dihapus per sesi.

**Alasan.** Sandbox tidak dideklarasikan di `docker-compose.poc.yml`: control plane yang membuatnya
(`docker run`), satu per bot, saat `Ensure`. Konsekuensinya `docker compose down` tidak bisa
membuangnya, dan satu sandbox yang masih hidup menahan volumenya sekaligus **jaringan internalnya**,
sehingga sisa sebuah run bukan satu container yang kelihatan di `docker ps` melainkan tiga jenis
objek — itu yang #245 dan #263 catat. Tetapi kesimpulan yang dulu diambil dari situ — "karena `down`
tidak bisa, biarkan proses yang menghapusnya saat berhenti" — salah arah: yang dihapus `Reap` bukan
sandbox yang menganggur melainkan **sesi yang sedang dipakai**, karena SIGTERM tidak membedakan
"restart untuk naik versi" dari "teardown". Volume yang ikut terhapus adalah profil Chromium dan
berkas bot itu: login yang sudah dilakukan, pekerjaan yang belum selesai. Sebuah proses yang mati
sementara sesinya hidup bukan teardown, jadi default-nya harus menyimpan, dan yang menghapus harus
menyebutkan dirinya sendiri.

**Mengapa `Reap` terpisah dari `Cleanup`.** `Cleanup` — yang dipakai `POST /v1/admin/cleanup` —
punya janji yang didokumentasikan: ia **tidak pernah** menyentuh container yang sedang jalan, karena
ia untuk sisa yang sudah mati, dan rutenya token-protected tapi tetap sebuah rute HTTP. `Reap`
justru sebaliknya pada dua hal: yang sedang jalan memang sasarannya, dan ia tidak punya rute HTTP
sama sekali — hanya bisa dipanggil dari dalam proses, setelah HTTP-nya berhenti, jadi tidak ada
permintaan yang bisa memicunya. Menggabungkan keduanya akan membuat salah satu janji itu bohong.

**Cakupannya label, bukan awalan nama.** `Reap` dan `Cleanup` menyaring dengan `label=vcomputer.bot`
**dan** `label=vcomputer.stack=<prefix>`. Sebelumnya penyaringnya `name=^<prefix>-`, dan itu bukan
cakupan yang sama: awalan adalah awalan, jadi stack default `vcomputer` juga mencocokkan container
`vcomputer-qa-bot`, volume `vcomputer-qa-bot-home`, dan — lewat `Reap` — menghapusnya beserta
sesinya. Label mencatat siapa yang membuat objek itu, jadi ia tidak bisa cocok dengan milik orang
lain.

**Yang dibeli, dan yang tidak.** Yang dibeli: restart apa pun — `compose restart`,
`up -d --force-recreate`, daemon docker yang di-restart — tidak menyentuh sesi yang sedang berjalan,
dan teardown tetap mungkin lewat satu perintah yang menyebut stacknya. `scripts/e2e.sh` membuktikan
keduanya: `restart-*` (`compose restart` pada control plane, tanpa opt-in, lalu sandbox + volume +
berkas di `/home/bolu` masih ada dan sesinya jalan lagi), dan `TEARDOWN=1` yang memeriksa tiga hal
setelah `cleanup.sh --running --volumes` + `down`: nol container, nol volume, nol jaringan bernama
`${STACK}-*`. Yang **tidak** dibeli: stack yang dibiarkan berhenti tanpa teardown eksplisit memang
meninggalkan sandbox dan volumenya — itu keputusan, bukan kelalaian, dan pesan log saat shutdown
menyebutkan kedua jalur untuk membereskannya. Control plane yang kena SIGKILL (`docker kill`, OOM,
host mati) juga tidak menghapus apa pun, yang sekarang sama dengan perilaku normalnya.

### 3.10 Halaman yang menjawab 4xx/5xx bukan navigasi yang berhasil

**Keputusan.** `navigate` membaca status HTTP dokumen utamanya, dan `status >= 400` pada dokumen
yang sudah `complete` dijawab `502 {"error":"navigation failed","url":...,"final_url":...,"http_status":<status>}`.
Statusnya diambil dari `performance.getEntriesByType("navigation")[0].responseStatus` di target CDP
yang sama dengan polling `readyState` yang sudah ada (tiap 100 ms), jadi tidak ada permintaan kedua
dan tidak ada `fetch` yang bisa berbenturan dengan CORS. Jawaban `200` membawa `http_status` juga;
`0` berarti "tidak ada jawaban HTTP" (`about:blank`, `data:`), dan pada `502` field itu hanya muncul
bila ada angkanya.

**Alasan.** `404` dari server tetap `readyState: "complete"`, dengan judul yang terlihat seperti
halaman biasa (`404 Not Found`), dan jawaban `200 {"navigated":true}` untuk halaman seperti itu
adalah kebohongan yang paling mudah dipercaya: worker yang menerimanya akan lanjut membaca layar
yang isinya pesan error, dan kegagalan itu muncul jauh di hilir sebagai "modelnya salah membaca
halaman". Verifikasi load yang lama hanya membuktikan *sesuatu* sudah dimuat, bukan bahwa yang
dimuat adalah halaman yang diminta.

**Batasnya.** Yang diperiksa adalah status dokumen utamanya, bukan status setiap subresource, dan
bukan isi halaman: halaman yang menjawab `200` dengan pesan error di dalamnya tetap lolos — itu
penilaian model, bukan penilaian control plane. Halaman error yang tidak punya jawaban HTTP
(`chrome-error://`, timeout) tetap gagal lewat jalur yang sudah ada, tanpa `http_status`.

### 3.11 `scripts/e2e.sh`: berhenti, bukan mencatat, saat alamatnya salah

**Keputusan.** `STACK` dan `VCOMPUTER_CONTROL_PORT` wajib (`${VAR:?…}`), tanpa default; build,
`compose up`, dan `/health` yang gagal menghentikan run lewat `finish()`; dan sebelum permintaan
pertama yang mengubah apa pun, skrip membuktikan port yang disapanya milik stacknya —
`docker ps --filter publish=$PORT --format '{{.Names}}'` harus menjawab `${STACK}-control`, dan
label `com.docker.compose.project` container itu harus `$STACK`. Bila pembuktian itu tidak pernah
terjadi, `finish()` melewati bagian HTTP dari teardown-nya (`purge` dan `admin/cleanup`) dan hanya
menjalankan `cleanup.sh --running --volumes` yang sudah dibatasi label dan awalan nama.

**Alasan.** Default `STACK=vcomputer` dan `VCOMPUTER_CONTROL_PORT=18090` adalah default ke stack
yang paling mungkin sudah hidup di host itu, dan kedua bahayanya berbeda: `STACK` menentukan setiap
nama yang **dihapus**, sedangkan port menentukan ke mana setiap permintaan **dikirim** — dan port
tidak bisa diturunkan dari `STACK`, karena compose mempublikasikan apa pun yang dikatakan
`VCOMPUTER_CONTROL_PORT`. Di host yang sudah menjalankan stack demo, `compose up` gagal pada bind,
dan kegagalan itu dulu hanya dicatat sebagai note sehingga `DELETE`, `purge`, dan `admin/cleanup`
berikutnya tetap pergi ke control plane yang memang memiliki port itu: 401 bila tokennya berbeda,
penghapusan sandbox stack lain bila tokennya kebetulan sama. Pemeriksaan identitas itu karena itu
bukan kehati-hatian tambahan melainkan syarat agar sisa skrip boleh berjalan.

**Yang dibeli, dan yang tidak.** Yang dibeli: satu run tidak bisa menyentuh stack yang tidak
disebutnya, dan kegagalan precondition tidak lagi menyamar sebagai kegagalan pemeriksaan.
Yang **tidak** dibeli: skrip ini tetap tidak tahu apa yang ada di host, jadi dua run paralel dengan
`STACK` berbeda tetap aman hanya karena port dan nama yang berbeda — bukan karena skripnya
mengoordinasikan apa pun.

## 4. Konsekuensi

- **Bukan sandbox untuk kode tak tepercaya.** Prompt injection dari halaman yang dibaca browser tetap
  kelas risiko baru (riset §7), dan pertahanannya tetap gerbang persetujuan, bukan container ini.
- **Biaya mengikuti jumlah bot, bukan aktivitas.** Satu sandbox per bot berarti RAM 24/7 per bot
  sampai ada *reaper* idle + snapshot; PoC tidak punya keduanya, dan itu sebabnya ia tidak boleh
  dibawa apa adanya ke produksi.
- **Desktop (VNC) adalah lapisan termahal dan paling tidak perlu.** Riset menempatkannya di Fase 4;
  ia ada di PoC karena isu #4 meminta "komputer" yang bisa dilihat, dan karena tangkapan layar
  adalah bukti visual termurah untuk PR.
- **`navigate` membuktikan halamannya termuat, bukan hanya perintahnya terkirim.** Control plane
  menyambung ke websocket target CDP yang baru, memanggil `Page.enable`, membaca
  `document.readyState`/`location.href`, dan menunggu event load sampai
  `VCOMPUTER_NAVIGATE_TIMEOUT` (default `15s`). Halaman yang berakhir di `chrome-error://`, tidak
  selesai dimuat, **atau menjawab 4xx/5xx pada dokumen utamanya** dijawab
  `502 {"error":"navigation failed",...,"http_status":<status>}`, bukan `200`. Tanpa itu, "navigate
  sukses" berarti "perintahnya terkirim", dan halaman error lolos sebagai bukti (§3.10).
- **Sisa sandbox bisa dibuang dengan sengaja, dan volumenya ikut kalau diminta.**
  `DELETE /v1/sessions/<bot>` tetap berarti "hentikan container, simpan volume"; `?purge=true`
  menambahkan `docker rm -f` container dan `docker volume rm` volume rumahnya, jadi `Ensure`
  berikutnya mulai dari profil kosong. Untuk sisa yang tidak lagi punya record sesi,
  `POST /v1/admin/cleanup[?volumes=true]` menghapus container berlabel `vcomputer.bot` yang tidak
  jalan dan — bila `volumes=true` — volume berlabel sama, dengan jawaban
  `{"removed_containers":N,"removed_volumes":M}`. Label itu dipasang saat container dan volume
  dibuat justru supaya pembersihan ini bisa menargetkan dirinya sendiri tanpa menyentuh milik stack
  lain; label `vcomputer.stack=<prefix>` yang membuat cakupannya tepat, bukan awalan nama (#263).
- **Teardown adalah tindakan eksplisit, bukan efek samping dari berhentinya proses.** Sandbox tidak
  ada di file compose, jadi `docker compose down` tidak bisa membuangnya; yang membuangnya adalah
  `scripts/cleanup.sh --running --volumes` dengan `STACK` yang disebut, atau — bila operator memang
  ingin prosesnya yang membereskan — control plane yang dijalankan dengan
  `VCOMPUTER_REAP_ON_SHUTDOWN=1` (§3.9). SIGTERM biasa **tidak** menghapus apa pun: ia adalah
  restart, dan volume yang akan ikut terhapus adalah sesinya. `scripts/e2e.sh` membuktikan keduanya:
  `restart-*` (sesi, volume, dan berkas di `/home/bolu` bertahan melewati `compose restart`) dan
  `TEARDOWN=1` (nol container, nol volume, nol jaringan bernama `${STACK}-*`).
- **Tidak ada perubahan pada jalur produksi.** Paket `internal/modules/vcomputer` dan control plane
  belum didaftarkan di `container`/`agent-worker` saat keputusan ini diambil, jadi `api` dan worker
  berperilaku persis seperti sebelumnya. Revisi 2026-10-16 mengubah bagian ini: activity-nya
  sekarang terdaftar di agent worker lewat `container.RegisterVComputerTasks`, sementara loop
  tugas agennya tetap tidak berubah.
- **Urutan yang tetap berlaku.** Gerbang 1 (E14.6, #118) tetap penentu apakah ini dilanjutkan:
  tanpa bukti tugas pelanggan yang gagal tanpa browser/terminal, Fase 2 tidak dimulai.

## 5. Dampak ke repositori
- `docs/adr/0003-virtual-computer.md` — dokumen ini.
- `apps/virtual-computer/README.md` — diagram arsitektur, daftar permukaan API + aturan token, semantik `navigate` dan `cdp_url`, rahasia sandbox, tabel environment, cara menjalankan (uji modul dan `scripts/e2e.sh`), pembersihan sisa (purge + `cleanup.sh`), trade-off `docker.sock`, dan daftar hal yang masih di-stub.
- `apps/virtual-computer/sandbox/{Dockerfile,entrypoint.sh}` — image: Xvfb → fluxbox → x11vnc → websockify/noVNC → Chromium, plus `vcomputer-gate` (dibangun di satu stage `Dockerfile` dan disalin ke image) sebagai satu-satunya listener di alamat jaringan container.
- `apps/virtual-computer/sandbox/gate/` — `vcomputer-gate`: modul Go sendiri (hanya stdlib, tanpa `go.sum`), karena ia berjalan di domain kepercayaan yang berbeda dari control plane. Menyajikan `/healthz` terbuka dan memproksikan `/cdp/*` + `/novnc/*` ke loopback setelah memeriksa `X-Sandbox-Secret`.
- `apps/virtual-computer/control/` — control plane (modul Go terpisah, hanya stdlib) plus `Dockerfile`-nya (`docker:27-cli`, berjalan sebagai root dengan socket host ter-mount): `Runtime` dengan `InMemoryRuntime` (tanpa Docker, dipakai uji) dan `DockerRuntime` (nyata, lewat CLI docker), siklus hidup sesi termasuk `purge`, klien gate (rahasia per sandbox, proxy `/cdp/` dan `/novnc/`), verifikasi load di `navigate` (termasuk status HTTP dokumennya), `admin/cleanup`, dan `Reap` yang **hanya** jalan bila `VCOMPUTER_REAP_ON_SHUTDOWN=1` (§3.9).
- `apps/virtual-computer/docker-compose.poc.yml` — service `vcomputer-control` (dua jaringan, port hanya di loopback) dan `vcomputer-fixture` (`busybox httpd` di jaringan internal), plus service `vcomputer-sandbox` sebagai target build di profil `build`. Nama-namanya diparameterkan (`VCOMPUTER_STACK` dan kawan-kawan) dengan default yang menjaga nama aslinya, supaya dua stack bisa hidup berdampingan di satu host. Sandbox per bot tidak ada di file ini: runtime yang membuatnya, dengan volume per bot.
- `apps/virtual-computer/fixture/index.html` — halaman uji yang bisa dibuka browser sandbox tanpa internet.
- `apps/virtual-computer/scripts/{e2e.sh,cleanup.sh}` — checklist end-to-end (build, naikkan stack, periksa, tulis bukti, cetak PASS/FAIL) dan pembersih CLI container/volume sisa. `e2e.sh` menuntut `STACK` dan `VCOMPUTER_CONTROL_PORT`, berhenti saat precondition-nya gagal, membuktikan port yang disapanya milik stacknya, membuktikan sesi bertahan melewati `compose restart` control plane, dan dengan `TEARDOWN=1` membuktikan tidak ada sisa `${STACK}-*` (§3.9, §3.11); `cleanup.sh` menuntut `STACK` dan menerima `--running` untuk ikut membuang sandbox yang sedang jalan (§3.9).
- `apps/virtual-computer/demo/index.html` — halaman statis yang menyematkan noVNC asli lewat proxy control plane; tanpa mock desktop.
- `apps/backend/internal/modules/vcomputer/{domain,service,temporal,workflow}` — port `ComputerUseClient` (termasuk `Navigate`), klien HTTP dengan bearer token, activity-nya, dan `SmokeWorkflow`. **Terdaftar di agent worker** sejak Revisi 2026-10-16 (`internal/container/vcomputer.go`, `cmd/vcomputer-smoke`). Sejak Revisi QA PR #234, `502 {"error":"navigation failed"}` dipetakan ke sentinel `ErrNavigationFailed` dan dijadikan **non-retryable** di Temporal; hanya kegagalan transport yang tetap bisa diulang.
- `go.work` — memuat modul control dan modul gate supaya `go test` di dalamnya jalan dari checkout; modul produk tidak berubah.

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
  dan mengabaikan `--remote-debugging-address=0.0.0.0`, jadi CDP memang tidak bisa
  dipublikasikan dari dalam sandbox. Revisi ini mulanya menambahkan proses penerus port di
  alamat container; bentuk yang berlaku sekarang adalah gate (§3.8), yang meneruskan ke
  loopback tanpa membuka CDP sendiri, dan `cdp_url` yang berupa path control plane — lihat
  revisi lanjutan di bawah.
- **Jaringan internal**: `vcomputer-internal` (`internal: true`) hanya memuat sandbox dan
  satu service fixture — tanpa egress dari dalam container. Control plane satu-satunya
  anggota kedua jaringan (`vcomputer-internal` + `vcomputer-edge`). Catatan: `internal: true`
  tidak memblokir host → container, jadi perlindungan port sandbox datang dari gate, bukan
  dari bentuk jaringan ini.
- **noVNC lewat proxy**: `/v1/sessions/{bot}/novnc/...` diteruskan `httputil.ReverseProxy`
  ke gate sandbox berikut upgrade websocket, sehingga browser hanya perlu menjangkau
  control plane.
- **Token wajib**: `VCOMPUTER_CONTROL_TOKEN` tidak punya default dan control plane menolak
  start tanpa itu; setiap rute `/v1/*` butuh bearer (perbandingan constant-time), dan URL
  noVNC boleh menukar `?token=` dengan cookie `vc_token` (`HttpOnly`, `SameSite=Strict`,
  `Path=/v1/sessions/`). `/health` tetap terbuka.
- **Port per sandbox, bukan `6080` hardcoded**: `VCOMPUTER_SANDBOX_NOVNC_PORT` dan port gate
  dikirim ke setiap container. Sandbox tidak pernah di-`-p`, dan hanya control plane yang
  dipublikasikan, itupun di loopback.
- **Trade-off `/var/run/docker.sock` diterima untuk PoC**: container control me-mount socket
  host dan berjalan sebagai root, yang setara akses root di host; itu lubang terbesar di
  PoC ini dan dinyatakan terbuka di README. Jalur produksinya tetap klien Docker API di node
  sandbox — tidak pernah socket di dalam container (riset §4.2).
- **`go.work` memuat modul control**, jadi ujinya jalan dari checkout; modul produk
  tidak berubah.

Catatan supersesi: tabel di §3.2 dan baris "Jaringan" di Ringkasan sudah diperbarui oleh revisi
lanjutan di bawah; §3.6 (control API tidak pernah publik) tetap berlaku apa adanya — control plane
tetap hanya di `127.0.0.1` pada host.

## Revisi lanjutan 2026-10-16: gate sandbox, rahasia per sandbox, purge

Revisi ini menutup lubang yang paling mudah dijangkau di PoC: sampai di sini, CDP dan noVNC terbuka
bagi apa pun yang bisa menyapa alamat sandbox di jaringan internal — termasuk container lain di
jaringan itu. Yang berubah:

- **`vcomputer-gate` menjadi satu-satunya listener di alamat jaringan sandbox** (§3.8). Modul Go
  sendiri, hanya stdlib, dibangun di satu stage image sandbox dan disalin ke `/usr/local/bin`. Ia
  memproksikan `/cdp/*` ke `127.0.0.1:9222` dan `/novnc/*` ke `127.0.0.1:${NOVNC_PORT}`, menulis
  ulang Host menjadi `localhost`, meneruskan upgrade websocket, dan membiarkan `/healthz` terbuka.
- **Rahasia per sandbox**: `SANDBOX_SECRET`, 32 karakter hex dari `crypto/rand`, di-generate control
  plane saat `Ensure`, dikirim sebagai `-e`, dan diperiksa gate lewat header `X-Sandbox-Secret`
  (constant-time). Ia tidak pernah masuk jawaban JSON mana pun, dan setelah control plane restart ia
  dibaca kembali dari `docker inspect --format '{{json .Config.Env}}'`.
- **`cdp_url` berubah arti**: dari `http://<ip>:<port>` menjadi path control plane
  `/v1/sessions/<bot>/cdp`. Rute itu (method apa pun, termasuk websocket) mem-proxy ke gate, dan
  `webSocketDebuggerUrl` di jawaban `/json/list` dan `/json/new` ditulis ulang ke prefix yang sama —
  jadi pemegang token control plane bisa mengendarai CDP tanpa pernah tahu alamat container. Method
  sengaja tidak disaring: DevTools HTTP API bukan API seragam-method (`/json/list` dan `/json/version`
  `GET`, `/json/new` `PUT`, `/json/close` `GET`), dan rute yang menyebut method akan menjawab `405`
  untuk permintaan yang sah.
- **Satu port per sandbox, bukan dua**: `VCOMPUTER_SANDBOX_GATE_PORT` (default `8443`) adalah port
  yang disapa control plane untuk CDP maupun noVNC, karena gate yang memutuskan ke mana setiap
  path pergi; `VCOMPUTER_SANDBOX_NOVNC_PORT` tetap ada dan sekarang jelas berarti port **di dalam**
  sandbox, tempat websockify mengikat loopback.
- **`VCOMPUTER_SANDBOX_NAME_PREFIX`** (default `vcomputer`) menentukan nama container
  (`<prefix>-<bot>`) dan volume (`<prefix>-<bot>-home`), dan ditulis ke label `vcomputer.stack`
  setiap container dan volume — label itulah yang jadi cakupan pembersihan, supaya dua stack di satu
  host tidak saling menghapus (#263).
- **`navigate` memverifikasi load-nya** dan menjawab `502 {"error":"navigation failed",...}` bila
  halaman berakhir di `chrome-error://` atau tidak selesai dimuat dalam `VCOMPUTER_NAVIGATE_TIMEOUT`
  (default `15s`).
- **Purge dan pembersihan**: `DELETE /v1/sessions/<bot>?purge=true` menghapus container **dan**
  volume rumahnya; `POST /v1/admin/cleanup[?volumes=true]` menyapu container berlabel
  `vcomputer.bot` yang tidak jalan dan — bila `volumes=true` — volume berlabel sama. Volume
  sekarang dibuat dengan label `vcomputer.bot=<bot id>` dan `vcomputer.stack=<prefix>` supaya
  penyapuan itu bisa menargetkan dirinya sendiri secara tepat.
- **Stack yang bisa digeser namanya**: `scripts/e2e.sh` menurunkan setiap nama dari `STACK`
  (`${STACK}-internal`, `${STACK}-edge`, `${STACK}-sandbox:poc`, `${STACK}-control:poc`,
  `${STACK}-control`, `${STACK}-<bot>`), menghormati `VCOMPUTER_CONTROL_TOKEN` yang sudah diisi,
  menerima `BOT`, `PREBUILT=1`, `BUILD_NETWORK=host`, dan `TEARDOWN=1` (purge + cleanup + `compose
  down`, tanpa meninggalkan volume). Compose menerima `VCOMPUTER_STACK`,
  `VCOMPUTER_CONTROL_IMAGE`, `VCOMPUTER_CONTROL_CONTAINER`, `VCOMPUTER_FIXTURE_CONTAINER`,
  `VCOMPUTER_SANDBOX_IMAGE`, `VCOMPUTER_INTERNAL_NETWORK`, `VCOMPUTER_EDGE_NETWORK`, semuanya
  dengan default yang menjaga nama aslinya. `scripts/cleanup.sh` adalah padanan CLI dari
  `admin/cleanup`, untuk dipakai saat tidak ada yang bisa menjawab HTTP.
- **Koreksi yang menyertai**: klaim "host tidak punya rute ke port sandbox" yang ada di versi
  pertama dokumen ini dan di README **salah** dan sudah dihapus; penjelasannya di §3.8.

## Revisi 2026-10-16 setelah QA PR #211

QA menemukan tiga hal di jalur konfigurasi worker → control plane, semuanya sudah diperbaiki:

- **Token salah bukan token kosong.** Control plane menjawab dua `401` yang berbeda:
  `{"error":"a bearer token is required"}` bila tidak ada kredensial sama sekali, dan
  `{"error":"the token is not valid"}` bila kredensialnya ada tapi tidak cocok
  (`internal/httpapi`). Kalimat itu diteruskan klien apa adanya, jadi laporan run menyebut
  token yang salah sebagai token yang salah, bukan menyuruh operator mencari variabel yang
  sudah diisi.
- **Kesalahan konfigurasi tidak lagi di-retry.** Activity membungkus `ErrNotConfigured`
  sebagai `temporal.NewNonRetryableApplicationError` bertipe `VComputerNotConfigured`
  (`internal/modules/vcomputer/temporal`), dengan `ErrNotConfigured` tetap sebagai cause
  sehingga `errors.Is` di sisi dalam proses (klien dan activity) tidak berubah; di sisi
  workflow yang bertahan hanya tipe dan kalimatnya, jadi cabangnya lewat pesan. Alamat kosong
  maupun token yang ditolak karenanya gagal pada attempt pertama; `ErrUnavailable`,
  `ErrInvalidInput`, dan `ErrNoSession` tetap retryable, karena sandbox yang belum siap memang
  layak ditanya lagi.
- **Alamat tanpa token tidak lagi "configured" tanpa catatan.** `NewVComputerTasks` mencatat
  `Warn` yang menyebut `VCOMPUTER_CONTROL_TOKEN` kosong dan bahwa setiap panggilan akan
  dijawab `401`. Kliennya tetap dibangun — yang dilaporkan jadi penolakan control plane,
  bukan "tidak terkonfigurasi" yang menyembunyikan variabel mana yang kurang.

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
  saat start. `VCOMPUTER_CONTROL_TIMEOUT` yang tidak bisa di-parse **bukan** kesalahan startup
  (revisi 2026-10-16, isu #219): ia dicatat sebagai warning dan nilai dari berkas konfigurasi yang
  berlaku, karena fitur opsional tidak boleh mematikan worker yang juga menjalankan tugas, rutin,
  dan memori.
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

Catatan supersesi: urutan langkah di atas tidak lagi berakhir di `Exec` — revisi setelah QA PR
#211 menambahkan `StopSession` sebagai langkah terakhir. Sisa butirnya masih berlaku.

Batas §1 dan §4 tentang jalur produksi karena itu berlaku dengan satu pengecualian: yang
terdaftar adalah activity dan satu workflow smoke, bukan loop tugas agennya.

## Revisi 2026-10-16 setelah QA PR #211 (isu #220): smoke run mematikan sesinya sendiri

`SmokeWorkflow` menjalankan `EnsureSession` → … → `Exec` tanpa pernah memanggil `StopSession`,
jadi setiap smoke run meninggalkan container `vcomputer-<bot>` beserta volumenya dalam keadaan
running; QA sempat menyisakan tujuh container yang harus dihapus manual. Activity `StopSession`
sudah terdaftar tapi tidak punya bukti end-to-end lewat Temporal karena tidak ada yang
menjadwalkannya.

- **Cleanup-nya bagian dari workflow, bukan langkah opsional di pemanggil.** `StopSession`
  dijadwalkan lewat `defer` di atas `workflow.NewDisconnectedContext`, jadi ia jalan di setiap
  jalur — termasuk saat sebuah langkah gagal, yang justru kasus yang meninggalkan container.
  Context terpisah itu yang membuatnya tetap jalan ketika workflow-nya sendiri sudah gagal atau
  dibatalkan; `defer` biasa akan menjadwalkan stop ke context yang sudah selesai.
- **Run yang gagal men-stop adalah run yang gagal.** Kegagalan `StopSession` dikembalikan sebagai
  error workflow, bukan dilaporkan sebagai catatan: sandbox yang tidak mau mati adalah container
  yang tertinggal, dan hasil sukses yang menyisakannya justru keadaan yang tidak akan dilihat
  lagi.
- **`keep_session` membalik perilaku itu**, untuk kasus desktop-nya memang mau dilihat setelah
  run; `session_stopped` di hasil menyatakan mana yang terjadi. `cmd/vcomputer-smoke` meneruskan
  ini sebagai `-keep-session`, dan tangkapan `-out` butuh flag itu — capture-nya diminta setelah
  workflow selesai, ke sandbox yang tanpa `-keep-session` sudah mati, jadi `-out` tanpa flag itu
  ditolak sebelum worker-nya jalan.
- **Ini yang membedakan smoke run dari tugas.** `StopSession` sengaja **tidak** dipanggil di
  akhir tugas, karena sesi yang berakhir bersama tugasnya kehilangan profil browser yang justru
  disimpan volume itu; smoke run tidak punya penerus yang memakai profil itu, dan pemilik sandbox
  yang baru dibuatnya adalah run itu sendiri.
- **`StopSession` bukan purge, dan hasilnya berbeda per runtime.** Di runtime `memory`,
  `Stop` melupakan record sesi, jadi `GET /v1/sessions/<bot>` sesudahnya `404`. Di runtime
  `docker` yang nyata, `Stop` hanya `docker stop` dan menjawab `200` (`{"stopped":true}`;
  `GET` sesudahnya `200` dengan `state:"stopped"`): container `vcomputer-<bot>` tertinggal
  dalam keadaan Exited bersama volume `vcomputer-<bot>-home`, dan keduanya hanya hilang lewat
  teardown eksplisit — bukan `404`.

## Revisi 2026-10-16 setelah QA PR #234

QA menolak PR #234 karena empat hal: teardown yang didokumentasikan tidak membersihkan sandbox,
`scripts/e2e.sh` bisa mengarah ke stack lain, halaman error lolos sebagai navigasi yang berhasil,
dan kegagalan navigasi diulang tanpa guna oleh Temporal. Tiga yang terakhir dicatat di sini;
yang pertama diperbaiki dengan `Reap` saat SIGTERM, dan perbaikan itu dibalik lagi oleh #263 —
lihat bagian #263 di bawah. Keputusan di atas tetap berlaku kecuali di bagian yang disebut di sini.

- **`scripts/e2e.sh` menuntut `STACK` dan `VCOMPUTER_CONTROL_PORT`, dan berhenti saat precondition-nya
  gagal** (§3.11). Keduanya `${VAR:?…}`, tanpa default `vcomputer`/`18090`; build, `compose up`, dan
  `/health` yang gagal memanggil `finish()` alih-alih melanjutkan ke pemeriksaan berikutnya; dan
  sebelum permintaan yang mengubah apa pun, skrip membuktikan `127.0.0.1:$PORT` dipegang
  `${STACK}-control` dengan label project `$STACK`. Bila pembuktian itu tidak terjadi, teardown
  melewati `purge` dan `admin/cleanup` dan hanya menjalankan `cleanup.sh --running --volumes`.
- **`navigate` menolak halaman 4xx/5xx** (§3.10). Status HTTP dokumen utamanya dibaca dari
  `performance.getEntriesByType("navigation")[0].responseStatus` di polling yang sama, `status >= 400`
  dijawab `502 {"error":"navigation failed",...,"http_status":<status>}`, dan `200` membawa
  `http_status` juga. `scripts/e2e.sh` menambah pemeriksaan `navigate-404-*` terhadap fixture
  (`/tidak-ada-404`), dan `navigate-fixture-http-status` untuk sisi `200`-nya.
- **Kegagalan navigasi tidak diulang lagi.** `502 {"error":"navigation failed"}` dipetakan klien
  backend ke sentinel `domain.ErrNavigationFailed` (bukan `ErrUnavailable`), dan activity `Navigate`
  membungkusnya dengan `temporal.NewNonRetryableApplicationError` bertipe `NavigationFailed`: URL yang
  sama akan menjawab hal yang sama, jadi percobaan kedua hanya membuang waktu dan kuota. Yang tetap
  bisa diulang: kegagalan transport dan control plane yang tidak menjawab (`ErrUnavailable`).
  `cmd/vcomputer-smoke` sekarang memakai `http://vcomputer-fixture:8080/` sebagai contoh URL-nya,
  karena sandbox memang tidak punya egress.

## Revisi 2026-10-16: teardown hanya eksplisit (#263)

#263 menemukan bahwa memperbaiki #245 dengan menghapus sandbox saat control plane berhenti
menukar satu bug dengan bug yang lebih mahal: SIGTERM adalah restart (`compose restart`,
`up -d --force-recreate`, daemon docker yang di-restart), dan yang ikut terhapus bersama sandbox
adalah volume `/home/bolu` — profil Chromium dan berkas bot itu. Keputusan di atas tetap berlaku
kecuali di bagian yang disebut di sini.

- **`Reap` tidak lagi berjalan pada SIGTERM** (§3.9). Penghapusan sandbox saat proses berhenti
  menjadi opt-in `VCOMPUTER_REAP_ON_SHUTDOWN=1` (hanya `1`/`true`, default mati); teardown yang
  didokumentasikan adalah `scripts/cleanup.sh --running --volumes` dengan `STACK` yang disebut.
  `scripts/e2e.sh` membuktikan kedua arahnya: `restart-*` (`compose restart` pada control plane,
  tanpa opt-in, lalu sandbox masih jalan, volume masih ada, berkas penanda di `/home/bolu` masih
  terbaca, dan sesinya jalan lagi) dan `teardown-*` (nol container, nol volume, nol jaringan
  bernama `${STACK}-*`).
- **Cakupan `Cleanup`/`Reap` adalah label `vcomputer.stack=<prefix>`, bukan `name=^<prefix>-`**
  (§3.9). Awalan nama juga mencocokkan stack lain yang namanya berawalan sama — `vcomputer` cocok
  dengan `vcomputer-qa` — jadi penghapusan yang di-scope dengan awalan bisa mengambil sandbox dan
  volume stack lain. Runtime menulis label itu saat membuat container dan volume.
- **`scripts/cleanup.sh` menuntut `STACK`** (§3.9). Sebelumnya default-nya `vcomputer`, yaitu satu
  nilai yang membuat pembersihan menyasar stack yang mungkin sedang dipakai demo; sekarang
  `${STACK:?…}`, sama seperti `e2e.sh` (§3.11).
- **`DELETE /v1/sessions/<bot>` tetap menyisakan volume**; `?purge=true` tetap ada sebagai opt-in
  eksplisit untuk satu bot (§3.5).

## Keputusan yang tetap terbuka

- **Cakupan akhir**: per bot (isu #4) atau per workspace (riset §6) — dan bagaimana `bot_id` dipetakan bila kelak per workspace.
- **Cara model mencapainya**: server MCP workspace-scoped (riset §5.1) atau adapter native; dan bagaimana `computer.exec` dilabeli — seluruhnya `write_external`, atau allowlist perintah `read`-only.
- **Layar sebagai sumber daya eksklusif**: lease (`computer:<id>:screen`, pola yang sudah ada di repo) belum dibangun; dengan satu sandbox per bot ia belum dibutuhkan.
- **On/off dan biaya**: reaper idle, snapshot, kuota per paket — tidak ada di PoC.
- **Isolasi**: kapan pindah ke gVisor + Kubernetes (riset Fase 2), dan node sandbox mana yang dipakai.
- **Rahasia gate**: `SANDBOX_SECRET` statis seumur sandbox dan bisa dibaca dari env container oleh siapa pun yang punya akses docker. Rotasi, penyimpanan di secret store, atau penggantinya (mTLS per sandbox, gate dengan identitas sendiri) belum diputuskan.
- **Persetujuan**: bentuk draf untuk aksi sandbox (tangkapan layar + aksi yang akan dikirim, bukan "jalankan perintah") — memperluas komponen draf yang sekarang hanya membawa `action_kind` + `payload`.
