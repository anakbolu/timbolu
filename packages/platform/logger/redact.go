package logger

import (
	"net/http"
	"net/url"
	"strings"
)

// Redacted is the placeholder written instead of a sensitive value.
const Redacted = "[REDACTED]"

// sensitiveFragments are matched against the normalised key (lowercase, without
// separators). Matching is intentionally greedy: over-redacting a log line is
// harmless, leaking a token is not.
var sensitiveFragments = []string{
	"authorization",
	"cookie",
	"password",
	"passwd",
	"secret",
	"token",
	"apikey",
	"credential",
	"privatekey",
	"session",
	"signature",
}

// IsSensitiveKey reports whether a header or parameter name must be redacted.
func IsSensitiveKey(key string) bool {
	normalised := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(key))
	if normalised == "" {
		return false
	}
	for _, fragment := range sensitiveFragments {
		if strings.Contains(normalised, fragment) {
			return true
		}
	}
	return false
}

// RedactValue returns the placeholder for a non-empty value, and an empty
// string for an empty one so absent headers stay absent.
func RedactValue(value string) string {
	if value == "" {
		return ""
	}
	return Redacted
}

// RedactHeaders returns a copy of the headers with sensitive values replaced.
// It is used when a request line is logged at debug level.
func RedactHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		joined := strings.Join(values, ", ")
		if IsSensitiveKey(key) {
			out[key] = RedactValue(joined)
			continue
		}
		out[key] = joined
	}
	return out
}

// RedactQuery replaces the value of every sensitive query parameter.
func RedactQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}

	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		// An unparsable query string could hide anything: do not log it.
		return Redacted
	}

	for key := range values {
		if IsSensitiveKey(key) {
			values[key] = []string{Redacted}
		}
	}

	return values.Encode()
}
