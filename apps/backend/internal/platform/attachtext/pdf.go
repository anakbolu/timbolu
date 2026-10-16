package attachtext

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ledongthuc/pdf"
)

// pdfTimeout bounds one extraction. A malformed PDF can send the reader into a
// loop over a cross-reference table that points at itself, and a bounded answer
// is worth more than a complete one: the file's note still reaches the model.
const pdfTimeout = 5 * time.Second

// pdfMaxTextBytes bounds what is read out of one PDF before the per-file
// character limit is applied. The reader is an io.Reader over a document the
// caller already fetched, so this bound is about the cost of assembling the
// string, not about the network.
const pdfMaxTextBytes = 4 << 20

// errPDFTimeout is the reason a PDF that took too long is reported with.
var errPDFTimeout = errors.New("ekstraksi PDF melebihi batas waktu")

// pdfResult is what the extraction goroutine reports back.
type pdfResult struct {
	text string
	err  error
}

// extractPDF reads the text layer of a PDF.
//
// It never panics and never blocks forever: the reader is a third-party parser
// over untrusted bytes, so the call runs under recover, and it runs in its own
// goroutine so the caller can give up on a document that will not finish. The
// result channel is buffered, so giving up leaks nothing but the goroutine
// itself, and only for a file that is already pathological.
func extractPDF(content []byte) (string, error) {
	if len(content) == 0 {
		return "", nil
	}

	done := make(chan pdfResult, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- pdfResult{err: fmt.Errorf("PDF tidak bisa dibaca: %v", recovered)}
			}
		}()

		text, err := readPDF(content)
		done <- pdfResult{text: text, err: err}
	}()

	select {
	case result := <-done:
		return result.text, result.err
	case <-time.After(pdfTimeout):
		return "", errPDFTimeout
	}
}

// readPDF does the extraction, without any of the guards.
func readPDF(content []byte) (string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", fmt.Errorf("PDF tidak bisa dibaca: %w", err)
	}

	plain, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("teks PDF tidak bisa diekstrak: %w", err)
	}

	text, err := io.ReadAll(io.LimitReader(plain, pdfMaxTextBytes))
	if err != nil {
		return "", fmt.Errorf("teks PDF tidak bisa dibaca: %w", err)
	}

	return string(text), nil
}
