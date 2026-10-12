package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
)

// TestSensitiveRefusesTheFourPatterns is the E10.7 gate: a value that must not be
// remembered is recognised, and each one is named so the refusal can say why.
func TestSensitiveRefusesTheFourPatterns(t *testing.T) {
	cases := map[string]string{
		"kata sandi admin: rahasia123":             "kata sandi",
		"password = qwerty2026":                    "kata sandi",
		"PIN mesin EDC 4821":                       "kata sandi",
		"kode OTP saya 482913":                     "kode OTP",
		"Kode verifikasi: 9182. Jangan dibagikan.": "kode OTP",
		"NIK pelanggan 3273010101990001":           "nomor identitas atau rekening",
		"nomor rekening BCA 1234567890":            "nomor identitas atau rekening",
		"Coba tagih ke kartu 4111 1111 1111 1111":  "nomor kartu",
		"kartu 5500-0000-0000-0004 kedaluwarsa":    "nomor kartu",
		"CVV kartu itu 123":                        "nomor identitas atau rekening",
		"Ganti kata sandi tiap tiga bulan ya":      "kata sandi",
		"ongkir Bandung 4111 1111 1111 1111":       "nomor kartu",
		"Pelanggan minta nota ke NIK 351234567890": "nomor identitas atau rekening",
		"password situs itu sama dengan email":     "kata sandi",
	}

	for text, reason := range cases {
		t.Run(text, func(t *testing.T) {
			got, sensitive := Sensitive(text)
			require.True(t, sensitive, "%q must be recognised as sensitive", text)
			require.Contains(t, got, reason)
		})
	}
}

// TestSensitiveAllowsOrdinaryBusinessFacts is the half that keeps the filter
// usable: a filter that refuses prices, order numbers, and dates is a filter
// people turn off.
func TestSensitiveAllowsOrdinaryBusinessFacts(t *testing.T) {
	cases := []string{
		"Jam buka 08.00-21.00",
		"Ongkir Bandung 15 ribu",
		"Paket ukuran 40x40x40 tersedia",
		"Pesanan 1204 dikirim besok",
		"Invoice 2026-10-12 sudah dibayar",
		"Diskon 20% untuk pesanan di atas 500 ribu",
		"Alamat gudang Jalan Merdeka 122, Bandung",
		"Telepon toko 081234567890",
		"Resi pengiriman 0034567891234",
		"Rekening toko dipakai untuk transfer pembeli",
		"Jangan lupa konfirmasi pembayaran sebelum kirim",
		"Pembeli minta lewat WhatsApp saja",
		"Stok kunyit tinggal 3 karung",
		"Nomor pesanan berikutnya 000012",
		"Total tagihan tiga juta rupiah",
		"Tarif kurir naik jadi 20 ribu per kilogram",
		"Kode pos 40115",
		"Nomor meja 12",
		// A label with no value is a policy, not a secret: these three name the
		// thing the filter looks for and carry nothing to keep.
		"Data kartu kredit tidak kami simpan",
		"Jangan simpan nomor kartu pelanggan",
		"Kirim struk ke nomor rekening yang baru",
		"Nomor kartu hanya dicatat tiga angka terakhirnya",
		"Masukkan OTP 4 digit saat masuk",
	}

	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			reason, sensitive := Sensitive(text)
			require.False(t, sensitive, "%q must not be filtered (reported %q)", text, reason)
		})
	}
}

// TestLuhnIsWhatDecidesACard: a long digit run that fails the checksum is not a
// card, which is what keeps an invoice number out of the filter.
func TestLuhnIsWhatDecidesACard(t *testing.T) {
	// Sixteen digits that do not satisfy Luhn.
	_, sensitive := Sensitive("Kode internal 1234567890123456")
	require.False(t, sensitive)
}

// TestRememberIntentReadsTheMarkers is the E10.4 gate's first half: the words a
// person uses are what make a message a memory request, and the content is what
// is left after the marker.
func TestRememberIntentReadsTheMarkers(t *testing.T) {
	cases := map[string]string{
		"ingat ya, jam buka 08.00-21.00":        "jam buka 08.00-21.00",
		"Ingat ya ongkir Bandung 15 ribu":       "ongkir Bandung 15 ribu",
		"mulai sekarang tagih di akhir bulan":   "tagih di akhir bulan",
		"tolong ingat ya: pelanggan VIP gratis": "pelanggan VIP gratis",
		"jangan lupa ya, gudang buka Senin":     "gudang buka Senin",
		"catat: ongkir Surabaya 25 ribu":        "ongkir Surabaya 25 ribu",
		"ingat bahwa diskon hanya untuk member": "diskon hanya untuk member",
		"note: paket besar pakai kurir khusus":  "paket besar pakai kurir khusus",
		"perlu diingat, transfer hanya ke BCA":  "transfer hanya ke BCA",
	}

	for text, want := range cases {
		t.Run(text, func(t *testing.T) {
			content, ok := RememberIntent(text)
			require.True(t, ok)
			require.Equal(t, want, content)
		})
	}
}

// TestRememberIntentIgnoresOrdinaryMessages is the other half: a message that is
// not a request must not trigger a write, and must not be mistaken for one
// because it happens to contain the word.
func TestRememberIntentIgnoresOrdinaryMessages(t *testing.T) {
	cases := []string{
		"berapa ongkir ke Bandung?",
		"Aku ingat pesanan kemarin belum dikirim.",
		"tolong kirim ulang tagihannya",
		"oke",
		"makasih ya",
		// A marker with nothing after it asks for nothing in particular:
		// answering it with a confirmation of nothing would be worse than
		// letting the ordinary reply ask what to remember.
		"ingat ya",
		"catat:",
	}

	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			content, ok := RememberIntent(text)
			require.False(t, ok, "(%q) must not be read as a memory request; got %q", text, content)
		})
	}
}

// TestClassifyKind: the marker and the words decide what sort of memory it is.
func TestClassifyKind(t *testing.T) {
	cases := map[string]string{
		"mulai sekarang tagih di akhir bulan": domain.KindInstruction,
		"jangan kirim sebelum dibayar":        domain.KindInstruction,
		"pelanggan lebih suka dihubungi pagi": domain.KindPreference,
		"ongkir Bandung 15 ribu":              domain.KindFact,
	}

	for text, want := range cases {
		t.Run(text, func(t *testing.T) {
			require.Equal(t, want, ClassifyKind(text))
		})
	}
}

// TestClassifyLayer: talking about the Bolu itself is that Bolu's memory;
// anything else belongs to the workspace.
func TestClassifyLayer(t *testing.T) {
	cases := map[string]string{
		"kamu selalu tulis tanggal di tagihan": domain.LayerPrivate,
		"kamu jangan pakai emoji di email":     domain.LayerPrivate,
		"ongkir Bandung 15 ribu":               domain.LayerMain,
		"jam buka 08.00-21.00":                 domain.LayerMain,
	}

	for text, want := range cases {
		t.Run(text, func(t *testing.T) {
			require.Equal(t, want, ClassifyLayer(text))
		})
	}
}

// TestChunkTextSplitsOnParagraphs: the blank lines are the first boundary, and
// no piece exceeds the configured size.
func TestChunkTextSplitsOnParagraphsAndOverlaps(t *testing.T) {
	chunks := chunkText(
		"Jam buka 08.00-21.00.\n\nOngkir Bandung 15 ribu, Surabaya 25 ribu.\n\nGudang tutup hari Minggu.",
		60, 10,
	)
	require.Len(t, chunks, 3, "blank lines are the first boundary")
	for _, chunk := range chunks {
		require.LessOrEqual(t, len([]rune(chunk)), 60)
	}
}

// TestChunkTextNeverLosesText: every word of the document survives into a chunk,
// which is what makes the chunking safe to run on a customer's file.
func TestChunkTextNeverLosesText(t *testing.T) {
	document := strings.Repeat("Ongkir Bandung 15 ribu. ", 40)

	chunks := chunkText(document, 200, 20)
	require.NotEmpty(t, chunks)

	joined := strings.Join(chunks, " ")
	require.Contains(t, joined, "Ongkir Bandung 15 ribu")
	// The overlap duplicates text on purpose, so the total is longer than the
	// document rather than shorter.
	require.GreaterOrEqual(t, len(joined), len(document))
}

// TestChunkTextCutsAnUnbrokenParagraph: a paragraph nobody ended is still split,
// which is the case a paste of one line produces.
func TestChunkTextCutsAnUnbrokenParagraph(t *testing.T) {
	chunks := chunkText(strings.Repeat("a", 250), 100, 20)
	require.Greater(t, len(chunks), 1)
	for _, chunk := range chunks {
		require.LessOrEqual(t, len([]rune(chunk)), 100)
	}
}

// TestChunkTextIgnoresWhitespaceOnly: an upload of blank space has nothing to
// store.
func TestChunkTextIgnoresWhitespaceOnly(t *testing.T) {
	require.Empty(t, chunkText("  \n\n \t \n\n  ", 100, 10))
}
