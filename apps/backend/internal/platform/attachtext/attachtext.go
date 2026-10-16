// Package attachtext turns the files a message carries into the text — and the
// images — a model reads.
//
// It is the one implementation of that translation, shared by the in-process
// responder and the durable task runtime: both ask the same question ("what does
// this attachment contribute to the prompt?") and must answer it the same way,
// or the same file would be read differently depending on which path answered
// the message.
//
// Three rules run through the package:
//
//   - one bad file never fails a reply. Every failure — a missing object, a
//     corrupt PDF, an unsupported type — becomes a note in the prompt, so the
//     model is told the file exists and could not be read rather than being
//     shown a message with no sign of it;
//   - the budget is spent on the newest files first, because the message a
//     person just sent is the one they are asking about;
//   - what a file costs is bounded twice: per file, and across the request.
package attachtext

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// The product's judgement about how much of an attachment is worth a prompt,
// used when the configuration says nothing.
const (
	// DefaultMaxFileBytes is the largest file that is fetched at all.
	DefaultMaxFileBytes int64 = 10 << 20
	// DefaultMaxCharsPerFile bounds one file's text.
	DefaultMaxCharsPerFile = 20000
	// DefaultMaxCharsTotal bounds the text of every attachment of one request.
	DefaultMaxCharsTotal = 60000
	// DefaultMaxImages bounds how many pictures one request carries.
	DefaultMaxImages = 4
	// DefaultMaxImageBytes is the largest picture sent to a vision model.
	DefaultMaxImageBytes int64 = 5 << 20
)

// reasonMaxRunes bounds the reason quoted in a note. An error from a storage
// driver is normally one line; a pathological one must not become the prompt.
const reasonMaxRunes = 200

// partSeparator sits between two attachments' contributions to one message.
const partSeparator = "\n\n"

// ErrTooLarge is the reason a file was never fetched. A caller sets it on a File
// instead of reading the bytes, so the limit costs no download.
var ErrTooLarge = errors.New("berkas lebih besar dari batas baca")

// Limits bounds what attachments contribute to one request. A zero field means
// the default above, which is why Normalise is applied at the point a limit is
// read rather than trusted from configuration.
type Limits struct {
	// MaxFileBytes is the largest file fetched. A bigger one is not downloaded
	// at all: its note costs a line, its bytes would cost the request.
	MaxFileBytes int64
	// MaxCharsPerFile is how much of one file's text is included.
	MaxCharsPerFile int
	// MaxCharsTotal is how much text every attachment of one request may
	// contribute together.
	MaxCharsTotal int
	// MaxImages is how many pictures one request may carry.
	MaxImages int
	// MaxImageBytes is the largest picture that is sent as an image.
	MaxImageBytes int64
}

// Normalise fills every unset field from the defaults, so a deployment that
// configured nothing still has bounds.
func (l Limits) Normalise() Limits {
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxCharsPerFile <= 0 {
		l.MaxCharsPerFile = DefaultMaxCharsPerFile
	}
	if l.MaxCharsTotal <= 0 {
		l.MaxCharsTotal = DefaultMaxCharsTotal
	}
	if l.MaxImages <= 0 {
		l.MaxImages = DefaultMaxImages
	}
	if l.MaxImageBytes <= 0 {
		l.MaxImageBytes = DefaultMaxImageBytes
	}

	return l
}

// Oversized reports whether a file is too large to fetch, which the caller asks
// before reading the object so the limit costs no download.
func (l Limits) Oversized(byteSize int64) bool {
	return byteSize > l.Normalise().MaxFileBytes
}

// File is one attachment: what is known about it without reading it, and its
// bytes when they were read.
type File struct {
	Filename    string
	ContentType string
	ByteSize    int64
	// Content is the file's bytes. It is empty when Err is set, and also for an
	// empty file, which is not a failure.
	Content []byte
	// Err is why the bytes are absent: ErrTooLarge when the file was never
	// fetched, or whatever the storage reported.
	Err error
}

// Image is a picture that travels to the model as an image rather than as text.
type Image struct {
	MediaType string
	// Data is the base64 of the picture, without a data: prefix, which is the
	// shape every provider's vision API wants.
	Data string
}

// Part is one attachment's contribution to a message.
type Part struct {
	// Text is what the model reads: the file's content in a labelled section, or
	// the note that says why there is none. For a picture it is the line that
	// announces it.
	Text string
	// NoVision replaces Text when the active chain cannot see pictures, so a
	// model that is asked about a photo it never received is told exactly that
	// instead of being shown an announcement of a picture that is not there.
	// It is empty for a part that carries no picture.
	NoVision string
	// Image is set only for a picture that fits every bound.
	Image *Image
	// Err is why a file that was meant to be read was not: a missing object, an
	// oversized file, a corrupt PDF. It is what the caller logs. A limit that
	// was reached on purpose and a type that is not read are decisions rather
	// than failures, and leave it nil.
	Err error
}

// Turn is what the attachments of one message contribute to it.
//
// It exists because the contribution is not final until the model is known: a
// chain without vision turns every picture into a note, and the same turn has to
// be renderable both ways.
type Turn struct {
	parts []Part
}

// Add appends a rendered attachment to the turn.
func (t *Turn) Add(part Part) {
	if part.Text == "" {
		return
	}
	t.parts = append(t.parts, part)
}

// Empty reports whether the turn contributes nothing, which is the case for a
// message with no attachments.
func (t *Turn) Empty() bool {
	return t == nil || len(t.parts) == 0
}

// Text renders the turn. When vision is false every picture is replaced by the
// line that says it was not read.
func (t *Turn) Text(vision bool) string {
	if t == nil || len(t.parts) == 0 {
		return ""
	}

	rendered := make([]string, 0, len(t.parts))
	for _, part := range t.parts {
		text := part.Text
		if !vision && part.NoVision != "" {
			text = part.NoVision
		}
		rendered = append(rendered, text)
	}

	return strings.Join(rendered, partSeparator)
}

// Images returns the pictures the turn carries, in the order they were added.
// It is nil for a turn that carries none, which is what "no picture here" has to
// look like to a caller that is deciding whether to ask for vision.
func (t *Turn) Images() []Image {
	if t == nil {
		return nil
	}

	var images []Image
	for _, part := range t.parts {
		if part.Image != nil {
			images = append(images, *part.Image)
		}
	}

	return images
}

// Renderer renders the attachments of one request, spending that request's
// budget as it goes.
//
// It is a value with state rather than a function because the budget is per
// request: one Renderer per request, called once per attachment, newest first.
type Renderer struct {
	limits Limits
	chars  int
	images int
}

// NewRenderer builds a renderer for one request.
func NewRenderer(limits Limits) *Renderer {
	return &Renderer{limits: limits.Normalise()}
}

// Render turns one attachment into the part it contributes.
//
// It never fails: a file that cannot be read becomes the note that says so.
func (r *Renderer) Render(file File) Part {
	if file.Err != nil {
		return r.unreadable(file, reasonOf(file.Err))
	}

	// The stored type is a hint, not the truth: a client can upload anything
	// under any name, so the bytes decide whether this is a picture.
	if mediaType := sniffImage(file.Content); mediaType != "" {
		return r.image(file, mediaType)
	}

	// The boundary of a section is drawn per file rather than once per renderer:
	// a marker shared by every file of a request is one marker a file could
	// contain, and a file that contains the closing marker ends its own section.
	nonce := boundaryNonce()

	switch {
	case isPDF(file):
		text, err := extractPDF(file.Content)
		return r.document(file, nonce, text, err)
	case isText(file):
		return r.document(file, nonce, textOf(file.Content), nil)
	default:
		return r.note(file, "isi tidak dibaca — tipe ini belum didukung")
	}
}

// image sends a picture to the model, or explains why it does not.
func (r *Renderer) image(file File, mediaType string) Part {
	if int64(len(file.Content)) > r.limits.MaxImageBytes {
		return r.note(file, fmt.Sprintf("isi tidak dibaca — gambar lebih besar dari batas %s",
			sizeLabel(r.limits.MaxImageBytes)))
	}
	if r.images >= r.limits.MaxImages {
		return r.note(file, "isi tidak dibaca — batas jumlah gambar tercapai")
	}

	r.images++
	name := sanitiseName(file.Filename)
	text := fmt.Sprintf("[Gambar %s (%s, %s)]", name, mediaType, sizeLabel(file.ByteSize))
	r.chars += utf8.RuneCountInString(text)

	return Part{
		Text:     text,
		NoVision: fmt.Sprintf("[Gambar %s tidak bisa dibaca: model yang aktif tidak mendukung gambar]", name),
		Image: &Image{
			MediaType: mediaType,
			Data:      base64.StdEncoding.EncodeToString(file.Content),
		},
	}
}

// document renders a file whose content is text, whatever the source: a text
// file, or the text extracted from a PDF.
//
// An extraction that found nothing is a fact about the file — a scanned PDF has
// no text layer — and is said rather than shown as an empty section.
func (r *Renderer) document(file File, nonce, text string, err error) Part {
	if err != nil {
		return r.unreadable(file, reasonOf(err))
	}
	if isPDF(file) && strings.TrimSpace(text) == "" {
		return r.note(file, "PDF tanpa teks yang bisa diekstrak")
	}

	text, marker := truncate(text, r.limits.MaxCharsPerFile)
	text = section(file, nonce, text, marker)

	// The total budget is a hard bound: a file whose text does not fit in what
	// is left contributes its note and nothing else, so a request never carries
	// more text than the bound allows.
	if utf8.RuneCountInString(text) > r.remaining() {
		return r.note(file, "isi tidak dibaca — batas total konteks lampiran tercapai")
	}

	r.chars += utf8.RuneCountInString(text)

	return Part{Text: text}
}

// note renders the metadata-only line for a file whose content is deliberately
// not included, which is what keeps the model aware of the file without paying
// for it.
func (r *Renderer) note(file File, reason string) Part {
	text := fmt.Sprintf("[Lampiran %s (%s, %s): %s]",
		sanitiseName(file.Filename), mimeOf(file), sizeLabel(file.ByteSize), reason)
	r.chars += utf8.RuneCountInString(text)

	return Part{Text: text}
}

// unreadable renders the note for a file that was meant to be read and was not,
// where the reason is the whole message. The failure travels with the part, so a
// caller that has a logger can record which files never reach the model.
func (r *Renderer) unreadable(file File, reason string) Part {
	text := fmt.Sprintf("[Lampiran %s tidak bisa dibaca: %s]", sanitiseName(file.Filename), reason)
	r.chars += utf8.RuneCountInString(text)

	return Part{Text: text, Err: errors.New(reason)}
}

// remaining is how much of the request's total text budget is unspent.
func (r *Renderer) remaining() int {
	return r.limits.MaxCharsTotal - r.chars
}

// A file's own text must not be able to spell either marker tag, so both are
// matched case-insensitively: a model reads the case of a marker as noise.
var (
	openTagPattern  = regexp.MustCompile(`(?i)\[Lampiran`)
	closeTagPattern = regexp.MustCompile(`(?i)\[/Lampiran`)
)

// nameStripper removes what would let a filename break the marker it is quoted
// in: the line breaks and tabs that would end the line, and the brackets that
// would make the name look like a marker of its own.
var nameStripper = strings.NewReplacer("\r", "", "\n", "", "\t", "", "[", "", "]", "")

// nonceBytes is the width of a boundary nonce. Four bytes is eight hex
// characters: short enough to read in a prompt, wide enough that a file cannot
// carry the marker by accident, let alone on purpose.
const nonceBytes = 4

// fallbackNonce numbers the boundaries in the one case crypto/rand cannot.
var fallbackNonce atomic.Uint64

// boundaryNonce returns the marker that ends one file's section.
//
// It is random rather than derived from the file, because a marker a file can
// predict is a marker a file can contain, and a file that contains the closing
// marker ends its own section — leaving the text after it to read as though the
// application had written it.
func boundaryNonce() string {
	var raw [nonceBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand is documented never to fail. If a source ever does, a
		// counter still gives every boundary a marker of its own, which is the
		// one property a section needs.
		return fmt.Sprintf("%0*x", nonceBytes*2, fallbackNonce.Add(1))
	}

	return hex.EncodeToString(raw[:])
}

// sanitiseName is the filename as a marker may quote it: one line, with nothing
// in it that could pass for a bracket of the marker itself.
func sanitiseName(name string) string {
	return strings.TrimSpace(nameStripper.Replace(name))
}

// neutralise makes a file's text unable to pass for the markers around it.
//
// The closing tag is broken first and the opening one second, so the replacement
// of the first cannot be read as the second. The nonce goes last: it is what makes
// the closing marker unforgeable, so the one string that must never survive into a
// section is the nonce itself.
func neutralise(text, nonce string) string {
	text = closeTagPattern.ReplaceAllLiteralString(text, "[/ Lampiran")
	text = openTagPattern.ReplaceAllLiteralString(text, "[ Lampiran")

	return strings.ReplaceAll(text, nonce, "#")
}

// section wraps content in the markers that tell the model where the file
// begins and ends, so the text after a section cannot be read as part of it.
// The marker is the truncation line, empty when nothing was dropped.
//
// The name and the text are neutralised before they are quoted — a file is data,
// so neither of them may spell a marker and pass for something the application
// said — and both markers carry the nonce, so a section can only be ended by the
// marker that opened it.
func section(file File, nonce, text, marker string) string {
	if marker != "" {
		text += "\n" + marker
	}

	return fmt.Sprintf("[Lampiran: %s (%s, %s) #%s]\n%s\n[/Lampiran #%s]",
		sanitiseName(file.Filename), mimeOf(file), sizeLabel(file.ByteSize), nonce,
		neutralise(text, nonce), nonce)
}

// truncate keeps at most limit characters and returns the marker that says how
// much was dropped, empty when nothing was.
func truncate(text string, limit int) (string, string) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, ""
	}

	return string(runes[:limit]), truncationMarker(limit, len(runes))
}

// truncationMarker is the line a truncated file carries, so the model knows it
// is reading an excerpt rather than the whole document.
func truncationMarker(shown, total int) string {
	return fmt.Sprintf("[... terpotong: %d dari %d karakter ditampilkan]", shown, total)
}

// textOf reads bytes as text, replacing anything that is not valid UTF-8 rather
// than dropping it: a file with one bad byte is still mostly readable.
func textOf(content []byte) string {
	return strings.ToValidUTF8(string(content), "\uFFFD")
}

// isText reports whether a file is one of the text-like types the model reads
// directly, by its type or by its extension when the type says nothing.
func isText(file File) bool {
	switch mimeOf(file) {
	case "text/plain", "text/csv", "text/markdown", "application/json":
		return true
	}

	switch extension(file.Filename) {
	case ".txt", ".csv", ".md", ".json", ".log", ".tsv":
		return true
	}

	return false
}

// isPDF reports whether a file is a PDF, by its type or by its extension.
func isPDF(file File) bool {
	return mimeOf(file) == "application/pdf" || extension(file.Filename) == ".pdf"
}

// sniffImage reports which of the four supported picture types the bytes are,
// or the empty string when they are something else. The bytes decide: a stored
// content type is a claim, and this is the check.
func sniffImage(content []byte) string {
	if len(content) == 0 {
		return ""
	}

	switch mediaType := http.DetectContentType(content); mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mediaType
	default:
		return ""
	}
}

// mimeOf is the type a section header shows: the stored one without its
// parameters, or a generic type when the deployment stored none.
func mimeOf(file File) string {
	if base := baseMime(file.ContentType); base != "" {
		return base
	}

	return "application/octet-stream"
}

// baseMime strips the parameters of a content type, so a header reads
// "text/plain" rather than "text/plain; charset=utf-8".
func baseMime(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// extension is the lower-case extension of a filename, including the dot.
func extension(filename string) string {
	return strings.ToLower(path.Ext(strings.TrimSpace(filename)))
}

// reasonOf renders why a file could not be read, bounded and on one line.
func reasonOf(err error) string {
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if runes := []rune(reason); len(runes) > reasonMaxRunes {
		return string(runes[:reasonMaxRunes]) + "…"
	}

	return reason
}

// sizeLabel renders a byte count the way a person reads it. A small file is
// quoted exactly, because "22 B" is more useful than "0.0 KB".
func sizeLabel(byteSize int64) string {
	switch {
	case byteSize < 1024:
		return fmt.Sprintf("%d B", byteSize)
	case byteSize < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(byteSize)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(byteSize)/(1<<20))
	}
}
