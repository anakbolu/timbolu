package handler

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheMatrixIsAScannableShape: the screen draws what this returns, so it has to
// be a square matrix of bits with a quiet zone — a phone needs the margin, and a
// ragged matrix would draw as a broken picture rather than as a code.
func TestTheMatrixIsAScannableShape(t *testing.T) {
	// A payload of the shape WhatsApp sends: the code plus the reference, the
	// public key, and the advanced secret.
	rows := qrMatrix("2@AbCdEfGhIjKlMnOpQrStUvWxYz0123456789,AbCdEfGhIjKlMnOpQrStUvWxYz==,AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+,AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+\n")

	require.NotEmpty(t, rows)
	size := len(rows)
	require.GreaterOrEqual(t, size, 21, "a QR code's smallest version is 21 modules")

	for _, row := range rows {
		require.Len(t, row, size, "the matrix is square")
		require.True(t, strings.Trim(row, "01") == "", "a row is bits and nothing else")
	}

	// The quiet zone is what a phone needs to find the code's edges: four modules
	// of margin on every side, which is why the first rows are blank.
	const quiet = 4
	for _, index := range []int{0, 1, 2, 3} {
		require.Equal(t, strings.Repeat("0", size), rows[index], "the top margin is the quiet zone")
		require.Equal(t, strings.Repeat("0", size), rows[size-1-index], "and so is the bottom")
	}

	// The finder pattern is the square a reader locks onto, and a code without it
	// is one no phone scans. It is the first seven modules after the margin.
	require.Equal(t, "1111111", rows[quiet][quiet:quiet+7], "the top-left finder pattern is where a reader looks")
	require.Equal(t, "1000001", rows[quiet+1][quiet:quiet+7], "with its own ring below it")
	require.Equal(t, "1111111", rows[quiet+6][quiet:quiet+7], "and closed at the bottom")

	// The margin really is margin, not stray data.
	require.Equal(t, strings.Repeat("0", quiet), rows[quiet][:quiet])
}

// TestAnEmptyPayloadHasNoMatrix: a frame with no code draws nothing rather than an
// empty picture, which a screen would show as a broken image.
func TestAnEmptyPayloadHasNoMatrix(t *testing.T) {
	require.Nil(t, qrMatrix(""))
}

// TestTheMatrixCarriesThePayloadOnce is what makes the picture useful rather than
// merely present: two different payloads produce different pictures.
func TestTheMatrixCarriesADifferentPayloadEachTime(t *testing.T) {
	first := qrMatrix("2@code-one")
	second := qrMatrix("2@code-two")

	require.NotEmpty(t, first)
	require.NotEmpty(t, second)
	require.NotEqual(t, first, second, "a matrix that does not change with the payload is not carrying it")

	// And the same payload produces the same picture, so a screen that redraws does
	// not flash a different code at somebody who is midway through scanning.
	require.Equal(t, first, qrMatrix("2@code-one"))
}
