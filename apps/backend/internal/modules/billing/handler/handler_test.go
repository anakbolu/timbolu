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

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// The HTTP surface is this file: what a screen can ask for, and what it reads when
// something is refused.

// newTestApp mounts the billing routes behind the identity the container's
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

// fixture is one workspace's package, ready to be rendered.
func fixture() domain.Snapshot {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	plan := domain.Plan{
		ID:              uuid.New(),
		Key:             "keluarga",
		Name:            "Paket Keluarga",
		MonthlyPriceIDR: 149000,
		TokenQuota:      2_000_000,
		DailyCostMicros: 60_000_000,
		PeriodDays:      30,
		MaxAgents:       6,
		MaxIntegrations: 3,
		MaxMembers:      3,
	}
	subscription := domain.Subscription{
		ID:                 uuid.New(),
		WorkspaceID:        uuid.New(),
		PlanID:             plan.ID,
		Status:             domain.StatusActive,
		CurrentPeriodStart: now.AddDate(0, 0, -10),
		CurrentPeriodEnd:   now.AddDate(0, 0, 20),
	}

	return domain.Snapshot{
		Plan:         plan,
		Subscription: subscription,
		Entitlement:  domain.EntitlementActive,
		GraceDays:    7,
		Period: domain.QuotaPeriod{
			ID:             uuid.New(),
			WorkspaceID:    subscription.WorkspaceID,
			PeriodStart:    subscription.CurrentPeriodStart,
			PeriodEnd:      subscription.CurrentPeriodEnd,
			TokensGranted:  2_000_000,
			TokensToppedUp: 200_000,
			TokensUsed:     1_650_000,
		},
		Outstanding: []domain.Invoice{{
			ID:           uuid.New(),
			Kind:         domain.KindSubscription,
			Status:       domain.StatusPending,
			AmountIDR:    149000,
			Provider:     "duitku",
			Attempts:     1,
			Instructions: "Transfer ke BCA virtual account 8808 1234 5678",
			PeriodStart:  subscription.CurrentPeriodEnd,
			PeriodEnd:    subscription.CurrentPeriodEnd.AddDate(0, 0, 30),
		}},
		TopUps: []domain.TopUpPackage{{
			Key: "tambahan-200k", Name: "Tambahan 200 ribu token", TokenAmount: 200000, PriceIDR: 25000,
		}},
		Limits: domain.Limits{MaxAgents: 6, MaxIntegrations: 3, MaxMembers: 3},
	}
}

// TestTheSnapshotCarriesWhatAScreenNeeds: the numbers a person acts on are on the
// wire, and the ids are strings — a typed uuid would publish sixteen numbers and a
// client would read them as one.
func TestTheSnapshotCarriesWhatAScreenNeeds(t *testing.T) {
	svc := mocks.NewService(t)
	workspaceID := uuid.New()
	app := newTestApp(t, svc, workspaceID)

	svc.EXPECT().Snapshot(mock.Anything, mock.MatchedBy(func(scope domain.Scope) bool {
		return scope.WorkspaceID == workspaceID && scope.UserID != uuid.Nil
	})).Return(fixture(), nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/billing/subscription", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	body := resp.Body.String()
	require.Contains(t, body, `"plan_key":"keluarga"`)
	require.Contains(t, body, `"plan_name":"Paket Keluarga"`)
	require.Contains(t, body, `"entitlement":"active"`)
	require.Contains(t, body, `"read_only":false`)
	require.Contains(t, body, `"grace_days":7`)
	require.Contains(t, body, `"tokens_granted":2000000`)
	require.Contains(t, body, `"tokens_topped_up":200000`)
	require.Contains(t, body, `"tokens_used":1650000`)
	require.Contains(t, body, `"tokens_remaining":550000`)
	require.Contains(t, body, `"used_percent":75`)
	require.Contains(t, body, `"outstanding_idr":149000`)
	require.Contains(t, body, `"max_agents":6`)
	require.Contains(t, body, `"payment_instructions":"Transfer ke BCA virtual account 8808 1234 5678"`)
	require.Contains(t, body, `"period_start":"`)
	require.Contains(t, body, `"outstanding":[{`)
	// The packs are sent once, at the top level, and never as null. A list a client reads
	// with `.length` is a crash when Go marshals a nil slice, and a second copy inside the
	// subscription would be a second answer that can drift from this one — which is
	// exactly what the browser found: the field was written in one place and read in the
	// other.
	require.Contains(t, body, `"top_ups":[{"key":"tambahan-200k"`)
	require.NotContains(t, body, `"top_ups":null`)
	require.Equal(t, 1, strings.Count(body, `"top_ups"`), "one place, not two")
	// A field with nothing in it is left out rather than sent as a zero: a client that
	// read `paid_at` as the zero time would render 1970.
	require.NotContains(t, body, `"paid_at":"0001-`)
}

// TestThePlanListIsWhatAWorkspaceCanMoveTo: the packages are read by every member so a
// person can see what moving up costs before they ask for it.
func TestThePlanListIsWhatAWorkspaceCanMoveTo(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	svc.EXPECT().Plans(mock.Anything, mock.Anything).Return([]domain.Plan{
		{Key: "coba", Name: "Paket Coba", TokenQuota: 200000, PeriodDays: 14, IsTrial: true},
		{Key: "keluarga", Name: "Paket Keluarga", MonthlyPriceIDR: 149000, TokenQuota: 2_000_000, PeriodDays: 30},
	}, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/billing/plans", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"key":"coba"`)
	require.Contains(t, resp.Body.String(), `"is_trial":true`)
	require.Contains(t, resp.Body.String(), `"daily_cost_micros":0`)
	require.Contains(t, resp.Body.String(), `"count":2`)
}

// TestChangingThePackageAnswersTheNewSnapshot: the screen does not have to ask twice,
// and the answer carries the package that will be in force next.
func TestChangingThePackageAnswersTheNewSnapshot(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	snapshot := fixture()
	snapshot.PendingPlanKey = "rombongan"
	snapshot.PendingPlanName = "Paket Rombongan"

	svc.EXPECT().Change(mock.Anything, mock.Anything, domain.ChangeRequest{PlanKey: "rombongan"}).
		Return(snapshot, nil).Once()

	resp := do(t, app, fiber.MethodPost, "/api/billing/subscription", `{"plan_key":"rombongan"}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"pending_plan_key":"rombongan"`)
	require.Contains(t, resp.Body.String(), `"pending_plan_name":"Paket Rombongan"`)

	// A body that is not JSON is the caller's problem.
	resp = do(t, app, fiber.MethodPost, "/api/billing/subscription", `{`)
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_request")
}

// TestAnInvoiceListIsPaged: the filter is what a screen's "unpaid" tab asks for, and
// passing it through is what makes the tab work.
func TestAnInvoiceListIsPaged(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	svc.EXPECT().Invoices(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.InvoiceFilter) bool {
		return filter.Status == "pending" && filter.Limit == 5
	})).Return([]domain.Invoice{{
		ID: uuid.New(), Kind: domain.KindTopup, Status: domain.StatusPending,
		AmountIDR: 25000, TokenAmount: 200000, Provider: "duitku", Attempts: 2,
		Instructions: "Transfer ke BCA virtual account 123",
	}}, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/billing/invoices?status=pending&limit=5", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"kind":"topup"`)
	require.Contains(t, resp.Body.String(), `"token_amount":200000`)
	require.Contains(t, resp.Body.String(), `"attempts":2`)
	require.Contains(t, resp.Body.String(), `"payment_instructions":"Transfer ke BCA virtual account 123"`)
	require.Contains(t, resp.Body.String(), `"outstanding":true`)
	require.Contains(t, resp.Body.String(), `"count":1`)
}

// TestPayingAnInvoiceAnswersWhereToPay: the answer carries the virtual account, which
// is what a person needs; there is no link for this payment type and pretending there
// is one would be a button that opens nothing.
func TestPayingAnInvoiceAnswersWhereToPay(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	invoiceID := uuid.New()

	svc.EXPECT().Pay(mock.Anything, mock.Anything, invoiceID).Return(domain.Invoice{
		ID: invoiceID, Kind: domain.KindSubscription, Status: domain.StatusPending,
		AmountIDR: 149000, Provider: "duitku", Attempts: 1,
		Instructions: "Transfer ke BCA virtual account 8808 1234 5678",
	}, nil).Once()

	resp := do(t, app, fiber.MethodPost, "/api/billing/invoices/"+invoiceID.String()+"/pay", `{}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "virtual account 8808 1234 5678")

	// An invoice that is not a uuid never reaches the service.
	resp = do(t, app, fiber.MethodPost, "/api/billing/invoices/bukan-uuid/pay", `{}`)
	require.Equal(t, fiber.StatusBadRequest, resp.Code)

	// One that is already settled is a conflict: nothing is wrong with the request, and
	// there is nothing left to pay.
	svc.EXPECT().Pay(mock.Anything, mock.Anything, invoiceID).
		Return(domain.Invoice{}, domain.ErrInvoiceSettled).Once()

	resp = do(t, app, fiber.MethodPost, "/api/billing/invoices/"+invoiceID.String()+"/pay", `{}`)
	require.Equal(t, fiber.StatusConflict, resp.Code)
	require.Contains(t, resp.Body.String(), "invoice_settled")
}

// TestBuyingATopUpIsACreatedInvoice: the purchase is a new row, so it answers 201 with
// it rather than 200 with nothing.
func TestBuyingATopUpIsACreatedInvoice(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	svc.EXPECT().BuyTopUp(mock.Anything, mock.Anything, domain.TopUpRequest{PackageKey: "tambahan-200k"}).
		Return(domain.Invoice{
			ID: uuid.New(), Kind: domain.KindTopup, Status: domain.StatusPending,
			AmountIDR: 25000, TokenAmount: 200000,
		}, nil).Once()

	resp := do(t, app, fiber.MethodPost, "/api/billing/topups", `{"package_key":"tambahan-200k"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), `"token_amount":200000`)

	resp = do(t, app, fiber.MethodPost, "/api/billing/topups", `{`)
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
}

// TestTheRefusalsAreReadable: each sentinel is a different thing a person does next,
// and the status is what a screen branches on. A 404 and a 400 are both "no", but they
// send somebody to different places.
func TestTheRefusalsAreReadable(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"an invoice that is not there", domain.ErrInvoiceNotFound, fiber.StatusNotFound, "billing_not_found"},
		{"a package that is not sold", domain.ErrPlanNotFound, fiber.StatusNotFound, "billing_not_found"},
		{"a pack that is not sold", domain.ErrTopUpNotFound, fiber.StatusNotFound, "billing_not_found"},
		{"a workspace with no package", domain.ErrSubscriptionNotFound, fiber.StatusNotFound, "billing_not_found"},
		{"no payment provider", domain.ErrNoGateway, fiber.StatusServiceUnavailable, "billing_unavailable"},
		{"an invoice already settled", domain.ErrInvoiceSettled, fiber.StatusConflict, "invoice_settled"},
		{"a request that is wrong", domain.ErrInvalidInput, fiber.StatusBadRequest, "invalid_input"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := mocks.NewService(t)
			app := newTestApp(t, svc, uuid.New())

			svc.EXPECT().Snapshot(mock.Anything, mock.Anything).Return(domain.Snapshot{}, tc.err).Once()

			resp := do(t, app, fiber.MethodGet, "/api/billing/subscription", "")

			require.Equal(t, tc.status, resp.Code)
			require.Contains(t, resp.Body.String(), tc.code)
		})
	}

	// A payment notification that cannot be verified is the caller's payload, and the
	// status is what tells a provider not to retry it. Answering 500 would have every
	// gateway redeliver the same unverifiable body forever.
	webhook := mocks.NewService(t)
	webhookApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	WebhookRoutes(webhookApp.Group("/api"), New(webhook, zap.NewNop()))

	webhook.EXPECT().HandlePayment(mock.Anything, mock.Anything).
		Return(fmt.Errorf("%w: the callback signature does not match", domain.ErrUnverifiable)).Once()

	request := httptest.NewRequest(fiber.MethodPost, "/api/billing/webhooks/duitku", strings.NewReader("amount=1"))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationForm)
	upstream, err := webhookApp.Test(request, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Body.Close() })
	require.Equal(t, fiber.StatusBadRequest, upstream.StatusCode)

	// A failure nobody classified is the deployment's business: it is logged with its
	// own words and answered with one sentence.
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	svc.EXPECT().Snapshot(mock.Anything, mock.Anything).
		Return(domain.Snapshot{}, errors.New("the billing store is down")).Once()

	resp := do(t, app, fiber.MethodGet, "/api/billing/subscription", "")
	require.Equal(t, fiber.StatusInternalServerError, resp.Code)
	require.NotContains(t, resp.Body.String(), "billing store", "what the store said does not cross the wire")
}

// TestAWebhookIsAppliedAndItsProviderIsNamed: the route is public, and what makes it
// safe is the adapter's signature check — so what the handler owes it is the raw body,
// the headers, and which provider the path named.
func TestAWebhookIsAppliedAndItsProviderIsNamed(t *testing.T) {
	svc := mocks.NewService(t)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	api := app.Group("/api")
	WebhookRoutes(api, New(svc, zap.NewNop()))

	var delivery domain.Delivery
	svc.EXPECT().HandlePayment(mock.Anything, mock.MatchedBy(func(received domain.Delivery) bool {
		delivery = received
		return true
	})).Return(nil).Once()

	// Duitku posts a urlencoded form rather than JSON, and the handler hands over the raw
	// body so the adapter can verify a signature over exactly those bytes.
	req := httptest.NewRequest(fiber.MethodPost, "/api/billing/webhooks/duitku",
		strings.NewReader("merchantCode=D1&amount=149000&merchantOrderId=bolu-1-m&resultCode=00&signature=abc"))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set("X-Signature-Key", "abc")

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, "duitku", delivery.Provider)
	require.Contains(t, string(delivery.Body), "merchantOrderId=bolu-1-m",
		"the raw body is what a signature is checked against")
	require.Equal(t, "abc", delivery.Headers["X-Signature-Key"])

	// A delivery the module refuses is answered with the status that says whether
	// retrying can help: an unverifiable payload cannot be retried into being valid.
	svc.EXPECT().HandlePayment(mock.Anything, mock.Anything).
		Return(domain.ErrNoGateway).Once()

	req = httptest.NewRequest(fiber.MethodPost, "/api/billing/webhooks/xendit", strings.NewReader(`{}`))
	resp, err = app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}
