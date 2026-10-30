package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anakbolu/timbolu/apps/backend/internal/platform/oauth/google"
)

// googleStub stands in for Google's endpoints, so the sign-in flow can be driven
// end to end without leaving the machine.
type googleStub struct {
	server *httptest.Server
	email  string
}

func newGoogleStub(t *testing.T, email string) *googleStub {
	t.Helper()

	stub := &googleStub{email: email}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stub-access-token","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":            "google-subject-1",
			"email":          stub.email,
			"email_verified": true,
		})
	})

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)

	return stub
}

// client points the real Google client at the stub.
func (s *googleStub) client(t *testing.T) *google.Client {
	t.Helper()

	client, err := google.New(google.Config{
		ClientID:     "stub-client-id",
		ClientSecret: "stub-client-secret",
		RedirectURL:  "http://localhost/api/auth/google/callback",
		AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     s.server.URL + "/token",
		UserInfoURL:  s.server.URL + "/userinfo",
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("build google client: %v", err)
	}
	return client
}
