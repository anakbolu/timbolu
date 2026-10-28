package service

import (
	"archive/zip"
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// zipEntry and zipOf build a zip in memory, entries in the order they are given.
// An archive is the only way to reach the checks that look inside an OOXML upload,
// so the tests write the container a client would have sent.
type zipEntry struct {
	name    string
	content []byte
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		file, err := writer.Create(entry.name)
		require.NoError(t, err)
		_, err = file.Write(entry.content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	return buffer.Bytes()
}

// ooxmlOf builds the package a real Word, Excel, or PowerPoint file is: the part
// list that declares the document's parts, and one of those parts. Without the
// part list a zip is just a zip, whatever it is named.
func ooxmlOf(t *testing.T, part string) []byte {
	t.Helper()

	return zipOf(t,
		zipEntry{name: ooxmlContentTypes, content: []byte(`<?xml version="1.0"?><Types/>`)},
		zipEntry{name: part, content: []byte("<document/>")},
	)
}

// oleOf builds the bytes a legacy Office document opens with: the compound-file
// signature and the padding a real header carries. Nothing reads past it here,
// which is the point — the signature is what tells the container from noise.
func oleOf() []byte {
	content := make([]byte, 512)
	copy(content, oleHeader)
	return content
}

// unknownBytes is 512 bytes that sniff as neither text nor any signature the
// sniffer knows: the control bytes make it binary, and it opens with neither an
// executable magic nor an OLE header. It is what `random.doc` looked like when
// the octet-stream label let it through.
func unknownBytes() []byte {
	content := make([]byte, 512)
	for index := range content {
		content[index] = byte(index*37 + 0x13)
	}
	return content
}

// TestAnOfficeUploadIsCheckedInsideTheContainer: the extension and the sniffed
// type agree on "some zip" and on "some octet stream", which is all a sniffer can
// say — and all an archive of executables and 512 random bytes needed to pass.
// What is stored has to be the document the extension promises, so the container
// is opened and the answer is the type from the extension table or a refusal.
func TestAnOfficeUploadIsCheckedInsideTheContainer(t *testing.T) {
	tests := map[string]struct {
		filename    string
		content     []byte
		contentType string
		reason      string
	}{
		"a docx package": {
			filename:    "surat.docx",
			content:     ooxmlOf(t, "word/document.xml"),
			contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		},
		"an xlsx package": {
			filename:    "laporan.xlsx",
			content:     ooxmlOf(t, "xl/workbook.xml"),
			contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		},
		"a pptx package": {
			filename:    "paparan.pptx",
			content:     ooxmlOf(t, "ppt/presentation.xml"),
			contentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		},
		"a legacy doc": {
			filename:    "lama.doc",
			content:     oleOf(),
			contentType: "application/msword",
		},
		"a legacy xls": {
			filename:    "lama.xls",
			content:     oleOf(),
			contentType: "application/vnd.ms-excel",
		},
		"a legacy ppt": {
			filename:    "lama.ppt",
			content:     oleOf(),
			contentType: "application/vnd.ms-powerpoint",
		},
		"a zip holding an executable entry": {
			filename: "x.docx",
			content: zipOf(t,
				zipEntry{name: ooxmlContentTypes, content: []byte("<Types/>")},
				zipEntry{name: "evil.exe", content: []byte("MZ\x90\x00")},
			),
			reason: `holds an executable entry "evil.exe"`,
		},
		"a zip holding a batch entry": {
			filename: "x.xlsx",
			content: zipOf(t,
				zipEntry{name: ooxmlContentTypes, content: []byte("<Types/>")},
				zipEntry{name: "payload.bat", content: []byte("@echo off\n")},
			),
			reason: `holds an executable entry "payload.bat"`,
		},
		"a zip with no part list": {
			filename: "x.docx",
			content:  zipOf(t, zipEntry{name: "word/document.xml", content: []byte("<document/>")}),
			reason:   "is not an Office package",
		},
		"a zip with an entry named only for its dot": {
			filename: "x.docx",
			content: zipOf(t,
				zipEntry{name: ooxmlContentTypes, content: []byte("<Types/>")},
				zipEntry{name: "part.", content: []byte("x")},
			),
			reason: "holds an entry with no usable name",
		},
		"512 random bytes named doc": {
			filename: "random.doc",
			content:  unknownBytes(),
			reason:   "the content of .doc is not a compound Office file",
		},
		"a png renamed to doc": {
			filename: "gambar.doc",
			content:  pngOf("foto"),
			reason:   "the content of .doc is not a application/msword file",
		},
		"an executable named docx": {
			filename: "x.docx",
			content:  exeOf("payload"),
			reason:   "the content is an executable",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			contentType, err := validateAttachment(test.filename, test.content, defaultAttachmentExtensions)

			if test.reason != "" {
				require.ErrorIs(t, err, chatdomain.ErrFileTypeNotAllowed)
				require.ErrorContains(t, err, test.reason)
				require.Empty(t, contentType)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.contentType, contentType,
				"the stored type comes from the extension, never from the container")
		})
	}
}

// TestAnOOXMLZipAboveTheEntryCapIsRefused: an office file holds tens of entries,
// so an archive declaring thousands is not one — and walking its names is work an
// upload does not get to ask for. The cap is checked before any name is read.
func TestAnOOXMLZipAboveTheEntryCapIsRefused(t *testing.T) {
	entries := []zipEntry{{name: ooxmlContentTypes, content: []byte("<Types/>")}}
	for index := 0; index <= maxOOXMLEntries; index++ {
		entries = append(entries, zipEntry{
			name:    fmt.Sprintf("word/part%d.xml", index),
			content: []byte("<part/>"),
		})
	}

	_, err := validateAttachment("x.docx", zipOf(t, entries...), defaultAttachmentExtensions)
	require.ErrorIs(t, err, chatdomain.ErrFileTypeNotAllowed)
	require.ErrorContains(t, err, fmt.Sprintf("more than the %d an office file has", maxOOXMLEntries))
}