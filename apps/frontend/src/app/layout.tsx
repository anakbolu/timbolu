import type { Metadata, Viewport } from "next";
import { Figtree, Fredoka } from "next/font/google";
import { THEME_INIT_SCRIPT } from "@/lib/theme";
import "./globals.css";

const figtree = Figtree({
  variable: "--font-figtree",
  subsets: ["latin"],
  display: "swap",
});

const fredoka = Fredoka({
  variable: "--font-fredoka",
  subsets: ["latin"],
  display: "swap",
});

export const metadata: Metadata = {
  title: {
    default: "Timbolu — asisten kerja kantor",
    template: "%s · Timbolu",
  },
  description:
    "Enam asisten kecil yang kerja bareng untukmu: bikin invoice, balas WhatsApp, rekap pesanan, sampai merapikan file. Mereka siapkan semuanya, kamu tinggal cek dan setujui.",
  openGraph: {
    title: "Timbolu — asisten kerja kantor",
    description:
      "Enam asisten kecil yang kerja bareng untukmu: bikin invoice, balas WhatsApp, rekap pesanan, sampai merapikan file.",
    siteName: "Timbolu",
    locale: "id_ID",
    type: "website",
  },
};

export const viewport: Viewport = {
  // `theme-color` is deliberately absent: the inline script in the layout owns
  // that tag, because a declared one is re-rendered by React on every navigation
  // and would land after the script had already decided. `color-scheme` is safe
  // to declare — it says the document supports both, and never disagrees with the
  // theme — and it is what tells the browser to paint a dark scrollbar, caret and
  // autofill before any stylesheet has been parsed.
  colorScheme: "light dark",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html
      lang="id"
      className={`${figtree.variable} ${fredoka.variable} h-full antialiased`}
      // The inline script below writes the theme class onto this element before
      // React hydrates, so the markup React compares against has already changed.
      suppressHydrationWarning
    >
      <head>
        {/* Before the first paint: `localStorage` is not readable during
            server rendering, and a theme applied in an effect would paint the
            wrong one first. */}
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body className="flex min-h-full flex-col bg-bolu-bg font-sans text-bolu-ink">
        {children}
      </body>
    </html>
  );
}
