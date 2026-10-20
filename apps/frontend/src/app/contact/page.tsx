import type { Metadata } from "next";
import { ContactForm } from "@/components/bolu/contact-form";
import { ContentPage } from "@/components/bolu/content-page";

export const metadata: Metadata = { title: "Kontak" };

export default function ContactPage() {
  return (
    <ContentPage title="Hubungi Bolu" intro="Punya pertanyaan tentang produk atau privasi? Gunakan formulir demo berikut. Saat ini formulir belum tersambung ke layanan pengiriman pesan.">
      <ContactForm />
    </ContentPage>
  );
}
