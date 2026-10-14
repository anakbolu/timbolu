package logger

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newBuffered builds a JSON logger that writes into buf, so tests can assert on
// the exact bytes a production log line would contain.
func newBuffered(buf *bytes.Buffer, level zapcore.Level) *zap.Logger {
	encoder := zapcore.NewJSONEncoder(jsonEncoderConfig())
	return zap.New(zapcore.NewCore(encoder, zapcore.AddSync(buf), level))
}

func TestNewRejectsUnknownSettings(t *testing.T) {
	if _, err := New("loud", FormatJSON); err == nil {
		t.Error("expected an error for an unknown level")
	}
	if _, err := New("info", "xml"); err == nil {
		t.Error("expected an error for an unknown format")
	}
}

func TestNewProducesStructuredJSON(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		for _, format := range []string{FormatJSON, FormatConsole, ""} {
			log, err := New(level, format)
			if err != nil {
				t.Fatalf("New(%q, %q): %v", level, format, err)
			}
			if log == nil {
				t.Fatalf("New(%q, %q) returned no logger", level, format)
			}
		}
	}
}

func TestJSONEncoderFields(t *testing.T) {
	var buf bytes.Buffer
	log := newBuffered(&buf, zapcore.InfoLevel)

	log.Info("request", zap.String("request_id", "abc"), zap.String("workspace_id", "ws-1"))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}

	if line["msg"] != "request" {
		t.Errorf("unexpected msg %v", line["msg"])
	}
	if line["level"] != "info" {
		t.Errorf("unexpected level %v", line["level"])
	}
	if line["request_id"] != "abc" || line["workspace_id"] != "ws-1" {
		t.Errorf("context fields missing from the line: %v", line)
	}
	if _, ok := line["ts"]; !ok {
		t.Error("the line must carry a timestamp")
	}
}

// TestDefaultWriterWritesToStdout covers the production writer path: New is
// built without a sink, so the record must reach the process stdout.
func TestDefaultWriterWritesToStdout(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	original := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	log, err := New("info", FormatJSON)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("hello stdout")
	if err := log.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if !bytes.Contains(content, []byte("hello stdout")) {
		t.Fatalf("the record did not reach stdout: %s", content)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := newBuffered(&buf, zapcore.WarnLevel)

	log.Info("hidden")
	log.Warn("visible")

	if buf.Len() == 0 {
		t.Fatal("expected the warn record to be written")
	}
	if bytes.Contains(buf.Bytes(), []byte("hidden")) {
		t.Fatalf("info record leaked into a warn-level logger: %s", buf.String())
	}
}
