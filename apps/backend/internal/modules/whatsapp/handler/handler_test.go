package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// The HTTP surface is this file: what a screen can ask for, and what it reads when
// something is refused.

// newTestApp mounts the WhatsApp routes behind the identity the container's
// middlewares would have put on the request.
func newTestApp(t *testing.T, service domain.Service, workspaceID uuid.UUID) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fiberErr *fiber.Error
			code := fiber.StatusInternalServerError
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	group := app.Group("/api", func(c *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		return c.Next()
	})

	Routes(group, New(service, zap.NewNop()))
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	// The headers are copied because a response is more than its body: what an
	// attachment is served as decides whether a browser shows it or downloads it.
	for name, values := range resp.Header {
		for _, value := range values {
			recorder.Header().Add(name, value)
		}
	}
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

// fixture is one number, ready to be rendered.
func fixture() domain.Number {
	accepted := time.Now()
	return domain.Number{
		ID:            uuid.New(),
		WorkspaceID:   uuid.New(),
		IntegrationID: uuid.New(),
		Phone:         "628123456789",
		Label:         "WhatsApp ••••6789",
		AgentID:       uuid.New(),
		Status:        domain.StatusConnected,
		TOSAcceptedAt: &accepted,
		HourlyLimit:   40,
	}
}

// TestListShowsHowANumberIsDoing: the status, whether the warning was accepted, and
// the phone are what the screen reads, and the phone is masked rather than raw.
func TestListShowsHowANumberIsDoing(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	number := fixture()
	service.EXPECT().Numbers(mock.Anything, mock.Anything).Return([]domain.Number{number}, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/numbers", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	body := resp.Body.String()
	require.Contains(t, body, `"phone":"628123456789"`)
	require.Contains(t, body, `"label":"WhatsApp ••••6789"`)
	require.Contains(t, body, `"terms_accepted":true`)
	require.Contains(t, body, `"linked":true`)
	require.Contains(t, body, `"connected":true`)
	require.Contains(t, body, `"hourly_limit":40`)
	require.Contains(t, body, `"count":1`)
}

// TestAPreparedNumberIsCreatedWithItsBolu: the Bolu is chosen before the link, so
// the row exists to hang the warning on and there is somebody to answer once it is
// linked.
func TestAPreparedNumberIsCreatedWithItsBolu(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	agentID := uuid.New()

	service.EXPECT().
		Prepare(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.LinkRequest) bool {
			return request.AgentID == agentID && request.Phone == "+62 812-3456-789"
		})).
		Return(fixture(), nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/numbers",
		`{"phone":"+62 812-3456-789","agent_id":"`+agentID.String()+`"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), "number prepared")
}

// TestAPreparedNumberNeedsABolu: an id is the caller's problem, and saying so is
// cheaper than a write.
func TestAPreparedNumberNeedsABolu(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/numbers", `{"phone":"628123456789","agent_id":"bukan-uuid"}`)
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
}

// TestTheWarningMustBeAcceptedBeforeALink is E9.6's last gate at the API boundary:
// the refusal says what to do rather than what went wrong.
func TestTheWarningMustBeAcceptedBeforeALink(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().Number(mock.Anything, mock.Anything, numberID).Return(fixture(), nil).Once()
	service.EXPECT().BeginLink(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Link{}, domain.ErrTermsNotAccepted).Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/numbers/"+numberID.String()+"/link", `{}`)

	require.Equal(t, fiber.StatusConflict, resp.Code)
	require.Contains(t, resp.Body.String(), "terms_not_accepted")
	require.Contains(t, resp.Body.String(), "peringatan")
}

// TestStartingALinkAnswersTheFirstState: the screen has something to show on the
// same answer that started the attempt.
func TestStartingALinkAnswersTheFirstState(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().Number(mock.Anything, mock.Anything, numberID).Return(fixture(), nil).Once()
	service.EXPECT().BeginLink(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Link{ID: "link-1", NumberID: numberID, State: domain.LinkCode, Code: "qr-payload"}, nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/numbers/"+numberID.String()+"/link", `{}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"link_id":"link-1"`)
	require.Contains(t, resp.Body.String(), `"qr":"qr-payload"`)
	require.Contains(t, resp.Body.String(), `"state":"code"`)
}

// TestAnExpiredLinkIsGone: the answer tells the screen to offer a new code rather
// than to show a dead end.
func TestAnExpiredLinkIsGone(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().LinkState(mock.Anything, mock.Anything, "link-1").
		Return(domain.Link{}, domain.ErrLinkExpired).Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/links/link-1", "")

	require.Equal(t, fiber.StatusGone, resp.Code)
	require.Contains(t, resp.Body.String(), "link_expired")
}

// TestThePairCodeIsOffered: a phone that cannot scan uses this instead, and the code
// is what a person types.
func TestThePairCodeIsOffered(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		PairCode(mock.Anything, mock.Anything, "link-1", "+62 812-3456-789").
		Return(domain.Link{ID: "link-1", State: domain.LinkCode, PairCode: "ABCD1234"}, nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/links/link-1/pair-code",
		`{"phone":"+62 812-3456-789"}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"pair_code":"ABCD1234"`)
}

// TestAnAlreadyLinkedNumberIsAConflict: one number is one session, so a second link
// is refused with something a screen can explain.
func TestAnAlreadyLinkedNumberIsAConflict(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().Number(mock.Anything, mock.Anything, numberID).Return(fixture(), nil).Once()
	service.EXPECT().BeginLink(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Link{}, domain.ErrAlreadyLinked).Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/numbers/"+numberID.String()+"/link", `{}`)
	require.Equal(t, fiber.StatusConflict, resp.Code)
	require.Contains(t, resp.Body.String(), "already_linked")
}

// TestANumberWithNoBoluCannotBeLeftAlone: the message that arrives has no reader,
// and the API says which decision is missing.
func TestANumberWithNoBoluCannotBeLeftAlone(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().Unlink(mock.Anything, mock.Anything, numberID).
		Return(domain.Number{}, domain.ErrNoAgent).Once()

	resp := do(t, app, fiber.MethodDelete, "/api/whatsapp/numbers/"+numberID.String(), "")
	require.Equal(t, fiber.StatusConflict, resp.Code)
	require.Contains(t, resp.Body.String(), "no_agent")
}

// TestADeploymentWithoutAGatewaySaysSo: the module serves its reads and refuses to
// link, which is a deployment that has not configured one rather than a failure.
func TestADeploymentWithoutAGatewaySaysSo(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().Number(mock.Anything, mock.Anything, numberID).Return(fixture(), nil).Once()
	service.EXPECT().BeginLink(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Link{}, domain.ErrNotConfigured).Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/numbers/"+numberID.String()+"/link", `{}`)

	require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
	require.Contains(t, resp.Body.String(), "whatsapp_unavailable")
}

// TestTheCeilingIsChangedOnItsOwn: a screen that changes one thing does not send the
// rest back, so a change made in another tab cannot be overwritten by a stale copy.
func TestTheCeilingIsChangedOnItsOwn(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().Number(mock.Anything, mock.Anything, numberID).Return(fixture(), nil).Once()
	service.EXPECT().SetLimit(mock.Anything, mock.Anything, numberID, 12).
		Return(fixture(), nil).Once()

	resp := do(t, app, fiber.MethodPatch, "/api/whatsapp/numbers/"+numberID.String(), `{"hourly_limit":12}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "number updated")
}

// TestMessagesCarryWhatTheTaskNeeds: the message, the attachment's state, and the
// task it opened are all on the row, so a screen does not have to ask twice.
func TestMessagesCarryWhatTheTaskNeeds(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()
	taskID := uuid.New()

	service.EXPECT().
		Messages(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.MessageFilter) bool {
			return filter.NumberID == numberID && filter.ChatJID == "628999"
		})).
		Return([]domain.Inbound{{
			ID:          uuid.New(),
			NumberID:    numberID,
			WaMessageID: "msg-1",
			ChatJID:     "628999@s.whatsapp.net",
			SenderName:  "Bu Sari",
			Kind:        domain.KindImage,
			Text:        "ini ukurannya",
			Media:       domain.Media{Kind: domain.KindImage, Status: domain.MediaReady, Key: "whatsapp/x"},
			TaskID:      taskID,
		}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/numbers/"+numberID.String()+"/messages?chat=628999", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	body := resp.Body.String()
	require.Contains(t, body, `"wa_message_id":"msg-1"`)
	require.Contains(t, body, `"media_status":"ready"`)
	require.Contains(t, body, taskID.String())
}

// TestAMessageWithoutItsAttachmentIsANotFound: asking for bytes nobody stored is
// answered as absent rather than served as an empty file.
func TestAMessageWithoutItsAttachmentIsANotFound(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	messageID := uuid.New()

	service.EXPECT().MessageByID(mock.Anything, mock.Anything, messageID).
		Return(domain.Inbound{ID: messageID, Kind: domain.KindText}, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/messages/"+messageID.String()+"/media", "")
	require.Equal(t, fiber.StatusNotFound, resp.Code)
	require.Contains(t, resp.Body.String(), "media_not_found")
}

// TestTheAttachmentIsServedWithItsOwnType: a browser shows an image rather than
// downloading it, which is the difference between reading a photo a customer sent
// and saving a file.
func TestTheAttachmentIsServedWithItsOwnType(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	messageID := uuid.New()

	service.EXPECT().MessageByID(mock.Anything, mock.Anything, messageID).
		Return(domain.Inbound{
			ID: messageID, WaMessageID: "msg-1", Kind: domain.KindImage,
			Media: domain.Media{Kind: domain.KindImage, Status: domain.MediaReady, Key: "whatsapp/x/1/image-1.jpg"},
		}, nil).Once()
	service.EXPECT().MediaBytes(mock.Anything, "whatsapp/x/1/image-1.jpg").
		Return([]byte("jpeg-bytes"), nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/messages/"+messageID.String()+"/media", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Equal(t, "image/jpeg", resp.Header().Get(fiber.HeaderContentType))
	require.Contains(t, resp.Header().Get(fiber.HeaderContentDisposition), "msg-1.jpg")
	require.Equal(t, "jpeg-bytes", resp.Body.String())
}

// TestAThrottledLinkSaysToWait: WhatsApp throttles pairing, and "try again shortly" is
// not the same advice as "wait a few minutes". The module knows which it is, so its
// sentence is what a person reads.
func TestAThrottledLinkSaysToWait(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		PairCode(mock.Anything, mock.Anything, "link-1", mock.Anything).
		Return(domain.Link{}, fmt.Errorf(
			"%w: WhatsApp membatasi percobaan penautan; tunggu beberapa menit lalu coba lagi",
			domain.ErrUnavailable)).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/links/link-1/pair-code", `{"phone":"628123456789"}`)

	require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
	require.Contains(t, resp.Body.String(), "membatasi")
	require.Contains(t, resp.Body.String(), "tunggu beberapa menit")
}

// TestAnUnreachableGatewayKeepsItsOwnSentence: a deployment whose gateway is not
// configured has nothing to wait for, and that is a different message.
func TestAnUnreachableGatewayKeepsItsOwnSentence(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().Numbers(mock.Anything, mock.Anything).
		Return(nil, domain.ErrUnavailable).Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/numbers", "")
	require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
}

// TestTheOutboxShowsWhatHasNotLeft: a message that failed is visible with its
// reason, which is E9.5's second gate at the surface a person reads.
func TestTheOutboxShowsWhatHasNotLeft(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	numberID := uuid.New()

	service.EXPECT().
		Outbox(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.OutboxFilter) bool {
			return filter.NumberID == numberID && filter.Status == domain.OutboxFailed
		})).
		Return([]domain.Outbox{{
			ID: uuid.New(), NumberID: numberID, ToJID: "628999@s.whatsapp.net",
			Kind: domain.KindText, Text: "pesanannya sudah dikirim",
			Status: domain.OutboxFailed, Attempts: 3, LastError: "nomor tidak dikenal",
		}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/numbers/"+numberID.String()+"/outbox?status=failed", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"last_error":"nomor tidak dikenal"`)
	require.Contains(t, resp.Body.String(), `"attempts":3`)
	require.Contains(t, resp.Body.String(), `"status":"failed"`)
}

// TestAMalformedIdentifierIsTheCallersProblem: every route that names a number or a
// message refuses one that is not a uuid before anything is read.
func TestAMalformedIdentifierIsTheCallersProblem(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	for _, request := range []struct{ method, path string }{
		{fiber.MethodGet, "/api/whatsapp/numbers/bukan-uuid"},
		{fiber.MethodPatch, "/api/whatsapp/numbers/bukan-uuid"},
		{fiber.MethodDelete, "/api/whatsapp/numbers/bukan-uuid"},
		{fiber.MethodPost, "/api/whatsapp/numbers/bukan-uuid/terms"},
		{fiber.MethodPost, "/api/whatsapp/numbers/bukan-uuid/link"},
		{fiber.MethodGet, "/api/whatsapp/numbers/bukan-uuid/messages"},
		{fiber.MethodGet, "/api/whatsapp/numbers/bukan-uuid/outbox"},
		{fiber.MethodGet, "/api/whatsapp/messages/bukan-uuid/media"},
	} {
		require.Equal(t, fiber.StatusBadRequest, do(t, app, request.method, request.path, "{}").Code, request.path)
	}

	// And a body that is not JSON is refused before a service is asked anything.
	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodPost, "/api/whatsapp/numbers", "{").Code)
	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodPatch, "/api/whatsapp/numbers/"+uuid.NewString(), "{").Code)
}

// TestAnUnexpectedFailureIsAnInternalError: an error the handler does not recognise
// is a 500 with a code, and the detail stays in the log.
func TestAnUnexpectedFailureIsAnInternalError(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().Numbers(mock.Anything, mock.Anything).
		Return(nil, errors.New("the database is on fire")).Once()

	resp := do(t, app, fiber.MethodGet, "/api/whatsapp/numbers", "")
	require.Equal(t, fiber.StatusInternalServerError, resp.Code)
	require.NotContains(t, resp.Body.String(), "on fire")
}

// TestANumberWhatsappDoesNotKnowIsSaidInItsOwnWords: the module writes a sentence
// about a number WhatsApp does not recognise, and that sentence is what a person
// needs. Replacing it with "no such row" would read as a missing record rather than
// as a number to check.
func TestANumberWhatsappDoesNotKnowIsSaidInItsOwnWords(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		PairCode(mock.Anything, mock.Anything, "link-1", mock.Anything).
		Return(domain.Link{}, fmt.Errorf("%w: WhatsApp tidak mengenal nomor ••••6789; periksa nomornya", domain.ErrNotFound)).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/whatsapp/links/link-1/pair-code", `{"phone":"628123456789"}`)

	require.Equal(t, fiber.StatusNotFound, resp.Code)
	require.Contains(t, resp.Body.String(), "tidak mengenal")
	require.Contains(t, resp.Body.String(), "6789")
	require.NotContains(t, resp.Body.String(), "tidak ada di workspace ini")
}
