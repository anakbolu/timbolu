package seam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The second half of the seam is this file: what the gateway asks the product to
// do. Two things are under test and both are about the contract rather than the
// plumbing — the product refuses a caller who is not ours and a call with no
// tenant, and the client keeps the product's own classification of a failure so
// the engine can act on it (retry a 503, drop a 404).

const testToken = "shared-secret"

// fakeProduct answers the product surface without a database. One struct carries
// all four ports so a test can hand it to ProductDeps whole.
type fakeProduct struct {
	received     whatsappdomain.Inbound
	receivedNum  uuid.UUID
	receivedScop whatsappdomain.Scope
	receiveErr   error
	created      bool

	pendingLimit int
	pending      []whatsappdomain.Inbound
	pendingErr   error

	storedID    uuid.UUID
	storedMedia whatsappdomain.Media
	storeErr    error

	claimLimit int
	claimNum   uuid.UUID
	claimed    whatsappdomain.Claimed
	claimErr   error

	reclaimOlder time.Time
	reclaimed    int
	reclaimErr   error

	sentID  uuid.UUID
	sentWa  string
	sentAt  time.Time
	sentErr error

	retryID     uuid.UUID
	retryReason string
	retryAt     time.Time
	retryErr    error

	failedID       uuid.UUID
	failedReason   string
	failedAttempts int
	failedErr      error

	number    whatsappdomain.Number
	numberErr error

	report    whatsappdomain.ReportRequest
	reportErr error

	paired    whatsappdomain.PairedLink
	pairedErr error
}

func (f *fakeProduct) Receive(_ context.Context, scope whatsappdomain.Scope, numberID uuid.UUID, message whatsappdomain.Inbound) (whatsappdomain.Inbound, bool, error) {
	f.receivedScop, f.receivedNum, f.received = scope, numberID, message
	if f.receiveErr != nil {
		return whatsappdomain.Inbound{}, false, f.receiveErr
	}
	stored := message
	stored.ID = message.ID
	return stored, f.created, nil
}

func (f *fakeProduct) PendingMedia(_ context.Context, _ whatsappdomain.Scope, limit int) ([]whatsappdomain.Inbound, error) {
	f.pendingLimit = limit
	return f.pending, f.pendingErr
}

func (f *fakeProduct) StoreMedia(_ context.Context, _ whatsappdomain.Scope, id uuid.UUID, media whatsappdomain.Media) error {
	f.storedID, f.storedMedia = id, media
	return f.storeErr
}

func (f *fakeProduct) Claim(_ context.Context, _ whatsappdomain.Scope, numberID uuid.UUID, limit int) (whatsappdomain.Claimed, error) {
	f.claimNum, f.claimLimit = numberID, limit
	return f.claimed, f.claimErr
}

func (f *fakeProduct) Sent(_ context.Context, _ whatsappdomain.Scope, id uuid.UUID, waMessageID string, at time.Time) error {
	f.sentID, f.sentWa, f.sentAt = id, waMessageID, at
	return f.sentErr
}

func (f *fakeProduct) Retry(_ context.Context, _ whatsappdomain.Scope, id uuid.UUID, reason string, at time.Time) error {
	f.retryID, f.retryReason, f.retryAt = id, reason, at
	return f.retryErr
}

func (f *fakeProduct) Failed(_ context.Context, _ whatsappdomain.Scope, id uuid.UUID, reason string, attempts int) error {
	f.failedID, f.failedReason, f.failedAttempts = id, reason, attempts
	return f.failedErr
}

func (f *fakeProduct) Reclaim(_ context.Context, _ whatsappdomain.Scope, olderThan time.Time) (int, error) {
	f.reclaimOlder = olderThan
	return f.reclaimed, f.reclaimErr
}

func (f *fakeProduct) Report(_ context.Context, _ whatsappdomain.Scope, request whatsappdomain.ReportRequest) (whatsappdomain.Number, error) {
	f.report = request
	if f.reportErr != nil {
		return whatsappdomain.Number{}, f.reportErr
	}
	number := f.number
	number.ID = request.NumberID
	number.Status = request.Status
	return number, nil
}

func (f *fakeProduct) Number(_ context.Context, _ whatsappdomain.Scope, id uuid.UUID) (whatsappdomain.Number, error) {
	if f.numberErr != nil {
		return whatsappdomain.Number{}, f.numberErr
	}
	number := f.number
	number.ID = id
	return number, nil
}

func (f *fakeProduct) Paired(_ context.Context, _ whatsappdomain.Scope, paired whatsappdomain.PairedLink) error {
	f.paired = paired
	return f.pairedErr
}

// serveProduct stands the product up on a real listener and returns the client
// the gateway holds, so a test drives the whole seam rather than one half of it.
func serveProduct(t *testing.T, fake *fakeProduct) *ProductClient {
	t.Helper()

	product := NewProduct(ProductDeps{Inbox: fake, Drain: fake, Status: fake, Links: fake},
		ProductConfig{Token: testToken}, nil)
	mux := http.NewServeMux()
	product.Routes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return NewProductClient(server.URL, testToken, 5*time.Second)
}

func workspace() whatsappdomain.Scope {
	return whatsappdomain.Scope{WorkspaceID: uuid.New()}
}

// --------------------------------------------------------------------------
// The server's refusals.
// --------------------------------------------------------------------------

func TestTheProductRefusesACallerWhoIsNotOurs(t *testing.T) {
	fake := &fakeProduct{}
	client := NewProductClient(serverURL(t, fake), "wrong-token", 5*time.Second)

	_, _, err := client.Receive(context.Background(), workspace(), uuid.New(), whatsappdomain.Inbound{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized")
}

func TestTheProductRefusesACallWithNoTenant(t *testing.T) {
	fake := &fakeProduct{}
	client := serveProduct(t, fake)

	_, _, err := client.Receive(context.Background(), whatsappdomain.Scope{}, uuid.New(), whatsappdomain.Inbound{})
	require.Error(t, err)
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)
}

func TestTheProductRefusesANumberThatIsNotAUUID(t *testing.T) {
	fake := &fakeProduct{}
	server := serverURL(t, fake)

	response := rawCall(t, server, http.MethodPost, "/internal/whatsapp/numbers/not-a-uuid/inbound", testToken, uuid.New().String(), `{}`)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

func TestTheProductRefusesABodyThatIsNotJSON(t *testing.T) {
	fake := &fakeProduct{}
	server := serverURL(t, fake)

	response := rawCall(t, server, http.MethodPost, "/internal/whatsapp/numbers/"+uuid.New().String()+"/inbound", testToken, uuid.New().String(), `{not json`)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

func TestTheProductRefusesAWorkspaceThatIsNotAUUID(t *testing.T) {
	fake := &fakeProduct{}
	server := serverURL(t, fake)

	response := rawCall(t, server, http.MethodPost, "/internal/whatsapp/numbers/"+uuid.New().String()+"/inbound", testToken, "not-a-uuid", `{}`)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

func TestTheProductRefusesAnEmptyTokenConfiguration(t *testing.T) {
	// A deployment with no secret set refuses every call rather than serving the
	// fleet to whoever can reach the address.
	fake := &fakeProduct{}
	product := NewProduct(ProductDeps{Inbox: fake, Drain: fake, Status: fake, Links: fake}, ProductConfig{}, nil)
	mux := http.NewServeMux()
	product.Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	response := rawCall(t, server.URL, http.MethodPost, "/internal/whatsapp/numbers/"+uuid.New().String()+"/inbound", "", uuid.New().String(), `{}`)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

// --------------------------------------------------------------------------
// The client reaches every port, and the product acts on what it sent.
// --------------------------------------------------------------------------

func TestReceiveRecordsWhatArrived(t *testing.T) {
	fake := &fakeProduct{created: true}
	client := serveProduct(t, fake)

	scope := workspace()
	numberID := uuid.New()
	message := whatsappdomain.Inbound{WaMessageID: "wamid.1", ChatJID: "628@c.us", Text: "hi", Kind: whatsappdomain.KindText}

	stored, created, err := client.Receive(context.Background(), scope, numberID, message)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "wamid.1", stored.WaMessageID)
	require.Equal(t, scope.WorkspaceID, fake.receivedScop.WorkspaceID)
	require.Equal(t, numberID, fake.receivedNum)
	require.Equal(t, "hi", fake.received.Text)
}

func TestPendingMediaAsksForTheMessagesThatNeedBytes(t *testing.T) {
	fake := &fakeProduct{pending: []whatsappdomain.Inbound{
		{WaMessageID: "a", Kind: whatsappdomain.KindImage, Media: whatsappdomain.Media{Ref: "ref-a", Kind: whatsappdomain.KindImage}},
		{WaMessageID: "b", Kind: whatsappdomain.KindAudio},
	}}
	client := serveProduct(t, fake)

	pending, err := client.PendingMedia(context.Background(), workspace(), 25)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.Equal(t, 25, fake.pendingLimit)
	require.Equal(t, "ref-a", pending[0].Media.Ref)
	require.Equal(t, whatsappdomain.KindAudio, pending[1].Kind)
}

func TestStoreMediaRecordsWhereTheBytesWent(t *testing.T) {
	fake := &fakeProduct{}
	client := serveProduct(t, fake)

	messageID := uuid.New()
	media := whatsappdomain.Media{Kind: whatsappdomain.KindImage, Status: whatsappdomain.MediaReady, Key: "whatsapp/x/y/image.jpg"}
	require.NoError(t, client.StoreMedia(context.Background(), workspace(), messageID, media))

	require.Equal(t, messageID, fake.storedID)
	require.Equal(t, "whatsapp/x/y/image.jpg", fake.storedMedia.Key)
}

func TestClaimReturnsWhatMayGoOutAndWhatWasHeld(t *testing.T) {
	numberID := uuid.New()
	fake := &fakeProduct{claimed: whatsappdomain.Claimed{
		Ready: []whatsappdomain.Outbox{{ID: uuid.New(), ToJID: "628@c.us", Text: "hello"}},
		Held:  3,
	}}
	client := serveProduct(t, fake)

	claimed, err := client.Claim(context.Background(), workspace(), numberID, 10)
	require.NoError(t, err)
	require.Len(t, claimed.Ready, 1)
	require.Equal(t, 3, claimed.Held)
	require.Equal(t, numberID, fake.claimNum)
	require.Equal(t, 10, fake.claimLimit)
}

func TestSentRetryAndFailedReachTheQueue(t *testing.T) {
	fake := &fakeProduct{}
	client := serveProduct(t, fake)
	scope := workspace()

	messageID := uuid.New()
	at := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, client.Sent(context.Background(), scope, messageID, "wamid.out", at))
	require.Equal(t, messageID, fake.sentID)
	require.Equal(t, "wamid.out", fake.sentWa)
	require.True(t, fake.sentAt.Equal(at))

	require.NoError(t, client.Retry(context.Background(), scope, messageID, "provider blip", at))
	require.Equal(t, "provider blip", fake.retryReason)
	require.True(t, fake.retryAt.Equal(at))

	require.NoError(t, client.Failed(context.Background(), scope, messageID, "no route", 4))
	require.Equal(t, "no route", fake.failedReason)
	require.Equal(t, 4, fake.failedAttempts)
}

func TestReclaimAnswersHowManyWentBack(t *testing.T) {
	fake := &fakeProduct{reclaimed: 2}
	client := serveProduct(t, fake)

	older := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	reclaimed, err := client.Reclaim(context.Background(), workspace(), older)
	require.NoError(t, err)
	require.Equal(t, 2, reclaimed)
	require.True(t, fake.reclaimOlder.Equal(older))
}

func TestNumberAndReportReadAndWriteTheNumber(t *testing.T) {
	numberID := uuid.New()
	fake := &fakeProduct{number: whatsappdomain.Number{Phone: "628123", HourlyLimit: 60}}
	client := serveProduct(t, fake)
	scope := workspace()

	number, err := client.Number(context.Background(), scope, numberID)
	require.NoError(t, err)
	require.Equal(t, numberID, number.ID)
	require.Equal(t, "628123", number.Phone)

	at := time.Now().UTC().Truncate(time.Second)
	reported, err := client.Report(context.Background(), scope, whatsappdomain.ReportRequest{
		NumberID: numberID,
		Status:   whatsappdomain.StatusConnected,
		Reason:   "paired",
		At:       at,
	})
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.StatusConnected, reported.Status)
	require.Equal(t, numberID, fake.report.NumberID)
	require.Equal(t, "paired", fake.report.Reason)
}

func TestPairedRecordsWhoTheNumberAnswersAs(t *testing.T) {
	fake := &fakeProduct{}
	client := serveProduct(t, fake)

	numberID := uuid.New()
	require.NoError(t, client.Paired(context.Background(), workspace(), whatsappdomain.PairedLink{
		NumberID: numberID,
		Phone:    "628123",
		JID:      "628123@s.whatsapp.net",
	}))

	require.Equal(t, numberID, fake.paired.NumberID)
	require.Equal(t, "628123@s.whatsapp.net", fake.paired.JID)
}

// --------------------------------------------------------------------------
// The client keeps the product's classification.
// --------------------------------------------------------------------------

func TestTheClientKeepsTheProductsClassification(t *testing.T) {
	cases := []struct {
		status   int
		sentinel error
	}{
		{http.StatusNotFound, whatsappdomain.ErrNotFound},
		{http.StatusBadRequest, whatsappdomain.ErrInvalidInput},
		{http.StatusConflict, whatsappdomain.ErrInvalidInput},
		{http.StatusGone, whatsappdomain.ErrLinkExpired},
		{http.StatusServiceUnavailable, whatsappdomain.ErrUnavailable},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tc.status, map[string]any{"error": "no"})
			}))
			t.Cleanup(server.Close)

			client := NewProductClient(server.URL, testToken, 5*time.Second)
			_, err := client.Number(context.Background(), workspace(), uuid.New())
			require.ErrorIs(t, err, tc.sentinel)
			require.Contains(t, err.Error(), "no")
		})
	}
}

func TestAnUnexpectedStatusIsAFailureRatherThanARefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTeapot, map[string]any{"error": "surprise"})
	}))
	t.Cleanup(server.Close)

	client := NewProductClient(server.URL, testToken, 5*time.Second)
	_, err := client.Number(context.Background(), workspace(), uuid.New())
	require.Error(t, err)
	require.NotErrorIs(t, err, whatsappdomain.ErrNotFound)
	require.Contains(t, err.Error(), "surprise")
}

func TestAnUnreachableProductIsUnavailableRatherThanARefusal(t *testing.T) {
	// A message whose backend is briefly down is still queued: the engine must be
	// able to tell "try again" apart from "that did not work".
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	client := NewProductClient(url, testToken, 500*time.Millisecond)
	_, err := client.Number(context.Background(), workspace(), uuid.New())
	require.ErrorIs(t, err, whatsappdomain.ErrUnavailable)
}

func TestTheProductClientRefusesToGuessAnAddress(t *testing.T) {
	client := NewProductClient("", testToken, time.Second)
	_, err := client.Number(context.Background(), workspace(), uuid.New())
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)

	client = NewProductClient("http://127.0.0.1:1", "", time.Second)
	_, err = client.Number(context.Background(), workspace(), uuid.New())
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)
}

// --------------------------------------------------------------------------
// The product reports its own failures with the shared table.
// --------------------------------------------------------------------------

func TestTheProductMapsItsFailuresOntoStatuses(t *testing.T) {
	fake := &fakeProduct{numberErr: whatsappdomain.ErrNotFound}
	client := serveProduct(t, fake)

	_, err := client.Number(context.Background(), workspace(), uuid.New())
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
}

func TestAnUnclassifiedFailureSaysNothing(t *testing.T) {
	// A database error is the product's business; what crosses the seam is one
	// sentence and an internal status.
	fake := &fakeProduct{numberErr: errors.New("pgx: connection refused")}
	server := serverURL(t, fake)

	response := rawCall(t, server, http.MethodGet, "/internal/whatsapp/numbers/"+uuid.New().String(), testToken, uuid.New().String(), "")
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	require.Equal(t, "internal error", body["error"])
}

// --------------------------------------------------------------------------
// Helpers.
// --------------------------------------------------------------------------

func serverURL(t *testing.T, fake *fakeProduct) string {
	t.Helper()

	product := NewProduct(ProductDeps{Inbox: fake, Drain: fake, Status: fake, Links: fake},
		ProductConfig{Token: testToken}, nil)
	mux := http.NewServeMux()
	product.Routes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func rawCall(t *testing.T, baseURL, method, path, token, workspaceID, body string) *http.Response {
	t.Helper()

	request, err := http.NewRequest(method, baseURL+path, strings.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		request.Header.Set(TokenHeader, token)
	}
	if workspaceID != "" {
		request.Header.Set("X-Workspace-Id", workspaceID)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}
