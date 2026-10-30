# ADR 0004 — Daytona: satu sandbox per Bolu menggantikan `apps/virtual-computer`

- Status: **Diterima**
- Tanggal: 2026-10-30
- Isu: #397 — *Epic: ganti `apps/virtual-computer` dengan Daytona — sandbox per Bolu tanpa control plane sendiri*
- Tindak lanjut dokumen: #403 — ADR ini dan pelurusan referensi runtime
- Menggantikan: [ADR 0003](0003-virtual-computer.md) — *Virtual Computer: satu sandbox Docker per bot untuk PoC*
- Terkait: [ADR 0001](0001-temporal-dan-region.md) (region Jakarta, *claim check*), [ADR 0002](0002-kepatuhan-uu-pdp.md) (data pelanggan & rahasia), [docs/api.md](../api.md) § *Tool labels decide approval*

## Ringkasan keputusan

| Pertanyaan | Keputusan |
| --- | --- |
| Runtime sandbox | **Daytona**, bukan control plane sendiri |
| Unit | **Satu sandbox per Bolu**, dibuat lewat API/SDK Daytona |
| Kelas resource | `daytona-small` / `daytona-medium` / `daytona-large`; Linux VM dan Windows |
| Kendali agen | **Computer Use**: mouse, keyboard, screenshot, display/window list |
| Lihat layar + ambil alih | **VNC** lewat bridge WebSocket `wss://toolbox.daytona.com/vnc/{sandboxId}` (Fase 2, #302) |
| Start & sesi berlanjut | **Snapshot & warm pool** |
| Klien | SDK TypeScript `@daytona/sdk`, dipakai `apps/backend-js` |
| `apps/virtual-computer` | **Dipensiunkan** lewat #402 |
| Kontrak tool | **Tidak berubah**: label tool dan gerbang persetujuan tetap |

## 1. Konteks

**Fakta yang mengikat.** Dua PR hardening `apps/virtual-computer` — #234 (+9.392/−649) dan
#274 (+10.457/−708) — tidak akan lolos gate tick: terlalu besar untuk dibaca. Selama keduanya
belum di-merge, setiap issue runtime menumpuk tanpa ada yang bisa mendarat.

Sementara itu `apps/virtual-computer` masih PoC, dan daftar utangnya panjang:

| Isu | Utang |
| --- | --- |
| #192 | `docker.sock` di dalam container control setara akses root di host |
| #261 | Runtime sandbox belum gVisor/rootless |
| #263 | Reaper menghapus volume sesi saat restart |
| #245 | Teardown meninggalkan sandbox/volume/network |
| #244, #191 | `e2e.sh` menembak stack lain |
| #246, #189 | `/navigate` melaporkan sukses untuk halaman error |

**Kenapa berhenti.** Daftar itu bukan pekerjaan yang membuat Timbolu berbeda; itu pekerjaan yang
sudah disediakan Daytona. Membangunnya sendiri berarti menambah control plane yang harus dirawat
selamanya — beban tetap yang justru dihindari saat memilih Temporal Cloud ([ADR 0001](0001-temporal-dan-region.md)).

## 2. Keputusan

### 2.1 Satu sandbox per Bolu, lewat API/SDK

**Keputusan.** Setiap Bolu mendapat **satu sandbox Daytona**, dibuat dan dihentikan lewat API/SDK
Daytona. Kelas resource tersedia sebagai `daytona-small`, `daytona-medium`, dan `daytona-large`,
untuk Linux VM maupun Windows.

**Alasan.** Ini unit yang sama dengan PoC (satu sandbox per bot), tetapi yang mengurus pembuatan,
jaringan, dan pembersihannya adalah penyedia. Tidak ada `docker.sock`, tidak ada runtime container
yang harus dikeraskan sendiri, dan tidak ada control plane yang harus dirawat.

### 2.2 Computer Use sebagai permukaan kendali agen

**Keputusan.** Aksi agen memakai **Computer Use** Daytona: mouse, keyboard, screenshot, serta
display/window list — programatik, bukan lewat desktop yang di-stream.

**Alasan.** Permukaan ini yang dibutuhkan agen untuk bekerja, dan ia disediakan sebagai API. PoC
membangunnya sendiri dari `xdotool` + `scrot` di dalam container.

### 2.3 VNC untuk "lihat layar + ambil alih"

**Keputusan.** Tampilan layar dan pengambilalihan manusia memakai **VNC** Daytona lewat bridge
WebSocket `wss://toolbox.daytona.com/vnc/{sandboxId}` (bearer API key atau `rlp_token`).

**Alasan.** "Lihat layar Bolu + ambil alih" (Fase 2, #302) menjadi pekerjaan meng-embed, bukan
membangun proxy noVNC, token exchange, dan penerusan WebSocket seperti di PoC.

### 2.4 Snapshot & warm pool

**Keputusan.** Sesi yang berlanjut dan start cepat memakai **snapshot & warm pool** Daytona.

**Alasan.** PoC tidak punya keduanya; volume per bot adalah satu-satunya persistensi, dan itu
sumber isu #263 dan #245.

### 2.5 Klien TypeScript di `apps/backend-js`

**Keputusan.** Integrasi memakai SDK TypeScript **`@daytona/sdk`** di `apps/backend-js`, backend
yang melayani produksi.

**Alasan.** SDK resminya TypeScript; tidak perlu menulis klien HTTP sendiri atau menjembatani
runtime lain.

## 3. Batasan yang dihormati

- **VNC/Computer Use butuh sandbox dari default snapshot.** Image kustom harus memasang paket
  desktop sendiri: `xvfb`, `xfce4`, `x11vnc`, `novnc`, `dbus-x11`.
- **Snapshot Linux (full) terikat region + runner** yang mengambilnya.

Keduanya membatasi kapan image kustom boleh dipakai: jalur default adalah snapshot bawaan, dan
image kustom hanya bila memang perlu, dengan paket desktop lengkap.

## 4. Konsekuensi

- **Kontrak tidak berubah.** Label tool dan gerbang persetujuan ([docs/api.md](../api.md)
  § *Tool labels decide approval*) tetap: nama tool yang sama, batas izin yang sama. Yang diganti
  adalah eksekutornya, bukan kontraknya. Karena itu tidak ada perubahan pada `tool_catalog`,
  grant, maupun alur draf/parkir `waiting_approval`.
- **`apps/virtual-computer` dipensiunkan lewat #402**, termasuk `docker-compose.vcomputer.yml`
  dan `scripts/e2e.sh`. Sampai #402 mendarat, instruksi menjalankan PoC di
  [docs/deployment.md](../deployment.md) dan [apps/virtual-computer/README.md](../../apps/virtual-computer/README.md)
  tetap berlaku.
- **Issue runtime lama menjadi usang**: #192, #261, #263, #245, #244, #246, #191, #190, #189.
  Tidak ditutup sekarang; ditutup saat #402 mendarat, supaya jejaknya jelas kenapa mereka berhenti.
- **Pekerjaan dipecah kecil**, satu PR per potong, dan urutannya mengikat:

  | Potong | Isi |
  | --- | --- |
  | #398 | Klien + config, tanpa mengubah tool surface |
  | #399 | Peta workspace→sandbox dan siklus hidup |
  | #400 | Pindahkan tool vcomputer ke Computer Use |
  | #401 | Computer view + takeover di web |
  | #402 | Pensiunkan `apps/virtual-computer` |

  Klien dulu, lalu lifecycle, lalu tool; UI takeover menyusul setelah tool jalan.
- **Sandbox tidak lagi berjalan di host kami.** Apa yang boleh masuk sandbox — dan apa yang tidak
  pernah boleh ada di sana — tetap tunduk pada [ADR 0002](0002-kepatuhan-uu-pdp.md) dan aturan
  "rahasia tidak masuk sandbox" di [ADR 0003](0003-virtual-computer.md) §3.4.
- **Dokumen yang menyatakan PoC sebagai arah runtime dikoreksi**: status ADR 0003, blok `apps/`
  di `README.md`, dan paragraf PoC di [docs/deployment.md](../deployment.md).

## 5. Dampak ke repositori

- `docs/adr/0004-daytona.md` — dokumen ini.
- `docs/adr/0003-virtual-computer.md` — status menjadi *Digantikan oleh ADR 0004*; isi historisnya
  tidak dihapus.
- `README.md` — catatan bahwa `apps/virtual-computer/` adalah PoC yang sedang dipensiunkan.
- `docs/deployment.md` — catatan yang sama pada paragraf PoC, tanpa menghapus instruksinya.
- `apps/backend-js` — tempat `@daytona/sdk` dipakai (mulai #398). ADR ini tidak menambah kode.
- `apps/virtual-computer/` — dipensiunkan di #402.

## Keputusan yang tetap terbuka

- **Kelas resource per paket langganan.** Pemetaan paket → `daytona-small/medium/large` belum
  diputuskan; itu pekerjaan #398/#399, bukan ADR ini.
- **Penempatan snapshot kustom.** Karena snapshot Linux full terikat region + runner, region yang
  dipakai untuk snapshot kustom ditentukan saat image kustom pertama benar-benar dibutuhkan.
