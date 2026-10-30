import { ContactForm } from "@/components/bolu/contact-form";
import { ContentPage } from "@/components/bolu/content-page";
import { pageMetadata } from "@/lib/metadata";

export const metadata = pageMetadata({
  title: "Kontak",
  description:
    "Punya pertanyaan tentang produk atau privasi? Gunakan formulir demo berikut.",
  path: "/contact",
});

export default function ContactPage() {
  return (
    <ContentPage title="Hubungi Bolu" intro="Punya pertanyaan tentang produk atau privasi? Gunakan formulir demo berikut. Saat ini formulir belum tersambung ke layanan pengiriman pesan.">
      <ContactForm />
    </ContentPage>
  );
}
