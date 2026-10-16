package attachtext

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// pngBytes is a picture as the package sees one: the magic number is all the
// sniffing needs, and a real image would only make the fixture bigger.
func pngBytes(size int) []byte {
	content := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, size)...)
	return content
}

// pdfBytes builds a one-page PDF whose text layer is the given string.
//
// It is generated rather than checked in so the fixture and the assertion cannot
// drift apart, and because a PDF is the one input where "the extractor read it"
// is worth proving against a real parser rather than a stub.
func pdfBytes(text string) []byte {
	content := fmt.Sprintf("BT /F1 12 Tf 20 100 Td (%s) Tj ET", text)

	var buf bytes.Buffer
	offsets := make([]int, 6)
	object := func(number int, body string) {
		offsets[number] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	buf.WriteString("%PDF-1.4\n")
	object(1, "<< /Type /Catalog /Pages 2 0 R >>")
	object(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	object(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents 4 0 R "+
		"/Resources << /Font << /F1 5 0 R >> >> >>")
	object(4, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	object(5, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xref := buf.Len()
	buf.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)

	return buf.Bytes()
}

func TestRenderTextFile(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename:    "stok.txt",
		ContentType: "text/plain",
		ByteSize:    22,
		Content:     []byte("Stok kaos merah: 7 pcs"),
	})

	require.Equal(t, "[Lampiran: stok.txt (text/plain, 22 B)]\nStok kaos merah: 7 pcs\n[/Lampiran]", part.Text)
	require.Nil(t, part.Image)
	require.Empty(t, part.NoVision)
}

func TestRenderTextFileByExtensionWhenTheTypeSaysNothing(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename:    "laporan.csv",
		ContentType: "application/octet-stream",
		ByteSize:    12,
		Content:     []byte("a,b\n1,2\n3,4"),
	})

	require.Contains(t, part.Text, "a,b\n1,2\n3,4")
	require.Contains(t, part.Text, "laporan.csv")
}

func TestRenderTextFileReplacesInvalidUTF8(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "catatan.log", ContentType: "text/plain",
		ByteSize: 6, Content: []byte{'a', 0xff, 'b'},
	})

	require.Contains(t, part.Text, "a\uFFFDb")
}

func TestRenderTruncatesOneFile(t *testing.T) {
	renderer := NewRenderer(Limits{MaxCharsPerFile: 5})

	part := renderer.Render(File{
		Filename: "panjang.txt", ContentType: "text/plain",
		ByteSize: 26, Content: []byte("abcdefghijklmnopqrstuvwxyz"),
	})

	require.Contains(t, part.Text, "[Lampiran: panjang.txt (text/plain, 26 B)]\nabcde\n[... terpotong: 5 dari 26 karakter ditampilkan]\n[/Lampiran]")
}

func TestRenderPDF(t *testing.T) {
	content := pdfBytes("Stok kaos merah 7 pcs")
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "laporan.pdf", ContentType: "application/pdf",
		ByteSize: int64(len(content)), Content: content,
	})

	require.Contains(t, part.Text, "Stok kaos merah 7 pcs")
	require.Contains(t, part.Text, "[Lampiran: laporan.pdf (application/pdf,")
}

func TestRenderCorruptPDFBecomesANote(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "rusak.pdf", ContentType: "application/pdf",
		ByteSize: 16, Content: []byte("%PDF-1.4\ngarbage"),
	})

	require.Contains(t, part.Text, "[Lampiran rusak.pdf tidak bisa dibaca:")
	require.Nil(t, part.Image)
}

func TestRenderPDFWithoutTextSaysSo(t *testing.T) {
	content := pdfBytes("")
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "scan.pdf", ContentType: "application/pdf",
		ByteSize: int64(len(content)), Content: content,
	})

	require.Contains(t, part.Text, "PDF tanpa teks yang bisa diekstrak")
}

func TestRenderImage(t *testing.T) {
	content := pngBytes(16)
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "foto.png", ContentType: "image/png",
		ByteSize: int64(len(content)), Content: content,
	})

	require.NotNil(t, part.Image)
	require.Equal(t, "image/png", part.Image.MediaType)
	require.Equal(t, base64.StdEncoding.EncodeToString(content), part.Image.Data)
	require.Equal(t, "[Gambar foto.png (image/png, 24 B)]", part.Text)
	require.Contains(t, part.NoVision, "model yang aktif tidak mendukung gambar")
}

// A picture whose stored type lies is still a picture: the bytes decide.
func TestRenderImageIsSniffedNotTrusted(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "foto.txt", ContentType: "text/plain",
		ByteSize: 24, Content: pngBytes(16),
	})

	require.NotNil(t, part.Image)
	require.Equal(t, "image/png", part.Image.MediaType)
}

func TestRenderOversizedImageBecomesANote(t *testing.T) {
	renderer := NewRenderer(Limits{MaxImageBytes: 8})

	part := renderer.Render(File{
		Filename: "besar.png", ContentType: "image/png",
		ByteSize: 1024, Content: pngBytes(1024),
	})

	require.Nil(t, part.Image)
	require.Contains(t, part.Text, "[Lampiran besar.png (image/png, 1.0 KB): isi tidak dibaca — gambar lebih besar dari batas 8 B]")
}

func TestRenderCapsTheNumberOfImages(t *testing.T) {
	renderer := NewRenderer(Limits{MaxImages: 1})

	first := renderer.Render(File{Filename: "a.png", ContentType: "image/png", ByteSize: 24, Content: pngBytes(16)})
	second := renderer.Render(File{Filename: "b.png", ContentType: "image/png", ByteSize: 24, Content: pngBytes(16)})

	require.NotNil(t, first.Image)
	require.Nil(t, second.Image)
	require.Contains(t, second.Text, "batas jumlah gambar tercapai")
}

func TestRenderOfficeDocumentBecomesANote(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "foto.docx", ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		ByteSize: 2048, Content: []byte("PK\x03\x04binary"),
	})

	require.Equal(t,
		"[Lampiran foto.docx (application/vnd.openxmlformats-officedocument.wordprocessingml.document, 2.0 KB): isi tidak dibaca — tipe ini belum didukung]",
		part.Text)
}

func TestRenderUnreadableFileBecomesANote(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "stok.txt", ContentType: "text/plain",
		ByteSize: 22, Err: errors.New("storage: object not found"),
	})

	require.Equal(t, "[Lampiran stok.txt tidak bisa dibaca: storage: object not found]", part.Text)
}

func TestRenderTooLargeFileBecomesANote(t *testing.T) {
	renderer := NewRenderer(Limits{})

	part := renderer.Render(File{
		Filename: "besar.csv", ContentType: "text/csv",
		ByteSize: 99 << 20, Err: ErrTooLarge,
	})

	require.Contains(t, part.Text, "[Lampiran besar.csv tidak bisa dibaca: berkas lebih besar dari batas baca]")
}

func TestOversized(t *testing.T) {
	limits := Limits{MaxFileBytes: 10}

	require.False(t, limits.Oversized(10))
	require.True(t, limits.Oversized(11))
	require.False(t, Limits{}.Oversized(DefaultMaxFileBytes))
	require.True(t, Limits{}.Oversized(DefaultMaxFileBytes+1))
}

func TestNormaliseFillsEveryField(t *testing.T) {
	require.Equal(t, Limits{
		MaxFileBytes:    DefaultMaxFileBytes,
		MaxCharsPerFile: DefaultMaxCharsPerFile,
		MaxCharsTotal:   DefaultMaxCharsTotal,
		MaxImages:       DefaultMaxImages,
		MaxImageBytes:   DefaultMaxImageBytes,
	}, Limits{}.Normalise())

	require.Equal(t, Limits{
		MaxFileBytes: 1, MaxCharsPerFile: 2, MaxCharsTotal: 3, MaxImages: 4, MaxImageBytes: 5,
	}, Limits{MaxFileBytes: 1, MaxCharsPerFile: 2, MaxCharsTotal: 3, MaxImages: 4, MaxImageBytes: 5}.Normalise())
}

// The total budget is what keeps a thread full of files from filling the
// window: once it is spent, the next file is named rather than quoted.
func TestRenderSpendsTheTotalBudget(t *testing.T) {
	renderer := NewRenderer(Limits{MaxCharsTotal: 100})

	first := renderer.Render(File{
		Filename: "satu.txt", ContentType: "text/plain", ByteSize: 40,
		Content: []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	second := renderer.Render(File{
		Filename: "dua.txt", ContentType: "text/plain", ByteSize: 40,
		Content: []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	})

	require.Contains(t, first.Text, "aaaa")
	require.Equal(t, "[Lampiran dua.txt (text/plain, 40 B): isi tidak dibaca — batas total konteks lampiran tercapai]", second.Text)
}

func TestTurnRendersTextAndImages(t *testing.T) {
	renderer := NewRenderer(Limits{})
	var turn Turn

	turn.Add(renderer.Render(File{
		Filename: "stok.txt", ContentType: "text/plain", ByteSize: 22,
		Content: []byte("Stok kaos merah: 7 pcs"),
	}))
	turn.Add(renderer.Render(File{
		Filename: "foto.png", ContentType: "image/png", ByteSize: 24, Content: pngBytes(16),
	}))

	require.False(t, turn.Empty())
	require.Contains(t, turn.Text(true), "Stok kaos merah: 7 pcs")
	require.Contains(t, turn.Text(true), "[Gambar foto.png (image/png, 24 B)]")

	images := turn.Images()
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MediaType)

	// Without vision the same turn is still readable: the picture is named and
	// said to be unreadable rather than announced as sent.
	require.Contains(t, turn.Text(false), "[Gambar foto.png tidak bisa dibaca: model yang aktif tidak mendukung gambar]")
	require.NotContains(t, turn.Text(false), "24 B")
}

func TestTurnIsEmptyWithoutParts(t *testing.T) {
	var turn Turn

	require.True(t, turn.Empty())
	require.Empty(t, turn.Text(true))
	require.Nil(t, turn.Images())
}

func TestSizeLabel(t *testing.T) {
	require.Equal(t, "0 B", sizeLabel(0))
	require.Equal(t, "22 B", sizeLabel(22))
	require.Equal(t, "1.0 KB", sizeLabel(1024))
	require.Equal(t, "1.5 KB", sizeLabel(1536))
	require.Equal(t, "2.0 MB", sizeLabel(2<<20))
}

func TestReasonIsBoundedAndSingleLine(t *testing.T) {
	part := NewRenderer(Limits{}).Render(File{
		Filename: "x.txt", Err: errors.New("a\nb  " + strings.Repeat("c", 500)),
	})

	require.Contains(t, part.Text, "a b cccc")
	require.NotContains(t, part.Text, "\n")
	require.Less(t, len(part.Text), 300)
}
