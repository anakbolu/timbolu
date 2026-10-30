import { ContentPage } from "@/components/bolu/content-page";
import { pageMetadata } from "@/lib/metadata";

export const metadata = pageMetadata({
  title: "Syarat dan Ketentuan",
  description:
    "Draf aturan penggunaan Timbolu. Dengan membuat akun atau menggunakan layanan, kamu menyetujui ketentuan yang berlaku setelah dokumen ini ditinjau dan diterbitkan secara resmi.",
  path: "/terms",
});

export default function TermsPage() {
  return (
    <ContentPage title="Syarat dan Ketentuan" intro="Draf aturan penggunaan Timbolu. Dengan membuat akun atau menggunakan layanan, kamu menyetujui ketentuan yang berlaku setelah dokumen ini ditinjau dan diterbitkan secara resmi.">
      <section><h2>1. Layanan</h2><p>Bolu membantu menyiapkan dan mengatur pekerjaan berdasarkan instruksi pengguna. Fitur, integrasi, paket, serta ketersediaan dapat berubah. Keterangan di situs tidak menjamin hasil tertentu atau ketersediaan tanpa gangguan.</p></section>
      <section><h2>2. Akun dan penggunaan yang wajar</h2><p>Kamu wajib memberikan informasi akun yang benar, menjaga keamanan akses akun, dan bertanggung jawab atas aktivitas yang dilakukan melalui akunmu. Jangan menggunakan layanan untuk melanggar hukum, mengganggu sistem, mengirim spam, atau melanggar hak orang lain.</p></section>
      <section><h2>3. Data, token, dan integrasi</h2><p>Kamu tetap bertanggung jawab atas data yang dimasukkan dan izin yang diberikan kepada integrasi. Jangan memberikan token API atau rahasia lebih luas dari yang diperlukan. Hindari memasukkan data sangat sensitif sebelum keamanan, pemrosesan, dan retensinya dijelaskan secara resmi. Pengguna harus memiliki hak untuk memproses dan membagikan data yang diberikan.</p></section>
      <section><h2>4. Tinjau pekerjaan sebelum digunakan</h2><p>Hasil otomatis dapat keliru, tidak lengkap, atau tidak sesuai konteks. Periksa draf, perhitungan, penerima, dan tindakan yang berdampak sebelum menyetujui atau menggunakannya. Kamu bertanggung jawab atas keputusan akhir dan izin tindakan pada akun yang terhubung.</p></section>
      <section><h2>5. Paket dan pembayaran</h2><p>Harga dan batas penggunaan ditampilkan pada halaman Harga. Ketentuan ini belum menetapkan mekanisme penagihan, pajak, pengembalian dana, atau pembatalan secara lengkap; rincian tersebut perlu diterbitkan sebelum pembayaran layanan diaktifkan.</p></section>
      <section><h2>6. Hak atas materi dan masukan</h2><p>Kamu mempertahankan hak atas materi yang kamu masukkan, sejauh kamu memilikinya. Kamu memberi izin terbatas kepada layanan untuk memproses materi itu guna menjalankan fitur yang kamu minta. Masukan sukarela dapat digunakan untuk memperbaiki layanan tanpa mengalihkan kepemilikan kontenmu.</p></section>
      <section><h2>7. Penangguhan dan perubahan</h2><p>Akses dapat dibatasi bila diperlukan untuk menjaga layanan atau menanggapi dugaan penyalahgunaan. Kami akan berupaya memberi pemberitahuan yang wajar bila memungkinkan. Perubahan ketentuan akan dicantumkan di halaman ini dengan tanggal pembaruan.</p></section>
      <section><h2>8. Ketentuan final</h2><p>Draf ini belum menetapkan badan hukum penyedia, alamat pemberitahuan, hukum yang berlaku, penyelesaian sengketa, batas tanggung jawab, maupun mekanisme pengakhiran. Bagian tersebut harus ditinjau dan dilengkapi oleh pemilik layanan sebelum dokumen menjadi kontrak final.</p></section>
    </ContentPage>
  );
}
