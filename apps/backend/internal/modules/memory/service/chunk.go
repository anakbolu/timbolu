package service

import "strings"

// maxDocumentRunes bounds one upload, so a paste of a whole book cannot fill the
// table and the bill in one request.
const maxDocumentRunes = 200_000

// maxChunks bounds how many pieces one upload becomes, for the same reason.
const maxChunks = 200

// chunkText splits a document into pieces of about `size` runes, breaking on
// blank lines first and on rune boundaries second.
//
// The pieces overlap by `overlap` runes so a sentence that falls on a boundary is
// still findable from either side, and the split is on paragraphs before it is on
// characters, because a chunk that starts in the middle of a sentence embeds
// worse than one that starts at a heading.
func chunkText(text string, size, overlap int) []string {
	if size <= 0 {
		size = 1000
	}
	if overlap < 0 || overlap >= size {
		overlap = 0
	}

	normalised := strings.ReplaceAll(text, "\r\n", "\n")
	paragraphs := strings.Split(normalised, "\n\n")

	chunks := make([]string, 0, 8)
	var current []string
	currentLen := 0

	flush := func() {
		joined := strings.TrimSpace(strings.Join(current, "\n\n"))
		if joined != "" {
			chunks = append(chunks, joined)
		}
		current = current[:0]
		currentLen = 0
	}

	for _, paragraph := range paragraphs {
		trimmed := strings.TrimSpace(paragraph)
		if trimmed == "" {
			continue
		}

		if len([]rune(trimmed)) > size {
			// A paragraph nobody ended is cut at rune boundaries, with the tail
			// of one piece repeated at the head of the next.
			flush()
			chunks = append(chunks, splitLong(trimmed, size, overlap)...)
			continue
		}

		if currentLen > 0 && currentLen+len([]rune(trimmed)) > size {
			flush()
		}

		current = append(current, trimmed)
		currentLen += len([]rune(trimmed))
	}

	flush()

	return chunks
}

// splitLong cuts one over-long paragraph into overlapping pieces.
func splitLong(text string, size, overlap int) []string {
	runes := []rune(text)
	pieces := make([]string, 0, len(runes)/size+1)

	for start := 0; start < len(runes); start += size - overlap {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}

		piece := strings.TrimSpace(string(runes[start:end]))
		if piece != "" {
			pieces = append(pieces, piece)
		}

		if end == len(runes) {
			break
		}
	}

	return pieces
}
