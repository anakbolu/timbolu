"use client";

import { useState, type FormEvent } from "react";

export function ContactForm() {
  const [submitted, setSubmitted] = useState(false);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitted(true);
  }

  return (
    <section className="rounded-3xl border border-bolu-border bg-bolu-surface p-6 shadow-sm sm:p-8">
      <p className="mb-5 text-sm text-bolu-muted">Demo lokal: isi formulir tidak dikirim atau disimpan.</p>
      <form className="space-y-5" onSubmit={handleSubmit}>
        <div className="space-y-2"><label className="font-semibold" htmlFor="contact-name">Nama</label><input className="min-h-12 w-full rounded-xl border border-bolu-border bg-bolu-bg px-4" id="contact-name" name="name" autoComplete="name" required maxLength={100} /></div>
        <div className="space-y-2"><label className="font-semibold" htmlFor="contact-email">Email</label><input className="min-h-12 w-full rounded-xl border border-bolu-border bg-bolu-bg px-4" id="contact-email" name="email" type="email" autoComplete="email" required maxLength={254} /></div>
        <div className="space-y-2"><label className="font-semibold" htmlFor="contact-topic">Topik</label><select className="min-h-12 w-full rounded-xl border border-bolu-border bg-bolu-bg px-4" id="contact-topic" name="topic" defaultValue="umum"><option value="umum">Pertanyaan umum</option><option value="privasi">Privasi dan data</option><option value="produk">Produk dan akun</option><option value="lainnya">Lainnya</option></select></div>
        <div className="space-y-2"><label className="font-semibold" htmlFor="contact-message">Pesan</label><textarea className="min-h-36 w-full rounded-xl border border-bolu-border bg-bolu-bg px-4 py-3" id="contact-message" name="message" required minLength={10} maxLength={2000} aria-describedby="contact-help" /><p className="text-sm text-bolu-muted" id="contact-help">Jangan sertakan token API, kata sandi, atau informasi rahasia.</p></div>
        <button className="min-h-12 rounded-full bg-bolu-solid px-6 font-semibold text-bolu-solid-ink" type="submit">Kirim demo</button>
      </form>
      {submitted && <p className="mt-5 rounded-xl bg-bolu-panel p-4" role="status">Demo berhasil dicoba. Pesan ini hanya ditampilkan di halaman ini dan belum dikirim ke tim Bolu.</p>}
    </section>
  );
}
