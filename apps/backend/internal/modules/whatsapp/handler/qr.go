package handler

import (
	qrcode "github.com/skip2/go-qrcode"
)

// The QR code a phone scans is rendered here rather than in the browser.
//
// Two reasons. The payload is a capability — whoever scans it links the number —
// so it must reach the person who started the attempt and nobody else, and the
// frame it travels in is already the authenticated per-attempt socket. And the
// browser has no QR encoder: rendering it client-side would mean a second
// dependency for something the server can do in a dozen lines.
//
// What crosses the wire is the matrix rather than an image: a list of rows of "1"
// and "0", which the screen draws. An SVG string would work too, but handing
// markup to a renderer is how a capability becomes an injection.

// qrMatrix renders a QR code as rows of bits, or nil when the payload cannot be
// encoded.
//
// The recovery level is low because the payload is a fixed-size protocol string
// and the code is shown on a screen rather than printed: a higher level would make
// the modules larger than the picture needs and slow a phone's read of it.
func qrMatrix(payload string) []string {
	if payload == "" {
		return nil
	}

	code, err := qrcode.New(payload, qrcode.Low)
	if err != nil {
		// An unencodable payload is a protocol problem rather than a user's, and
		// the attempt is reported by its state: answering with no matrix means the
		// screen offers the phone-code alternative instead of a broken picture.
		return nil
	}

	bitmap := code.Bitmap()
	rows := make([]string, 0, len(bitmap))
	for _, row := range bitmap {
		line := make([]byte, 0, len(row))
		for _, dark := range row {
			if dark {
				line = append(line, '1')
			} else {
				line = append(line, '0')
			}
		}
		rows = append(rows, string(line))
	}

	return rows
}
