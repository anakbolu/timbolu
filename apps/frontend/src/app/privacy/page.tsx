import type { Metadata } from "next";
import { ContentPage } from "@/components/bolu/content-page";

export const metadata: Metadata = { title: "Kebijakan Privasi" };

export default function PrivacyPage() {
  return (
    <ContentPage title="Kebijakan Privasi" intro="Draf kebijakan Keluarga Bolu tentang informasi yang kamu berikan dan cara menggunakannya. Terakhir diperbarui: 20 Oktober 2026.">
      <section><h2>1. Informasi yang dapat diproses</h2><p>Saat kamu menggunakan Bolu, kamu dapat memberikan informasi akun dan ruang kerja, instruksi tugas, isi percakapan, serta berkas atau data yang diperlukan untuk pekerjaan. Integrasi yang kamu hubungkan dapat memberikan data sesuai izin yang kamu pilih.</p></section>
      <section><h2>2. Token API dan rahasia</h2><p>Token API, kata sandi, kunci, kode akses, dan rahasia lain dapat memberi akses ke akun atau sistemmu. Perlakukan semuanya seperti kunci rumah: jangan menaruhnya di instruksi, chat, lampiran, tangkapan layar, atau formulir yang tidak secara khusus meminta kredensial.</p><ul><li>Berikan hanya akses minimum yang diperlukan dan cabut token yang tidak lagi digunakan.</li><li>Jangan kirim token API melalui halaman Kontak. Formulir demo di situs ini tidak meminta atau mengirim kredensial.</li><li>Jangan menganggap token, isi tugas, atau berkas telah dienkripsi, disamarkan, dihapus otomatis, atau dilindungi sertifikasi tertentu. Kemampuan itu belum dinyatakan di halaman ini.</li></ul><p>Kami berkomitmen membatasi penggunaan data pada penyediaan dan peningkatan layanan, serta tidak meminta rahasia yang tidak diperlukan. Rincian pengamanan teknis, lokasi pemrosesan, masa simpan, dan subprosesor perlu dikonfirmasi sebelum layanan dipakai dengan data sensitif.</p></section>
      <section><h2>3. Tujuan penggunaan</h2><p>Informasi digunakan untuk menjalankan fitur yang kamu minta, mengelola akun dan ruang kerja, menjaga keandalan layanan, serta menanggapi pertanyaan. Jika pemrosesan melibatkan penyedia model atau integrasi eksternal, data yang relevan dapat diteruskan agar fitur tersebut bekerja; konfigurasi dan daftar penyedia yang berlaku harus dijelaskan sebelum peluncuran produksi.</p></section>
      <section><h2>4. Kendali dan tanggung jawab</h2><p>Kamu bertanggung jawab memastikan bahwa kamu berhak memasukkan data yang diberikan kepada Bolu, memilih akses integrasi secara hati-hati, meninjau draf sebelum mengirimkannya, dan menghapus atau mencabut akses yang tidak lagi diperlukan melalui fitur yang tersedia.</p></section>
      <section><h2>5. Permintaan dan perubahan</h2><p>Untuk pertanyaan privasi atau permintaan terkait data, hubungi kami melalui halaman Kontak. Halaman ini adalah draf produk; alamat pemilik layanan, prosedur permintaan, jangka waktu tanggapan, dan jadwal retensi perlu dilengkapi sebelum kebijakan ini dipakai sebagai pemberitahuan final.</p></section>
    </ContentPage>
  );
}
