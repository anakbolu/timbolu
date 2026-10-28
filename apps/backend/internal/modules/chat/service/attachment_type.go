package service

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// attachmentType is one extension an upload may carry: what the file is stored
// as, and the sniffed types content of that extension really has.
//
// The two are checked together because either alone is a hole. An extension is
// what a client types, so `invoice.exe` renamed to `invoice.png` would pass on
// the name; the sniffed type is what the bytes are, so a text file named
// `logo.png` would pass on the content. A file is accepted only when both agree,
// and what is stored is the type derived from the extension — never the header
// the client sent, which is a claim and not a fact.
type attachmentType struct {
	contentType string
	sniffed     []string
}

// knownAttachmentTypes maps an accepted extension to what it may hold.
//
// The office formats are the interesting entries: `docx` and its two siblings
// are zip containers, and the legacy `doc`/`xls`/`ppt` are OLE compound files,
// which Go's sniffer reports as a bare octet stream. A sniffer can only say that
// the bytes are one of those containers, so the table stops there and the checks
// that need the inside — an OOXML package's entries, a compound file's signature
// — live in validateContainer, which runs after the sniff agrees.
var knownAttachmentTypes = map[string]attachmentType{
	"png":  {contentType: "image/png", sniffed: []string{"image/png"}},
	"jpg":  {contentType: "image/jpeg", sniffed: []string{"image/jpeg"}},
	"jpeg": {contentType: "image/jpeg", sniffed: []string{"image/jpeg"}},
	"gif":  {contentType: "image/gif", sniffed: []string{"image/gif"}},
	"webp": {contentType: "image/webp", sniffed: []string{"image/webp"}},
	"pdf":  {contentType: "application/pdf", sniffed: []string{"application/pdf"}},
	"txt":  {contentType: "text/plain", sniffed: []string{"text/plain"}},
	"csv":  {contentType: "text/csv", sniffed: []string{"text/plain"}},
	"md":   {contentType: "text/markdown", sniffed: []string{"text/plain"}},
	"docx": {
		contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		sniffed:     []string{"application/zip"},
	},
	"xlsx": {
		contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		sniffed:     []string{"application/zip"},
	},
	"pptx": {
		contentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		sniffed:     []string{"application/zip"},
	},
	"doc": {contentType: "application/msword", sniffed: legacyOffice},
	"xls": {contentType: "application/vnd.ms-excel", sniffed: legacyOffice},
	"ppt": {contentType: "application/vnd.ms-powerpoint", sniffed: legacyOffice},
}

// legacyOffice is what an OLE compound file may sniff as. Go's sniffer answers a
// bare octet stream for it; the other two are what other sniffers name the same
// container, so a file that came from one of them is not refused for the label.
var legacyOffice = []string{"application/octet-stream", "application/x-ole-storage", "application/vnd.ms-office"}

// oleHeader is the signature every OLE compound file opens with, which is what a
// legacy Office document is. The sniffed label above says "some container"; this
// says "the container Word, Excel, and PowerPoint write", and it is the one thing
// that tells a real `doc` from 512 bytes of anything renamed to it.
var oleHeader = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// ooxmlContentTypes is the entry that makes a zip an OOXML package: Word, Excel,
// and PowerPoint all declare their parts in it, so a zip without it is a renamed
// archive and not the document the extension promises.
const ooxmlContentTypes = "[Content_Types].xml"

// maxOOXMLEntries caps how many central-directory entries an OOXML upload may
// have. An office file holds tens; a cap keeps a walk of the names cheap and turns
// an archive built to have a hundred thousand of them into a refusal instead.
const maxOOXMLEntries = 1024

// defaultAttachmentExtensions is the allowlist of a deployment that configured
// none: the images, documents, and text files a chat is actually used for.
var defaultAttachmentExtensions = []string{
	"png", "jpg", "jpeg", "gif", "webp",
	"pdf",
	"txt", "csv", "md",
	"docx", "xlsx", "pptx",
	"doc", "xls", "ppt",
}

// forbiddenExtensions are refused whatever the configuration says.
//
// An allowlist is a policy a deployment sets, and this is not: an executable
// uploaded to a chat is a payload handed to whoever opens it, so no
// configuration can turn it on. `elf` and `bin` are on the list because both
// name a Unix binary, and `app` because it names a macOS bundle.
var forbiddenExtensions = map[string]bool{
	"exe": true, "dll": true, "com": true, "bat": true, "cmd": true,
	"msi": true, "scr": true, "ps1": true, "sh": true, "jar": true,
	"apk": true, "app": true, "elf": true, "bin": true,
}

// executableMagics are the leading bytes of a program, whatever it is named.
//
// The sniffed type is not enough for these: Go reports a bare octet stream for a
// PE, an ELF, and a Mach-O image alike, and a shebang script sniffs as plain
// text. Reading the magic is what makes "executables are refused" a rule about
// the bytes rather than about the name.
var executableMagics = [][]byte{
	{'M', 'Z'},               // PE
	{0x7f, 'E', 'L', 'F'},    // ELF
	{0xfe, 0xed, 0xfa, 0xce}, // Mach-O, 32-bit big endian (feedface)
	{0xce, 0xfa, 0xed, 0xfe}, // Mach-O, 32-bit little endian (cefaedfe)
	{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O, 64-bit big endian (feedfacf)
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O, 64-bit little endian (cffaedfe)
	{0xca, 0xfe, 0xba, 0xbe}, // universal binary, and a Java class
	{'#', '!'},               // shebang
}

// validateAttachment checks one upload against the allowlist and answers the type
// it is stored as.
//
// It is deliberately the first thing a store does: a refused upload must not open
// a message, write an object, or publish an event, so nothing may happen before
// this returns.
func validateAttachment(filename string, content []byte, allowed []string) (string, error) {
	if executableContent(content) {
		return "", fmt.Errorf("%w: the content is an executable, which is never accepted",
			chatdomain.ErrFileTypeNotAllowed)
	}

	if len(allowed) == 0 {
		return "", fmt.Errorf("%w: no attachment type is enabled for this deployment, so nothing can be uploaded",
			chatdomain.ErrFileTypeNotAllowed)
	}

	extension := normalizeExtension(filepath.Ext(filename))
	if extension == "" {
		return "", fmt.Errorf("%w: the file needs an extension; allowed: %s",
			chatdomain.ErrFileTypeNotAllowed, strings.Join(allowed, ", "))
	}
	if forbiddenExtensions[extension] {
		return "", fmt.Errorf("%w: .%s is an executable, which is never accepted",
			chatdomain.ErrFileTypeNotAllowed, extension)
	}
	if !containsExtension(allowed, extension) {
		return "", fmt.Errorf("%w: .%s is not accepted; allowed: %s",
			chatdomain.ErrFileTypeNotAllowed, extension, strings.Join(allowed, ", "))
	}

	known, ok := knownAttachmentTypes[extension]
	if !ok {
		// Unreachable through the allowlist, which only ever names a known
		// extension, and kept so a caller that hands this function its own list
		// cannot skip the content check.
		return "", fmt.Errorf("%w: .%s is not a type this service knows",
			chatdomain.ErrFileTypeNotAllowed, extension)
	}
	if !sniffMatches(http.DetectContentType(content), known.sniffed) {
		return "", fmt.Errorf("%w: the content of .%s is not a %s file",
			chatdomain.ErrFileTypeNotAllowed, extension, known.contentType)
	}

	if err := validateContainer(extension, content); err != nil {
		return "", err
	}

	return known.contentType, nil
}

// validateContainer is the check a sniffer cannot make for a container format.
//
// `application/zip` is what a spreadsheet and a program zipped together both look
// like, and `application/octet-stream` is what anything unknown looks like, so
// the sniff alone accepts an archive of executables named `docx` and 512 random
// bytes named `doc`. This opens the container far enough to tell.
//
// It reads the central directory and the first eight bytes — no entry is
// decompressed — so neither check can be turned into a zip bomb.
func validateContainer(extension string, content []byte) error {
	switch extension {
	case "docx", "xlsx", "pptx":
		return validateOOXML(extension, content)
	case "doc", "xls", "ppt":
		if !bytes.HasPrefix(content, oleHeader) {
			return fmt.Errorf("%w: the content of .%s is not a compound Office file",
				chatdomain.ErrFileTypeNotAllowed, extension)
		}
	}
	return nil
}

// validateOOXML walks the central directory of the zip an OOXML upload is and
// refuses it unless it is the package the extension claims.
//
// `[Content_Types].xml` is the difference between a document and a renamed zip:
// every Word, Excel, and PowerPoint file carries it. An executable entry is
// refused too — Office does not run one out of its own package, but whatever
// unpacks the upload next might, and the allowlist promises the chat holds no
// program. A name that ends in `.` is refused for the same reason a file name is
// checked: it is not a part any reader opens.
//
// The entries are counted before they are read, because a zip can declare more
// names than an office file would ever have; refusing it costs one comparison.
func validateOOXML(extension string, content []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return fmt.Errorf("%w: the zip inside .%s cannot be read",
			chatdomain.ErrFileTypeNotAllowed, extension)
	}
	if len(archive.File) > maxOOXMLEntries {
		return fmt.Errorf("%w: the zip inside .%s holds %d entries, more than the %d an office file has",
			chatdomain.ErrFileTypeNotAllowed, extension, len(archive.File), maxOOXMLEntries)
	}

	declaresParts := false
	for _, entry := range archive.File {
		name := entry.Name
		switch {
		case name == ooxmlContentTypes:
			declaresParts = true
		case name == "" || strings.HasSuffix(name, "."):
			return fmt.Errorf("%w: the zip inside .%s holds an entry with no usable name, %q",
				chatdomain.ErrFileTypeNotAllowed, extension, name)
		case forbiddenExtensions[normalizeExtension(filepath.Ext(name))]:
			return fmt.Errorf("%w: the zip inside .%s holds an executable entry %q",
				chatdomain.ErrFileTypeNotAllowed, extension, name)
		}
	}
	if !declaresParts {
		return fmt.Errorf("%w: the zip inside .%s is not an Office package: it has no %s",
			chatdomain.ErrFileTypeNotAllowed, extension, ooxmlContentTypes)
	}

	return nil
}

// executableContent reports whether content opens with the bytes of a program.
func executableContent(content []byte) bool {
	for _, magic := range executableMagics {
		if len(content) >= len(magic) && string(content[:len(magic)]) == string(magic) {
			return true
		}
	}
	return false
}

// sniffMatches compares a sniffed type against the ones an extension may hold.
//
// The parameters are dropped first: a sniffer answers `text/plain; charset=utf-8`
// for a text file, and the charset is not part of the type a caller accepts.
func sniffMatches(sniffed string, accepted []string) bool {
	base := sniffed
	if index := strings.IndexByte(base, ';'); index >= 0 {
		base = strings.TrimSpace(base[:index])
	}

	for _, want := range accepted {
		if base == want {
			return true
		}
	}
	return false
}

// normalizeExtension reduces an extension to the one spelling the tables use: no
// leading dot, no case. `PNG`, `.png`, and `png` all name the same type, and a
// client sends whichever it likes.
func normalizeExtension(extension string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(extension)), ".")
}

// containsExtension reports whether an allowlist names one extension.
func containsExtension(allowed []string, extension string) bool {
	for _, candidate := range allowed {
		if candidate == extension {
			return true
		}
	}
	return false
}

// allowedExtensions turns what a deployment configured into the list the
// validator uses.
//
// The configuration may only narrow what the service knows: an extension outside
// the table above has no content check to apply, so naming one would be a way to
// accept any bytes at all, and naming an executable is refused outright. Both are
// ignored with a warning rather than failing the start-up — a typo in a
// deployment's list must not take the chat down.
//
// A list that names nothing usable is the one case that does not fall back: the
// deployment that asked for the narrowest policy would otherwise get the widest
// one, and an allowlist that silently widens is a policy that does nothing. The
// answer is an empty list — not nil, so the intent reads as a list — and every
// upload is refused until the configuration is fixed.
func allowedExtensions(configured []string, log *zap.Logger) []string {
	if len(configured) == 0 {
		return defaultAttachmentExtensions
	}

	allowed := make([]string, 0, len(configured))
	seen := make(map[string]bool, len(configured))
	for _, raw := range configured {
		extension := normalizeExtension(raw)
		if extension == "" || seen[extension] {
			continue
		}
		switch {
		case forbiddenExtensions[extension]:
			warnUnusableExtension(log, extension, "it is an executable")
		case knownAttachmentTypes[extension].contentType == "":
			warnUnusableExtension(log, extension, "it is not a type this service knows")
		default:
			seen[extension] = true
			allowed = append(allowed, extension)
		}
	}

	if len(allowed) == 0 {
		if log != nil {
			log.Error("no configured attachment extension is usable: every upload is refused until the list is fixed",
				zap.Strings("configured", configured))
		}
		return []string{}
	}

	return allowed
}

func warnUnusableExtension(log *zap.Logger, extension, reason string) {
	if log == nil {
		return
	}
	log.Warn("ignoring a configured attachment extension",
		zap.String("extension", extension), zap.String("reason", reason))
}
