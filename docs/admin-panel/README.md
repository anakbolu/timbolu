# Panel internal — before / after

Tangkapan layar obrolan sebelum dan sesudah tata letak panel internal
(`timbolu_admin_panel.html`): sidebar ringkas, obrolan di tengah, dan panel thread
kerja di kanan.

| Berkas | Layar |
| --- | --- |
| `before-desktop.png` | Obrolan lama: sidebar 248px, kolom Riwayat Obrolan, gelembung gelap |
| `before-mobile.png` | Obrolan lama di 390px |
| `after-desktop.png` | Obrolan baru di 1440px: sidebar, obrolan, dan panel thread yang terbuka |
| `after-mobile.png` | Obrolan baru di 390px |
| `after-mobile-thread.png` | Panel thread sebagai laci di 390px |

**Cara diambil.** Berbeda dengan galeri `docs/dashboard`, gambar ini *tidak* diambil
dari stack `docker compose`: lingkungan pengambilannya tidak punya Docker. Gambar
diambil dari `next dev` dengan Playwright, dan setiap panggilan ke API dijawab oleh
data tiruan di skrip pengambilannya (Bolu, obrolan, satu thread, tiga lampiran). Jadi
tampilannya nyata, tetapi isinya bukan workspace sungguhan, dan status koneksi di
header terbaca "menyambung ulang…" karena stream realtime sengaja tidak dijawab.
Diambil 2026-10-24.
