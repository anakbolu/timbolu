/**
 * Keluarga Bolu crew data — ported 1:1 from design/refs/data.js.
 * Shapes, mouths, feet, colours and copy must stay identical to the design.
 */

export type ShapeName =
  | "kacang"
  | "hantu"
  | "gumpal"
  | "awan"
  | "tetes"
  | "mochi"
  | "bulat"
  | "kotak"
  | "daun";
export type MouthName = "open" | "smile" | "focus" | "w" | "o" | "sleep" | "chill";

type ShapeDef = {
  /** Body outline path, in the 200×200 viewBox. */
  d: string;
  /** Body outline stroke width. */
  sw: number;
  /** Transform applied to the face group. */
  face: string;
  /** Baseline used to place the feet; `0` means the shape has no feet. */
  foot: number;
};

type MouthDef = {
  d: string;
  fill: string;
  sw: number;
};

export const SHAPES: Record<ShapeName, ShapeDef> = {
  kacang: {
    d: "M60 50 C100 30 160 40 165 90 C170 140 140 170 95 168 C50 166 28 140 32 100 C34 75 40 60 60 50 Z",
    sw: 4,
    face: "translate(100 107) scale(1.1)",
    foot: 168,
  },
  hantu: {
    d: "M90 30 C130 26 150 60 146 100 C143 126 154 142 162 156 Q172 174 152 174 C130 174 112 168 100 160 C70 168 52 140 52 104 C52 60 60 34 90 30 Z",
    sw: 6,
    face: "translate(98 89) scale(1)",
    foot: 0,
  },
  gumpal: {
    d: "M50 52 C80 36 130 38 158 52 C176 70 172 130 160 152 C140 172 70 174 46 154 C30 130 30 72 50 52 Z",
    sw: 4,
    face: "translate(100 103) scale(1.15)",
    foot: 166,
  },
  awan: {
    d: "M50 162 Q20 162 22 132 Q24 106 50 102 Q50 64 88 60 Q112 36 140 58 Q176 60 172 100 Q190 114 182 140 Q176 164 150 162 Z",
    sw: 4,
    face: "translate(102 119) scale(1.05)",
    foot: 162,
  },
  tetes: {
    d: "M88 42 Q100 22 112 42 Q150 92 160 120 Q166 172 100 172 Q34 172 40 120 Q50 92 88 42 Z",
    sw: 6,
    face: "translate(100 129) scale(1.05)",
    foot: 172,
  },
  mochi: {
    d: "M28 142 Q24 72 100 68 Q176 72 172 142 Q172 172 100 172 Q28 172 28 142 Z",
    sw: 4,
    face: "translate(100 127) scale(1.1)",
    foot: 172,
  },
  bulat: {
    d: "M100 38 C138 38 168 68 168 106 C168 144 138 174 100 174 C62 174 32 144 32 106 C32 68 62 38 100 38 Z",
    sw: 4,
    face: "translate(100 108) scale(1.15)",
    foot: 174,
  },
  kotak: {
    d: "M64 40 L136 40 Q166 40 166 70 L166 142 Q166 172 136 172 L64 172 Q34 172 34 142 L34 70 Q34 40 64 40 Z",
    sw: 4,
    face: "translate(100 109) scale(1.2)",
    foot: 172,
  },
  daun: {
    d: "M40 60 C70 46 150 50 166 66 C176 80 150 140 110 158 C80 170 46 160 38 130 C30 100 26 70 40 60 Z",
    sw: 4,
    face: "translate(98 101) scale(1.05)",
    foot: 0,
  },
};

export const MOUTHS: Record<MouthName, MouthDef> = {
  open: { d: "M-12 9 Q0 11 12 9 Q10 26 0 26 Q-10 26 -12 9 Z", fill: "#1E1B2E", sw: 0 },
  smile: { d: "M-9 12 Q0 20 9 12", fill: "none", sw: 4.5 },
  focus: { d: "M-7 16 Q0 13 7 16", fill: "none", sw: 4.5 },
  w: { d: "M-9 13 Q-4.5 19 0 13 Q4.5 19 9 13", fill: "none", sw: 4 },
  o: { d: "M-5 18 a5 6 0 1 0 10 0 a5 6 0 1 0 -10 0 Z", fill: "#1E1B2E", sw: 0 },
  sleep: { d: "M-6 17 L6 17", fill: "none", sw: 4 },
  chill: { d: "M-8 14 Q0 20 8 14", fill: "none", sw: 4 },
};

export type BotShape = {
  /** Body fill, and stroke on the outline. */
  color: string;
  /** Foot fill. */
  feetColor: string;
  /** Body outline path. */
  d: string;
  /** Body outline stroke width. */
  sw: number;
  /** Face group transform. */
  face: string;
  hasFeet: boolean;
  /** Two flat ellipse feet, positioned from the shape baseline. */
  feet: string;
  mouth: string;
  mouthFill: string;
  mouthSw: number;
  /** Pupil offset transform. */
  look: string;
  /** Animation delay, shared by bob/blink/glance. */
  delay: string;
  /** Draw the paused face: closed eyes instead of the blinking ones. */
  eyesClosed?: boolean;
  /** Draw the relaxed face: half-lidded eyes, used while a bot is chilling. */
  eyesLazy?: boolean;
  /** Draw the coffee cup + humming note that go with the chill face. */
  chillFx?: boolean;
};

export type CrewMember = {
  name: string;
  role: string;
  /** Body shape; every name here also exists in `BOLU_SHAPES`. */
  shape: ShapeName;
  /** Card background tint. */
  tint: string;
  bio: string;
  /** Rotating "currently working on" lines. */
  acts: string[];
} & BotShape;

type BotSeed = {
  name: string;
  role: string;
  shape: ShapeName;
  color: string;
  feetColor: string;
  tint: string;
  mouth: MouthName;
  look: string;
  delay: string;
  bio: string;
  acts: string[];
};

/** Two flat ellipse feet, placed just under the shape's baseline. */
function feetFor(shape: ShapeDef): string {
  const y = shape.foot - 3;
  return `M70 ${y} a12 8 0 1 0 24 0 a12 8 0 1 0 -24 0 Z M106 ${y} a12 8 0 1 0 24 0 a12 8 0 1 0 -24 0 Z`;
}

function mk(seed: BotSeed): CrewMember {
  const shape = SHAPES[seed.shape];
  const mouth = MOUTHS[seed.mouth];
  return {
    name: seed.name,
    role: seed.role,
    shape: seed.shape,
    color: seed.color,
    feetColor: seed.feetColor,
    tint: seed.tint,
    bio: seed.bio,
    acts: seed.acts,
    d: shape.d,
    sw: shape.sw,
    face: shape.face,
    hasFeet: shape.foot > 0,
    feet: feetFor(shape),
    mouth: mouth.d,
    mouthFill: mouth.fill,
    mouthSw: mouth.sw,
    look: seed.look,
    delay: seed.delay,
  };
}

export type FaceVariant = {
  /** Mouth expression; keeps the member's own mouth when omitted. */
  mouth?: MouthName;
  /** Pupil offset transform; keeps the member's own when omitted. */
  look?: string;
  /** Draw the paused (closed-eye) face. */
  closed?: boolean;
  /** Draw the relaxed (half-lidded) face. Ignored when `closed` is set. */
  lazy?: boolean;
  /** Draw the coffee cup + humming note beside the face. */
  fx?: boolean;
};

/**
 * Rebuild a crew member's face with a different mouth, pupil offset or closed
 * eyes — the design's `mk()` variant helper, used for paused and typing faces.
 * Everything else (shape, colours, feet, delay) is kept as-is.
 */
export function withFace(member: BotShape, variant: FaceVariant = {}): BotShape {
  const mouth = variant.mouth ? MOUTHS[variant.mouth] : null;
  const closed = variant.closed ?? false;
  return {
    ...member,
    mouth: mouth ? mouth.d : member.mouth,
    mouthFill: mouth ? mouth.fill : member.mouthFill,
    mouthSw: mouth ? mouth.sw : member.mouthSw,
    look: variant.look ?? member.look,
    eyesClosed: closed,
    eyesLazy: !!variant.lazy && !closed,
    chillFx: variant.fx ?? false,
  };
}

const CREW_SEEDS = [
  {
    name: "Oren",
    role: "Tukang invoice",
    shape: "kacang",
    color: "#F28C4E",
    feetColor: "#D9692A",
    tint: "#FDE7DA",
    mouth: "focus",
    look: "translate(2 3)",
    delay: "0s",
    bio: "Bikin invoice dari data pesanan, kirim ke pelanggan, lalu menagih dengan sopan kalau sudah lewat jatuh tempo.",
    acts: [
      "Membuat invoice INV-0042",
      "Menghitung diskon dan pajak",
      "Mengirim invoice ke pelanggan",
    ],
  },
  {
    name: "Biru",
    role: "Penjaga WhatsApp",
    shape: "hantu",
    color: "#2E9BEF",
    feetColor: "#1B7AC7",
    tint: "#DCEEFD",
    mouth: "open",
    look: "translate(-2 2)",
    delay: ".4s",
    bio: "Membalas pertanyaan yang itu-itu saja: ongkir, status pesanan, stok, jam buka, dan nomor rekening.",
    acts: [
      "Membalas “Kak, ongkir ke Bandung?”",
      "Mengirim nomor resi ke pelanggan",
      "Membalas “Masih ready, Kak?”",
    ],
  },
  {
    name: "Lila",
    role: "Pengatur jadwal",
    shape: "gumpal",
    color: "#A77DC9",
    feetColor: "#8459AB",
    tint: "#EEE5F7",
    mouth: "smile",
    look: "translate(3 -2)",
    delay: ".8s",
    bio: "Mencari slot kosong, mengirim undangan rapat, dan mengingatkan tenggat sehari sebelumnya.",
    acts: [
      "Menjadwalkan rapat Kamis 10.00",
      "Mengirim undangan kalender",
      "Mengingatkan tenggat pembayaran",
    ],
  },
  {
    name: "Ijo",
    role: "Juru rekap",
    shape: "mochi",
    color: "#4DBB72",
    feetColor: "#34985A",
    tint: "#DFF3E6",
    mouth: "focus",
    look: "translate(0 3)",
    delay: "1.2s",
    bio: "Memindahkan data pesanan ke spreadsheet, memperbarui stok, dan menyusun rekap mingguan.",
    acts: ["Mencatat pesanan baru", "Memperbarui sisa stok", "Menyusun rekap mingguan"],
  },
  {
    name: "Pinky",
    role: "Pengarsip",
    shape: "awan",
    color: "#F59ABF",
    feetColor: "#E07199",
    tint: "#FDE6EF",
    mouth: "w",
    look: "translate(-3 1)",
    delay: "1.6s",
    bio: "Merapikan nama file, memilah dokumen, dan menyimpan bukti transfer ke folder yang benar.",
    acts: [
      "Mengganti nama scan_0193.pdf",
      "Memindahkan kuitansi ke folder Oktober",
      "Menyimpan bukti transfer",
    ],
  },
  {
    name: "Kunyit",
    role: "Pembalas email",
    shape: "tetes",
    color: "#FFC21A",
    feetColor: "#DDA000",
    tint: "#FFF3CC",
    mouth: "smile",
    look: "translate(2 1)",
    delay: "2s",
    bio: "Memilah inbox, menandai email penting, dan menyiapkan draf balasan untuk kamu periksa.",
    acts: ["Memilah email masuk", "Menandai email penting", "Menyiapkan draf balasan"],
  },
] satisfies BotSeed[];

export const CREW: CrewMember[] = CREW_SEEDS.map(mk);

const HORE_SEEDS = [
  {
    name: "Cerah",
    role: "Pembuat konten",
    shape: "kotak",
    color: "#F2645A",
    feetColor: "#D1463D",
    tint: "#FDE1DF",
    mouth: "smile",
    look: "translate(2 -2)",
    delay: ".3s",
    bio: "Menulis caption, menyusun kalender konten, dan menyiapkan jadwal posting media sosial.",
    acts: ["Menulis caption promo", "Menyusun kalender konten", "Menjadwalkan posting"],
  },
  {
    name: "Gembul",
    role: "Pengelola iklan",
    shape: "bulat",
    color: "#1BB4C8",
    feetColor: "#118A9A",
    tint: "#D6F3F7",
    mouth: "focus",
    look: "translate(-2 2)",
    delay: ".9s",
    bio: "Memantau iklan yang berjalan, mencatat hasilnya, dan memberi saran kapan perlu diubah.",
    acts: ["Memantau iklan promo", "Mencatat hasil iklan", "Membandingkan dua versi iklan"],
  },
  {
    name: "Riang",
    role: "Pembalas komentar",
    shape: "daun",
    color: "#9CC827",
    feetColor: "#7DA215",
    tint: "#EEF6D5",
    mouth: "open",
    look: "translate(3 1)",
    delay: "1.5s",
    bio: "Membalas komentar dan DM media sosial, dan menahan keluhan supaya kamu bisa membacanya dulu.",
    acts: ["Membalas komentar Instagram", "Menandai keluhan", "Membalas DM"],
  },
] satisfies BotSeed[];

/** The "Tim Hore" crew — the dashboard's Tim lain section shows only these. */
export const HORE: CrewMember[] = HORE_SEEDS.map(mk);

/** Every bot the dashboard knows about: Tim Bolu plus Tim Hore. */
export const ALL_BOTS: CrewMember[] = [...CREW, ...HORE];

/** The logo reuses Oren's body with the "open" mouth. */
export const LOGO = mk({
  name: "bolu",
  role: "",
  shape: "kacang",
  color: "#F28C4E",
  feetColor: "#D9692A",
  tint: "#FDE7DA",
  mouth: "open",
  look: "translate(0 0)",
  delay: "0s",
  bio: "",
  acts: [],
});

/** Crew lookup that throws on typos instead of silently rendering nothing. */
export function bot(name: string): CrewMember {
  const found = CREW.find((member) => member.name === name);
  if (!found) throw new Error(`Unknown crew member: ${name}`);
  return found;
}

/** Dashboard-wide bot lookup covering both teams. */
export function allBot(name: string): CrewMember {
  const found = ALL_BOTS.find((member) => member.name === name);
  if (!found) throw new Error(`Unknown crew member: ${name}`);
  return found;
}

export const FEED = [
  "Oren mengirim INV-0041",
  "Biru membalas 8 chat",
  "Pinky merapikan 14 file",
  "Lila memasang pengingat rapat",
  "Ijo memperbarui rekap",
  "Kunyit menyiapkan 3 draf balasan",
];

/** How long each act stays on screen before the crew rotates. */
export const ACT_INTERVAL_MS = 2400;

/** Feed ages shown next to the three most recent completions. */
export const FEED_AGES = ["baru saja", "1 menit lalu", "3 menit lalu"];

export type TabId = "inv" | "wa" | "rekap" | "arsip";

export type TabStep = {
  who: string;
  text: string;
};

export type Tab = {
  id: TabId;
  label: string;
  /** Panel background tint. */
  tint: string;
  steps: TabStep[];
};

function steps(rows: [string, string][]): TabStep[] {
  return rows.map(([who, text]) => ({ who, text }));
}

export const TABS: Tab[] = [
  {
    id: "inv",
    label: "Bikin invoice",
    tint: "#FDE7DA",
    steps: steps([
      ["Ijo", "Mengambil data pesanan dari rekap"],
      ["Oren", "Mengisi invoice dan menghitung total"],
      ["Lila", "Memasang pengingat jatuh tempo"],
      ["Kunyit", "Menyiapkan email pengantar invoice"],
    ]),
  },
  {
    id: "wa",
    label: "Balas WhatsApp",
    tint: "#DCEEFD",
    steps: steps([
      ["Biru", "Membaca pertanyaan dan menyiapkan balasan"],
      ["Ijo", "Mengecek status pesanan dan nomor resi"],
      ["Biru", "Mengirim balasan sesuai gaya bahasamu"],
    ]),
  },
  {
    id: "rekap",
    label: "Rekap pesanan",
    tint: "#DFF3E6",
    steps: steps([
      ["Biru", "Meneruskan pesanan dari chat"],
      ["Ijo", "Mencatat ke spreadsheet dan menandai yang belum lunas"],
      ["Oren", "Menagih pesanan yang belum dibayar"],
    ]),
  },
  {
    id: "arsip",
    label: "Rapikan file",
    tint: "#FDE6EF",
    steps: steps([
      ["Pinky", "Membaca isi dokumen yang baru masuk"],
      ["Pinky", "Memberi nama yang jelas dan memindahkan ke folder"],
      ["Ijo", "Menautkan bukti bayar ke baris rekap"],
    ]),
  },
];

export type FlowStep = {
  n: number;
  title: string;
  text: string;
  /** Present when a bot handles the step; absent for the "Kamu" step. */
  bot?: CrewMember;
};

export const FLOW: FlowStep[] = [
  {
    n: 1,
    bot: bot("Biru"),
    title: "Pesanan masuk",
    text: "Biru membalas pelanggan di WhatsApp dan mengonfirmasi pesanan.",
  },
  {
    n: 2,
    bot: bot("Ijo"),
    title: "Dicatat",
    text: "Ijo memasukkan pesanan ke spreadsheet dan mengurangi stok.",
  },
  {
    n: 3,
    bot: bot("Oren"),
    title: "Invoice dibuat",
    text: "Oren menyusun invoice dari data yang dicatat Ijo.",
  },
  { n: 4, title: "Kamu setujui", text: "Satu ketukan untuk memeriksa dan mengirim." },
  {
    n: 5,
    bot: bot("Lila"),
    title: "Diingatkan",
    text: "Lila memasang pengingat sebelum jatuh tempo.",
  },
  {
    n: 6,
    bot: bot("Pinky"),
    title: "Diarsipkan",
    text: "Pinky menyimpan invoice dan bukti bayar ke foldernya.",
  },
];

export type RekapRow = {
  tgl: string;
  nama: string;
  barang: string;
  total: string;
  status: "Lunas" | "Belum";
};

export const REKAP_ROWS: RekapRow[] = [
  { tgl: "6 Okt", nama: "Rina", barang: "Kaos hitam L ×2", total: "Rp 170.000", status: "Lunas" },
  { tgl: "6 Okt", nama: "Dimas", barang: "Hoodie abu M", total: "Rp 245.000", status: "Belum" },
  {
    tgl: "7 Okt",
    nama: "Toko Sari",
    barang: "Kaos polos ×10",
    total: "Rp 1.124.000",
    status: "Lunas",
  },
  { tgl: "7 Okt", nama: "Ayu", barang: "Totebag kanvas", total: "Rp 65.000", status: "Lunas" },
  { tgl: "7 Okt", nama: "Bagas", barang: "Kemeja flanel L", total: "Rp 189.000", status: "Belum" },
];

export type InvoiceLine = {
  barang: string;
  qty: number;
  jumlah: string;
};

export const INVOICE_LINES: InvoiceLine[] = [
  { barang: "Kaos polos hitam L", qty: 10, jumlah: "Rp 850.000" },
  { barang: "Sablon 1 warna", qty: 10, jumlah: "Rp 250.000" },
  { barang: "Ongkos kirim", qty: 1, jumlah: "Rp 24.000" },
];

export type ArsipFile = {
  from: string;
  to: string;
};

export const ARSIP_FILES: ArsipFile[] = [
  { from: "scan_0193.pdf", to: "2026-10-07_Invoice_INV-0042.pdf" },
  { from: "IMG_20261006_1422.jpg", to: "2026-10-06_BuktiTransfer_Rina.jpg" },
  { from: "Document (3).pdf", to: "2026-10-05_Kontrak_TokoSari.pdf" },
  { from: "WhatsApp Image 2026-10-07.jpeg", to: "2026-10-07_BuktiTransfer_Ayu.jpeg" },
];

export const FAQ: { q: string; a: string }[] = [
  {
    q: "Apakah Bolu mengirim pesan tanpa izin saya?",
    a: "Secara bawaan tidak. Semua balasan dan invoice menunggu persetujuanmu dulu. Kamu bisa membuka izin kirim otomatis untuk jenis pesan tertentu, misalnya info jam buka.",
  },
  {
    q: "Aplikasi apa saja yang bisa disambungkan?",
    a: "[DAFTAR INTEGRASI, misalnya WhatsApp Business, Gmail, Google Sheets, Google Drive]",
  },
  {
    q: "Bisa pakai format invoice saya sendiri?",
    a: "Bisa. Unggah contoh invoice yang biasa kamu pakai, dan Oren akan mengikuti susunan, logo, serta nomor urutnya.",
  },
  { q: "Berapa biayanya?", a: "[INFO HARGA]" },
];

export const FEATURES: { title: string; text: string; icon: "check" | "clock" | "log" }[] = [
  {
    icon: "check",
    title: "Setujui sebelum terkirim",
    text: "Invoice, balasan, dan email masuk sebagai draf. Kamu bisa ubah, setujui, atau biarkan Bolu jalan sendiri untuk hal-hal yang sudah kamu percayakan.",
  },
  {
    icon: "clock",
    title: "Atur jam kerja mereka",
    text: "Tentukan kapan Biru boleh membalas chat dan kapan Oren boleh menagih, supaya pelanggan tidak dihubungi tengah malam.",
  },
  {
    icon: "log",
    title: "Semua tercatat",
    text: "Setiap langkah punya catatan: siapa yang mengerjakan, kapan, dan datanya dari mana. Gampang dicek kalau ada yang janggal.",
  },
];
