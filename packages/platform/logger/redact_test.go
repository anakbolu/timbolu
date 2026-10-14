package logger

import (
	"net/http"
	"net/url"
	"testing"
)

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"Authorization",
		"authorization",
		"Cookie",
		"Set-Cookie",
		"X-Api-Key",
		"api_key",
		"X-Auth-Token",
		"password",
		"refresh_token",
		"X-Signature",
		"X-Amz-Credential",
		"Session-Id",
		"private-key",
	}
	for _, key := range sensitive {
		if !IsSensitiveKey(key) {
			t.Errorf("expected %q to be sensitive", key)
		}
	}

	harmless := []string{"Accept", "Content-Type", "User-Agent", "X-Request-Id", "X-Trace-Id", ""}
	for _, key := range harmless {
		if IsSensitiveKey(key) {
			t.Errorf("expected %q to be harmless", key)
		}
	}
}

func TestRedactHeaders(t *testing.T) {
	headers := http.Header{
		"Authorization": {"Bearer super-secret"},
		"Content-Type":  {"application/json"},
		"X-Api-Key":     {"key-123"},
		"X-Request-Id":  {"abc"},
	}

	redacted := RedactHeaders(headers)

	if redacted["Authorization"] != Redacted {
		t.Errorf("Authorization must be redacted, got %q", redacted["Authorization"])
	}
	if redacted["X-Api-Key"] != Redacted {
		t.Errorf("X-Api-Key must be redacted, got %q", redacted["X-Api-Key"])
	}
	if redacted["Content-Type"] != "application/json" {
		t.Errorf("Content-Type must survive, got %q", redacted["Content-Type"])
	}
	if redacted["X-Request-Id"] != "abc" {
		t.Errorf("X-Request-Id must survive, got %q", redacted["X-Request-Id"])
	}

	// The original map must not be modified: the request still needs its headers.
	if headers.Get("Authorization") != "Bearer super-secret" {
		t.Error("RedactHeaders modified the input headers")
	}
}

func TestRedactValue(t *testing.T) {
	if got := RedactValue(""); got != "" {
		t.Errorf("an absent value must stay absent, got %q", got)
	}
	if got := RedactValue("token"); got != Redacted {
		t.Errorf("expected %q, got %q", Redacted, got)
	}
}

func TestRedactQuery(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantAbsent []string
		wantKeep   string
	}{
		{
			name:       "token parameter is replaced",
			query:      "token=abc123&page=2",
			wantAbsent: []string{"abc123"},
			wantKeep:   "page=2",
		},
		{
			name:       "password and api key are replaced",
			query:      "password=hunter2&api_key=k9",
			wantAbsent: []string{"hunter2", "k9"},
		},
		{
			name:     "unparsable query is dropped entirely",
			query:    "%zz=1&token=abc",
			wantKeep: Redacted,
		},
		{
			name:     "empty query stays empty",
			query:    "",
			wantKeep: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactQuery(tt.query)

			for _, secret := range tt.wantAbsent {
				if contains(got, secret) {
					t.Fatalf("redacted query %q still contains %q", got, secret)
				}
			}
			if tt.wantKeep != "" && !contains(got, tt.wantKeep) {
				t.Fatalf("redacted query %q does not contain %q", got, tt.wantKeep)
			}
		})
	}
}

// TestRedactQueryKeepsItParsable proves the redacted value survives encoding, so
// a log line stays readable.
func TestRedactQueryKeepsItParsable(t *testing.T) {
	got := RedactQuery("token=a b&page=2")

	values, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("redacted query is not parsable: %v", err)
	}
	if values.Get("token") != Redacted {
		t.Fatalf("expected the token to be replaced, got %q", values.Get("token"))
	}
	if values.Get("page") != "2" {
		t.Fatalf("expected page to survive, got %q", values.Get("page"))
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
