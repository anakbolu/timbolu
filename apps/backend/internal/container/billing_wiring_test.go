package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	billingdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain/mocks"
	billinghandler "github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/handler"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// The wiring tests for billing cover what only the container can get wrong: which routes
// exist, what they answer when the module is not configured, and — the part a unit test
// cannot see because it spans every module — where the read-only guard sits.
//
// The guard is the reason this file exists. It is registered on four route groups rather
// than inside each service, so "everything that writes stops" is one decision; what could
// go wrong is a group somebody forgot, and that is a list this test enumerates.

// TestOpenBillingWithoutADatabaseLeavesTheModuleUnwired keeps the container's
// "running without Postgres" contract.
func TestOpenBillingWithoutADatabaseLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openBilling(context.Background(), c.Config))
	require.Nil(t, c.Billing, "the service stays nil, so its routes report not configured")
	require.Nil(t, c.Repositories.Billing)

	// And the ports the other modules ask are unwired rather than answering zero, which
	// would be a cap of none.
	require.Nil(t, c.maxAgentsLimit())
	require.Nil(t, c.billingIntegrationLimits())
}

// TestBillingRoutesAnswer503WhenTheModuleIsMissing is the honest answer for a deployment
// without a database, and it is the same shape every other module's fallback has.
func TestBillingRoutesAnswer503WhenTheModuleIsMissing(t *testing.T) {
	app := billingFallbackApp(t)

	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/billing/subscription"},
		{http.MethodGet, "/api/billing/plans"},
		{http.MethodPost, "/api/billing/subscription"},
		{http.MethodGet, "/api/billing/invoices"},
		{http.MethodPost, "/api/billing/invoices/" + uuid.NewString() + "/pay"},
		{http.MethodPost, "/api/billing/topups"},
	} {
		resp := callBilling(t, app, request.method, request.path, nil)
		require.Equal(t, http.StatusServiceUnavailable, resp.Code, request.method+" "+request.path)
		require.Contains(t, resp.Body.String(), "billing_not_configured")
	}
}

// TestOnlyTheBillingRoutesAreRegisteredAsTheWebhook: the provider's notification is the
// one route that has to be reachable without a session, and it is registered under a
// path that names the provider so nothing can be verified against the wrong adapter.
func TestOnlyTheBillingRoutesAreRegisteredAsTheWebhook(t *testing.T) {
	svc := mocks.NewService(t)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	api := app.Group("/api")

	billinghandler.Routes(api, billinghandler.New(svc, zap.NewNop()))
	billinghandler.WebhookRoutes(api, billinghandler.New(svc, zap.NewNop()))

	// The session endpoints reject a request with no tenant before reaching the service:
	// an operation that did not know whose package it acts on would act on anybody's.
	svc.EXPECT().Snapshot(mock.Anything, mock.Anything).Return(billingdomain.Snapshot{}, billingdomain.ErrSubscriptionNotFound).Maybe()

	resp := callBilling(t, app, http.MethodGet, "/api/billing/subscription", nil)
	require.NotEqual(t, http.StatusOK, resp.Code)

	// The webhook reaches the service with the provider the path named.
	var delivery billingdomain.Delivery
	svc.EXPECT().HandlePayment(mock.Anything, mock.MatchedBy(func(received billingdomain.Delivery) bool {
		delivery = received
		return true
	})).Return(nil).Once()

	resp = callBilling(t, app, http.MethodPost, "/api/billing/webhooks/duitku", []byte(`merchantOrderId=bolu-1-m&amount=149000&merchantCode=D1&resultCode=00&signature=x`))
	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, "duitku", delivery.Provider)
}

// TestTheReadOnlyGuardStopsWritesAndNeverReads is the epic's fourth gate where a client
// meets it.
//
// The two halves matter equally: a workspace past its grace window may not write, and it
// can still read everything it has — the promise is that no data is hidden, so a GET is
// always answered.
func TestTheReadOnlyGuardStopsWritesAndNeverReads(t *testing.T) {
	workspaceID := uuid.New()
	svc := mocks.NewService(t)

	// Every write in this table is a different module's route, because the guard is
	// registered on the groups rather than inside the services. The list is the point:
	// a group somebody forgot is a write that would go through.
	writes := []struct{ method, path string }{
		{http.MethodPost, "/api/agents"},
		{http.MethodPatch, "/api/agents/" + uuid.NewString()},
		{http.MethodDelete, "/api/agents/" + uuid.NewString()},
		{http.MethodPost, "/api/conversations"},
		{http.MethodPatch, "/api/workspaces/current"},
		{http.MethodPost, "/api/invitations"},
		{http.MethodPost, "/api/routines"},
		{http.MethodPost, "/api/whatsapp/numbers"},
		{http.MethodPost, "/api/drafts/" + uuid.NewString() + "/approve"},
		{http.MethodPost, "/api/integrations/connect"},
		{http.MethodDelete, "/api/memories/" + uuid.NewString()},
		{http.MethodPost, "/api/llm/providers"},
	}

	svc.EXPECT().ReadOnly(mock.Anything, workspaceID).Return(true, nil).Maybe()

	app := guardedApp(t, svc, workspaceID)
	for _, request := range writes {
		resp := callBilling(t, app, request.method, request.path, nil)
		require.Equal(t, fiber.StatusPaymentRequired, resp.Code, request.method+" "+request.path)
		require.Contains(t, resp.Body.String(), "mode baca saja")
	}

	// A read is answered, and the guard does not even ask the subscription about it.
	reads := []struct{ method, path string }{
		{http.MethodGet, "/api/agents"},
		{http.MethodGet, "/api/routines"},
		{http.MethodGet, "/api/drafts"},
		{http.MethodGet, "/api/llm/providers"},
	}
	for _, request := range reads {
		resp := callBilling(t, app, request.method, request.path, nil)
		require.NotEqual(t, fiber.StatusPaymentRequired, resp.Code, request.method+" "+request.path)
	}

	// The way out is always open: a workspace that cannot pay would be read-only forever
	// if the call that fixes it were refused too.
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/api/billing/subscription"},
		{http.MethodPost, "/api/billing/invoices/" + uuid.NewString() + "/pay"},
		{http.MethodPost, "/api/billing/topups"},
	} {
		resp := callBilling(t, app, request.method, request.path, nil)
		require.NotEqual(t, fiber.StatusPaymentRequired, resp.Code, request.method+" "+request.path)
	}
}

// TestTheGuardAsksBeforeEveryWriteAndOnlyWhenItIsWired: a workspace inside its period is
// left alone, and a deployment without billing never has a write refused.
func TestTheGuardAsksBeforeEveryWriteAndOnlyWhenItIsWired(t *testing.T) {
	workspaceID := uuid.New()

	// Inside the period: the write goes through.
	svc := mocks.NewService(t)
	svc.EXPECT().ReadOnly(mock.Anything, workspaceID).Return(false, nil).Once()

	app := guardedApp(t, svc, workspaceID)
	resp := callBilling(t, app, http.MethodPost, "/api/agents", nil)
	require.NotEqual(t, fiber.StatusPaymentRequired, resp.Code)

	// A deployment with no billing wired never refuses anything.
	unwired := guardedApp(t, nil, workspaceID)
	resp = callBilling(t, unwired, http.MethodPost, "/api/agents", nil)
	require.NotEqual(t, fiber.StatusPaymentRequired, resp.Code)
}

// TestASubscriptionThatCannotBeReadDoesNotOpenWrites: a subscription that cannot be *read*
// is not one that is absent. Nothing knows whether the workspace lapsed, so the write is
// refused rather than opened on a guess — and reads keep working, because a broken package
// is not a reason to hide what a workspace has.
func TestASubscriptionThatCannotBeReadDoesNotOpenWrites(t *testing.T) {
	workspaceID := uuid.New()
	svc := mocks.NewService(t)

	svc.EXPECT().ReadOnly(mock.Anything, workspaceID).
		Return(false, errors.New("the subscription store is down")).Once()

	app := guardedApp(t, svc, workspaceID)
	resp := callBilling(t, app, http.MethodPost, "/api/agents", nil)

	require.Equal(t, fiber.StatusPaymentRequired, resp.Code)

	resp = callBilling(t, app, http.MethodGet, "/api/agents", nil)
	require.NotEqual(t, fiber.StatusPaymentRequired, resp.Code)
}

// TestAWorkspaceWithNoPackageAtAllIsNotReadOnly: a workspace whose subscription row is
// missing has stopped paying for nothing, because nothing was sold to it. Refusing its
// writes would leave a workspace that can create unlimited Bolu and nothing else — the caps
// read the same absence as "no cap" — so the write goes through and the row is reported
// loudly instead. It is not a way to spend for free: the quota port still refuses, because
// an allowance nobody knows is not an allowance.
func TestAWorkspaceWithNoPackageAtAllIsNotReadOnly(t *testing.T) {
	workspaceID := uuid.New()
	svc := mocks.NewService(t)

	svc.EXPECT().ReadOnly(mock.Anything, workspaceID).
		Return(false, billingdomain.ErrSubscriptionNotFound).Once()

	app := guardedApp(t, svc, workspaceID)
	resp := callBilling(t, app, http.MethodPost, "/api/agents", nil)

	require.NotEqual(t, fiber.StatusPaymentRequired, resp.Code)
}

// TestABillingPathIsRecognisedButASimilarOneIsNot: the exemption is a prefix, and a path
// that merely starts with the same letters must not be let through.
func TestABillingPathIsRecognisedButASimilarOneIsNot(t *testing.T) {
	require.True(t, isBillingPath("/billing"))
	require.True(t, isBillingPath("/billing/subscription"))
	require.False(t, isBillingPath("/billingfoo"))
	require.False(t, isBillingPath("/bill"))
	require.False(t, isBillingPath("/agents"))
}

// guardedApp mounts the real guard in front of a route table that names each module's
// shape, which is what the middleware sees in production: one group per access level.
func guardedApp(t *testing.T, svc billingdomain.Service, workspaceID uuid.UUID) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	cfg := testConfig()
	cfg.Http.ApiPrefix = "/api"
	c := &Container{Config: cfg, Logger: zap.NewNop(), Billing: svc}

	identify := func(ctx *fiber.Ctx) error {
		middleware.SetIdentity(ctx, uuid.New(), "owner@example.com")
		middleware.SetScope(ctx, database.Scope{UserID: uuid.New(), WorkspaceID: workspaceID})
		return ctx.Next()
	}

	// A handler that answers 200 when it is reached, so what is asserted is the guard
	// rather than the module behind it.
	reached := func(ctx *fiber.Ctx) error { return ctx.SendStatus(fiber.StatusOK) }

	for _, path := range []string{
		"/agents", "/agents/:id", "/conversations", "/workspaces/current", "/invitations",
		"/routines", "/whatsapp/numbers", "/drafts/:id/approve", "/integrations/connect",
		"/memories/:id", "/llm/providers",
	} {
		app.Group("/api", identify, c.requireActiveSubscription()).All(path, reached)
	}

	// The billing routes carry the same guard and are let through by the exemption.
	for _, path := range []string{"/billing/subscription", "/billing/invoices/:id/pay", "/billing/topups"} {
		app.Group("/api", identify, c.requireActiveSubscription()).All(path, reached)
	}

	return app
}

// billingFallbackApp mounts the real route table with the module unwired, which is what a
// deployment without a database serves.
func billingFallbackApp(t *testing.T) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(ctx *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return ctx.Status(code).SendString(err.Error())
		},
	})

	registerBillingFallback(app.Group("/api"))
	return app
}

// callBilling performs one request against a test app.
func callBilling(t *testing.T, app *fiber.App, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}

	resp, err := app.Test(request, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

// TestAPackageThatCannotBeReadIsNotUnlimited: the caps read a *missing* subscription as
// no cap, because nothing was sold to that workspace. A database that cannot be read is a
// different thing, and reporting it is what stops an outage from meaning "unlimited".
func TestAPackageThatCannotBeReadIsNotUnlimited(t *testing.T) {
	workspaceID := uuid.New()
	svc := mocks.NewService(t)

	svc.EXPECT().Limits(mock.Anything, workspaceID).
		Return(billingdomain.Limits{}, errors.New("the subscription store is down")).Once()

	limits := billingPlanLimits{service: svc}

	_, err := limits.MaxAgents(t.Context(), workspaceID)
	require.Error(t, err, "a failure to read is reported rather than read as unlimited")

	// A missing subscription is answered with no cap at all, which is what a workspace
	// that has had nothing sold to it may create.
	absent := mocks.NewService(t)
	absent.EXPECT().Limits(mock.Anything, workspaceID).
		Return(billingdomain.Limits{}, billingdomain.ErrSubscriptionNotFound).Once()

	limit, err := billingPlanLimits{service: absent}.MaxAgents(t.Context(), workspaceID)
	require.NoError(t, err)
	require.Zero(t, limit)
}
