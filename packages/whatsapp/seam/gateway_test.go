package seam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The seam between the API and the gateway is this file.
//
// Two things are under test and both are about honesty rather than convenience:
// the gateway refuses a caller who is not ours (it speaks for every tenant it
// serves), and it refuses a call with no tenant (an operation that does not know
// whose number it acts on would act on anybody's).

// fakeTransport answers the gateway's calls without a WhatsApp connection.
type fakeTransport struct {
	link      whatsappdomain.Link
	linkErr   error
	sentID    string
	sendErr   error
	sentAs    whatsappdomain.Snapshot
	sent      whatsappdomain.ToSend
	cancelled string
	unlinked  uuid.UUID
	online    bool
}

func (f *fakeTransport) Link(_ context.Context, scope whatsappdomain.Scope, numberID uuid.UUID, phone string) (whatsappdomain.Link, error) {
	if f.linkErr != nil {
		return whatsappdomain.Link{}, f.linkErr
	}
	link := f.link
	link.NumberID = numberID
	link.WorkspaceID = scope.WorkspaceID
	link.Phone = phone
	return link, nil
}

func (f *fakeTransport) LinkState(_ context.Context, id string) (whatsappdomain.Link, error) {
	return whatsappdomain.Link{ID: id, State: whatsappdomain.LinkCode, Code: "qr-payload"}, nil
}

func (f *fakeTransport) PairCode(_ context.Context, id string, phone string) (whatsappdomain.Link, error) {
	return whatsappdomain.Link{ID: id, State: whatsappdomain.LinkCode, PairCode: "ABCD1234", Phone: phone}, nil
}

func (f *fakeTransport) Cancel(_ context.Context, id string) error {
	f.cancelled = id
	return nil
}

func (f *fakeTransport) Send(_ context.Context, snapshot whatsappdomain.Snapshot, message whatsappdomain.ToSend) (string, error) {
	f.sentAs = snapshot
	f.sent = message
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return f.sentID, nil
}

func (f *fakeTransport) Typing(context.Context, whatsappdomain.Snapshot, string) error { return nil }

func (f *fakeTransport) Unlink(_ context.Context, snapshot whatsappdomain.Snapshot) error {
	f.unlinked = snapshot.NumberID
	return nil
}

func (f *fakeTransport) Online(context.Context, uuid.UUID) (bool, error) { return f.online, nil }

// gatewayFixture is a running gateway with a scripted transport.
type gatewayFixture struct {
	server    *httptest.Server
	transport *fakeTransport
	token     string
	numberID  uuid.UUID
	workspace uuid.UUID
}

func newGatewayFixture(t *testing.T, transport *fakeTransport) *gatewayFixture {
	t.Helper()

	token := "test-gateway-token"
	gateway := NewGateway(transport, GatewayConfig{Token: token}, nil)

	mux := http.NewServeMux()
	gateway.Routes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &gatewayFixture{
		server:    server,
		transport: transport,
		token:     token,
		numberID:  uuid.New(),
		workspace: uuid.New(),
	}
}

// call performs one request against the gateway.
func (f *gatewayFixture) call(t *testing.T, method, path string, body any, token string, workspace uuid.UUID) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, reader)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set(TokenHeader, token)
	}
	if workspace != uuid.Nil {
		request.Header.Set("X-Workspace-Id", workspace.String())
	}

	response, err := f.server.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	payload, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, payload
}

// TestTheGatewayRefusesACallerWhoIsNotOurs: the gateway speaks for every tenant it
// serves, so a caller who reached it could link a number into any workspace. There
// is no read-only path worth leaving open.
func TestTheGatewayRefusesACallerWhoIsNotOurs(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{})

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no token at all", ""},
		{"the wrong token", "not-the-token"},
	} {
		status, _ := fixture.call(t, http.MethodPost,
			"/numbers/"+fixture.numberID.String()+"/link", map[string]any{"phone": "628123456789"},
			tc.token, fixture.workspace)
		require.Equal(t, http.StatusUnauthorized, status, tc.name)

		status, _ = fixture.call(t, http.MethodGet, "/numbers/"+fixture.numberID.String()+"/online", nil,
			tc.token, fixture.workspace)
		require.Equal(t, http.StatusUnauthorized, status, tc.name)
	}
}

// TestTheGatewayRefusesACallWithNoTenant: an operation that did not know whose
// number it acts on would act on anybody's.
func TestTheGatewayRefusesACallWithNoTenant(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{})

	status, body := fixture.call(t, http.MethodPost,
		"/numbers/"+fixture.numberID.String()+"/link", map[string]any{"phone": "628123456789"},
		fixture.token, uuid.Nil)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(body), "workspace")

	status, _ = fixture.call(t, http.MethodPost,
		"/numbers/"+fixture.numberID.String()+"/send", map[string]any{"to": "628999@s.whatsapp.net", "text": "halo"},
		fixture.token, uuid.Nil)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestAPathThatNamesNoNumberIsRefused: the id in the path is what decides which
// session speaks, so a malformed one is the caller's problem and is said so before
// anything is attempted.
func TestAPathThatNamesNoNumberIsRefused(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{})

	status, _ := fixture.call(t, http.MethodPost, "/numbers/not-a-uuid/link", map[string]any{"phone": "628123456789"},
		fixture.token, fixture.workspace)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestALinkCarriesTheTenantTheCallerNamed: what crosses the seam is the workspace
// the API resolved, and the gateway uses it rather than guessing.
func TestALinkCarriesTheTenantTheCallerNamed(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{link: whatsappdomain.Link{
		ID:    "link-1",
		State: whatsappdomain.LinkCode,
		Code:  "qr-payload",
	}})

	status, body := fixture.call(t, http.MethodPost,
		"/numbers/"+fixture.numberID.String()+"/link", map[string]any{"phone": "628123456789"},
		fixture.token, fixture.workspace)
	require.Equal(t, http.StatusOK, status)

	var answer struct {
		ID        string `json:"ID"`
		State     string `json:"State"`
		Code      string `json:"Code"`
		Number    string `json:"NumberID"`
		Phone     string `json:"Phone"`
		Workspace string `json:"WorkspaceID"`
	}
	require.NoError(t, json.Unmarshal(body, &answer))
	require.Equal(t, "link-1", answer.ID)
	require.Equal(t, whatsappdomain.LinkCode, answer.State)
	require.Equal(t, "qr-payload", answer.Code)
	require.Equal(t, fixture.numberID.String(), answer.Number)
	require.Equal(t, "628123456789", answer.Phone)
	require.Equal(t, fixture.workspace.String(), answer.Workspace, "the gateway acts for the tenant the API resolved")
}

// TestASendCarriesTheMessageAndTheNumber: the API hands over one queued message and
// the gateway answers WhatsApp's own id for it, which is what a history reads back.
func TestASendCarriesTheMessageAndTheNumber(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{sentID: "wa-message-1"})
	messageID := uuid.New()

	status, body := fixture.call(t, http.MethodPost, "/numbers/"+fixture.numberID.String()+"/send",
		map[string]any{
			"id":        messageID.String(),
			"to":        "628999@s.whatsapp.net",
			"kind":      whatsappdomain.KindText,
			"text":      "Halo Kak, pesanannya sudah dikirim.",
			"media_key": "",
		}, fixture.token, fixture.workspace)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(body), "wa-message-1")

	require.Equal(t, fixture.numberID, fixture.transport.sentAs.NumberID)
	require.Equal(t, fixture.workspace, fixture.transport.sentAs.WorkspaceID)
	require.Equal(t, messageID, fixture.transport.sent.ID)
	require.Equal(t, "628999@s.whatsapp.net", fixture.transport.sent.ToJID)
	require.Contains(t, fixture.transport.sent.Text, "dikirim")
}

// TestAnOfflineNumberIsAServiceProblem: nothing is wrong with the request, and the
// platform cannot carry it out. The API reads that as a queued message rather than
// as a failure, which is why the status matters.
func TestAnOfflineNumberIsAServiceProblem(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{sendErr: whatsappdomain.ErrNotConnected})

	status, body := fixture.call(t, http.MethodPost, "/numbers/"+fixture.numberID.String()+"/send",
		map[string]any{"to": "628999@s.whatsapp.net", "text": "halo"},
		fixture.token, fixture.workspace)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, string(body), "not connected")
}

// TestAnExpiredLinkIsGone: the answer tells a screen to ask for a new code rather
// than to show an error nobody can act on.
func TestAnExpiredLinkIsGone(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{linkErr: whatsappdomain.ErrLinkExpired})

	status, _ := fixture.call(t, http.MethodPost, "/numbers/"+fixture.numberID.String()+"/link",
		map[string]any{"phone": "628123456789"}, fixture.token, fixture.workspace)
	require.Equal(t, http.StatusGone, status)
}

// TestAnalreadyLinkedNumberIsAConflict: one number is one session, so a second
// link of the same number is refused with an answer a screen can explain.
func TestAnAlreadyLinkedNumberIsAConflict(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{linkErr: whatsappdomain.ErrAlreadyLinked})

	status, _ := fixture.call(t, http.MethodPost, "/numbers/"+fixture.numberID.String()+"/link",
		map[string]any{"phone": "628123456789"}, fixture.token, fixture.workspace)
	require.Equal(t, http.StatusConflict, status)
}

// TestAnUnexpectedFailureIsInternalAndSaysNothing: a transport error is the
// gateway's business, not the caller's, so what crosses the seam is one sentence
// rather than whatever the library said.
func TestAnUnexpectedFailureIsInternalAndSaysNothing(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{
		sendErr: errors.New("whatsapp: send: handshake failed with a very long stack of detail"),
	})

	status, body := fixture.call(t, http.MethodPost, "/numbers/"+fixture.numberID.String()+"/send",
		map[string]any{"to": "628999@s.whatsapp.net", "text": "halo"},
		fixture.token, fixture.workspace)
	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, string(body), "handshake", "the detail stays in the log")
}

// TestCancelAndUnlinkReachTheTransport: the two ways a session ends are routed.
func TestCancelAndUnlinkReachTheTransport(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{})

	status, _ := fixture.call(t, http.MethodDelete, "/links/link-7", nil, fixture.token, uuid.Nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "link-7", fixture.transport.cancelled)

	status, _ = fixture.call(t, http.MethodPost, "/numbers/"+fixture.numberID.String()+"/unlink", nil,
		fixture.token, fixture.workspace)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, fixture.numberID, fixture.transport.unlinked)
}

// TestOnlineAnswersWhatTheTransportKnows: the number's liveness is one fact, and
// the gateway is the only thing that has it.
func TestOnlineAnswersWhatTheTransportKnows(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{online: true})

	status, body := fixture.call(t, http.MethodGet, "/numbers/"+fixture.numberID.String()+"/online", nil,
		fixture.token, uuid.Nil)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(body), "true")
}

// TestHealthDoesNotClaimANumberIsOnline: liveness of the process is not liveness
// of a number, and conflating them would let a deployment report a healthy gateway
// with every number offline.
func TestHealthDoesNotClaimANumberIsOnline(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{})

	status, body := fixture.call(t, http.MethodGet, "/health", nil, "", uuid.Nil)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(body), "ok")
	require.NotContains(t, string(body), "online")
}

// TestTheClientSpeaksTheGateway: the client is the other half of this seam, and it
// is exercised against the real server rather than a stub, because what could be
// wrong is exactly the encoding between them.
func TestTheClientSpeaksTheGateway(t *testing.T) {
	fixture := newGatewayFixture(t, &fakeTransport{
		link:   whatsappdomain.Link{ID: "link-1", State: whatsappdomain.LinkWaiting},
		sentID: "wa-message-9",
	})
	client := NewClient(fixture.server.URL, fixture.token, 5*time.Second)

	ctx := t.Context()
	scope := whatsappdomain.Scope{WorkspaceID: fixture.workspace}

	link, err := client.Link(ctx, scope, fixture.numberID, "628123456789")
	require.NoError(t, err)
	require.Equal(t, "link-1", link.ID)
	require.Equal(t, fixture.workspace, link.WorkspaceID)

	state, err := client.LinkState(ctx, link.ID)
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.LinkCode, state.State)

	_, err = client.PairCode(ctx, link.ID, "628123456789")
	require.NoError(t, err)

	sentID, err := client.Send(ctx, whatsappdomain.Snapshot{
		NumberID:    fixture.numberID,
		WorkspaceID: fixture.workspace,
	}, whatsappdomain.ToSend{
		ID:    uuid.New(),
		ToJID: "628999@s.whatsapp.net",
		Kind:  whatsappdomain.KindText,
		Text:  "halo",
	})
	require.NoError(t, err)
	require.Equal(t, "wa-message-9", sentID)

	online, err := client.Online(ctx, fixture.numberID)
	require.NoError(t, err)
	require.False(t, online)

	require.NoError(t, client.Cancel(ctx, link.ID))
	require.NoError(t, client.Unlink(ctx, whatsappdomain.Snapshot{NumberID: fixture.numberID, WorkspaceID: fixture.workspace}))
}

// TestTheClientRefusesToGuessAnAddress: an unconfigured client answers with the
// fact rather than reaching a URL nobody meant. That is what lets the API run with
// no gateway: the module's reads work and its writes say what is missing.
func TestTheClientRefusesToGuessAnAddress(t *testing.T) {
	client := NewClient("", "", time.Second)

	_, err := client.Link(t.Context(), whatsappdomain.Scope{WorkspaceID: uuid.New()}, uuid.New(), "628123456789")
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)

	_, err = client.Send(t.Context(), whatsappdomain.Snapshot{NumberID: uuid.New()}, whatsappdomain.ToSend{})
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)
}

// TestTheClientKeepsTheGatewaysClassification is the fourth defect a browser found,
// and it is the same class as the third: the seam threw the classification away. A 404
// from the gateway means "no such number", and flattening it into a string made the API
// answer 500 for a number WhatsApp does not recognise.
func TestTheClientKeepsTheGatewaysClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"a row that is not there", http.StatusNotFound, whatsappdomain.ErrNotFound},
		{"a request that cannot be acted on", http.StatusBadRequest, whatsappdomain.ErrInvalidInput},
		{"a state that refuses it", http.StatusConflict, whatsappdomain.ErrInvalidInput},
		{"an expired attempt", http.StatusGone, whatsappdomain.ErrLinkExpired},
		{"a service that is down", http.StatusServiceUnavailable, whatsappdomain.ErrUnavailable},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, tc.status, map[string]any{"error": "whatsapp: not found: WhatsApp tidak mengenal nomor ••••6789"})
		}))
		t.Cleanup(server.Close)

		client := NewClient(server.URL, "token", 5*time.Second)
		_, err := client.Link(t.Context(), whatsappdomain.Scope{WorkspaceID: uuid.New()}, uuid.New(), "628123456789")

		require.ErrorIs(t, err, tc.want, tc.name)
		require.Contains(t, err.Error(), "tidak mengenal", tc.name, "the sentence the other end wrote is carried through")
	}

	// A status nobody mapped is an unexpected failure rather than a category, and it
	// must not be mistaken for one of them.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL, "token", 5*time.Second)
	_, err := client.Link(t.Context(), whatsappdomain.Scope{WorkspaceID: uuid.New()}, uuid.New(), "628123456789")
	require.Error(t, err)
	require.NotErrorIs(t, err, whatsappdomain.ErrNotFound)
	require.NotErrorIs(t, err, whatsappdomain.ErrInvalidInput)
}

// The status table is the seam's one vocabulary: the product writes it and the
// client reads it back. A sentinel the table does not classify crosses as a 500,
// which is a lie — it tells the gateway to retry something no retry will fix, and
// it pages whoever watches the gateway for a condition the product already knows
// how to name.
func TestTheStatusTableClassifiesEveryCrossableSentinel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		reverse error
	}{
		{"a row that is not here", whatsappdomain.ErrNotFound, http.StatusNotFound, whatsappdomain.ErrNotFound},
		{"a request that cannot be acted on", whatsappdomain.ErrInvalidInput, http.StatusBadRequest, whatsappdomain.ErrInvalidInput},
		{"a warning not yet accepted", whatsappdomain.ErrTermsNotAccepted, http.StatusBadRequest, whatsappdomain.ErrInvalidInput},
		{"a number already linked", whatsappdomain.ErrAlreadyLinked, http.StatusConflict, whatsappdomain.ErrInvalidInput},
		{"a number nobody answers yet", whatsappdomain.ErrNoAgent, http.StatusConflict, whatsappdomain.ErrInvalidInput},
		{"a number that is not connected", whatsappdomain.ErrNotConnected, http.StatusServiceUnavailable, whatsappdomain.ErrUnavailable},
		{"WhatsApp that is not reachable", whatsappdomain.ErrUnavailable, http.StatusServiceUnavailable, whatsappdomain.ErrUnavailable},
		{"an attempt that expired", whatsappdomain.ErrLinkExpired, http.StatusGone, whatsappdomain.ErrLinkExpired},
	} {
		// The product wraps its sentinels with the sentence a person reads, so the
		// table is asked about the wrapped error rather than the bare one.
		wrapped := fmt.Errorf("%w: nomor ini belum punya Bolu yang menjawab", tc.err)

		require.Equal(t, tc.status, StatusFor(wrapped), tc.name)
		require.ErrorIs(t, SentinelForStatus(tc.status), tc.reverse, tc.name+": the client reverses the table")
	}

	// A failure nobody classified stays a 500 and does not leak its sentence: the
	// gateway's own business is not something to hand across the seam.
	unclassified := errors.New("whatsapp: the database is on fire")
	require.Equal(t, http.StatusInternalServerError, StatusFor(unclassified))
	require.Equal(t, "internal error", Sentence(unclassified))
}
