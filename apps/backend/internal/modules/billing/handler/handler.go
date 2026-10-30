// Package handler exposes the billing endpoints. It talks to the domain service
// interface only.
package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Handler serves the billing endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return &Handler{service: service, log: log}
}

// Routes registers the subscription endpoints.
//
// Every member reads and every member pays, and that is deliberate: the person who
// notices the workspace has gone read-only is not always the owner, and the money
// arriving is what fixes it. Changing the *package* is the same set of people,
// because it is a choice about the workspace's own bill.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/billing/subscription", h.Snapshot)
	group.Get("/billing/plans", h.Plans)
	group.Post("/billing/subscription", h.Change)
	group.Get("/billing/invoices", h.Invoices)
	group.Post("/billing/invoices/:invoiceID/pay", h.Pay)
	group.Post("/billing/topups", h.BuyTopUp)
}

// WebhookRoutes registers a provider's payment notification.
//
// It is registered on the router a provider can reach without a session, for the
// same reason the integration webhooks are: the caller is Midtrans, not a person.
// What makes it safe instead is the adapter's own signature check, which is the only
// thing standing between a stranger and a paid package.
func WebhookRoutes(group fiber.Router, h *Handler) {
	group.Post("/billing/webhooks/:provider", h.Webhook)
}

// Snapshot returns the workspace's package, its usage, and what it owes.
func (h *Handler) Snapshot(c *fiber.Ctx) error {
	snapshot, err := h.service.Snapshot(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "read subscription", err)
	}

	return response.OK(c, "ok", wireSnapshot(snapshot))
}

// Plans returns the packages a workspace may move to.
func (h *Handler) Plans(c *fiber.Ctx) error {
	plans, err := h.service.Plans(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list plans", err)
	}

	payload := make([]planPayload, 0, len(plans))
	for _, plan := range plans {
		payload = append(payload, wirePlan(plan))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Change moves the workspace to another package.
func (h *Handler) Change(c *fiber.Ctx) error {
	var body changeRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	snapshot, err := h.service.Change(c.UserContext(), scope(c), domain.ChangeRequest{PlanKey: body.PlanKey})
	if err != nil {
		return h.fail(c, "change plan", err)
	}

	return response.OK(c, "plan changed", wireSnapshot(snapshot))
}

// Invoices lists what the workspace was charged.
func (h *Handler) Invoices(c *fiber.Ctx) error {
	invoices, err := h.service.Invoices(c.UserContext(), scope(c), domain.InvoiceFilter{
		Status: strings.TrimSpace(c.Query("status")),
		Limit:  c.QueryInt("limit", 0),
	})
	if err != nil {
		return h.fail(c, "list invoices", err)
	}

	payload := make([]invoicePayload, 0, len(invoices))
	for _, invoice := range invoices {
		payload = append(payload, wireInvoice(invoice))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Pay asks for a payment link for one invoice.
func (h *Handler) Pay(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("invoiceID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invoice id is not a uuid", "invalid_input", nil)
	}

	invoice, err := h.service.Pay(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "pay invoice", err)
	}

	return response.OK(c, "invoice payment", wireInvoice(invoice))
}

// BuyTopUp buys tokens inside the current period.
func (h *Handler) BuyTopUp(c *fiber.Ctx) error {
	var body topUpRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	invoice, err := h.service.BuyTopUp(c.UserContext(), scope(c), domain.TopUpRequest{PackageKey: body.PackageKey})
	if err != nil {
		return h.fail(c, "buy top-up", err)
	}

	return response.Created(c, "top-up invoice", wireInvoice(invoice))
}

// Webhook applies one payment notification.
//
// It answers 200 for anything the provider should not retry — a refusal it cannot
// fix by sending again — and 4xx for what it can. That distinction is the whole
// contract of a webhook: a provider that keeps retrying an unverifiable payload is a
// log nobody can read.
func (h *Handler) Webhook(c *fiber.Ctx) error {
	// The body is read whole, and it is safe to: Fiber caps a request body at the
	// application's own limit before a handler ever sees it, so an unauthenticated
	// route cannot be used to put arbitrary bytes in memory.
	body := c.Body()

	headers := map[string]string{}
	c.Request().Header.VisitAll(func(key, value []byte) {
		headers[string(key)] = string(value)
	})

	delivery := domain.Delivery{
		Provider: strings.TrimSpace(c.Params("provider")),
		Body:     body,
		Headers:  headers,
	}

	if err := h.service.HandlePayment(c.UserContext(), delivery); err != nil {
		return h.fail(c, "payment webhook", err)
	}

	return response.OK(c, "payment accepted", nil)
}

// scope is the tenant the request runs in, put there by the tenant middleware.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound),
		errors.Is(err, domain.ErrInvoiceNotFound),
		errors.Is(err, domain.ErrSubscriptionNotFound),
		errors.Is(err, domain.ErrPlanNotFound),
		errors.Is(err, domain.ErrTopUpNotFound):
		return response.Error(c, fiber.StatusNotFound,
			userMessage(err, domain.ErrNotFound), "billing_not_found", nil)
	case errors.Is(err, domain.ErrNoGateway):
		return response.Error(c, fiber.StatusServiceUnavailable,
			"pembayaran online belum dikonfigurasi di deployment ini", "billing_unavailable", nil)
	case errors.Is(err, domain.ErrInvoiceSettled):
		return response.Error(c, fiber.StatusConflict,
			userMessage(err, domain.ErrInvoiceSettled), "invoice_settled", nil)
	case errors.Is(err, domain.ErrUnverifiable):
		// 400 rather than 500: the payload is wrong and retrying it cannot help. A
		// provider told "internal error" would keep delivering the same unverifiable body,
		// which is a log nobody can read and a queue that never drains.
		return response.Error(c, fiber.StatusBadRequest,
			"pemberitahuan pembayaran tidak bisa diverifikasi", "unverifiable_payment", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest,
			userMessage(err, domain.ErrInvalidInput), "invalid_input", nil)
	default:
		// A failure nobody classified is the deployment's business, so it is
		// logged with its own words and answered with one sentence.
		h.log.Error("a billing request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal error", "internal_error", nil)
	}
}

// userMessage strips this module's own prefix from a refusal, so what a person reads
// is about what they asked for rather than about which package noticed.
func userMessage(err error, prefix error) string {
	message := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), prefix.Error()), ":"))
	if message == "" {
		return prefix.Error()
	}
	return message
}

// ---------------------------------------------------------------- wire types

type changeRequest struct {
	PlanKey string `json:"plan_key"`
}

type topUpRequest struct {
	PackageKey string `json:"package_key"`
}

type planPayload struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	MonthlyPriceIDR int64  `json:"monthly_price_idr"`
	TokenQuota      int64  `json:"token_quota"`
	DailyCostMicros int64  `json:"daily_cost_micros"`
	PeriodDays      int    `json:"period_days"`
	MaxAgents       int    `json:"max_agents"`
	MaxIntegrations int    `json:"max_integrations"`
	MaxMembers      int    `json:"max_members"`
	IsTrial         bool   `json:"is_trial"`
}

type subscriptionPayload struct {
	ID                 string `json:"id"`
	PlanKey            string `json:"plan_key"`
	PlanName           string `json:"plan_name"`
	PendingPlanKey     string `json:"pending_plan_key,omitempty"`
	PendingPlanName    string `json:"pending_plan_name,omitempty"`
	Status             string `json:"status"`
	Entitlement        string `json:"entitlement"`
	CurrentPeriodStart string `json:"current_period_start"`
	CurrentPeriodEnd   string `json:"current_period_end"`
	GraceUntil         string `json:"grace_until,omitempty"`
	GraceDays          int    `json:"grace_days"`
	ReadOnly           bool   `json:"read_only"`
	TokensGranted      int64  `json:"tokens_granted"`
	TokensToppedUp     int64  `json:"tokens_topped_up"`
	TokensUsed         int64  `json:"tokens_used"`
	TokensRemaining    int64  `json:"tokens_remaining"`
	UsedPercent        int    `json:"used_percent"`
	DailyCostMicros    int64  `json:"daily_cost_micros"`
	SpentTodayMicros   int64  `json:"spent_today_micros"`
	OutstandingIDR     int64  `json:"outstanding_idr"`
	MaxAgents          int    `json:"max_agents"`
	MaxIntegrations    int    `json:"max_integrations"`
	MaxMembers         int    `json:"max_members"`
}

type topUpPayload struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	TokenAmount int64  `json:"token_amount"`
	PriceIDR    int64  `json:"price_idr"`
}

type invoicePayload struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	AmountIDR    int64  `json:"amount_idr"`
	TokenAmount  int64  `json:"token_amount,omitempty"`
	PeriodStart  string `json:"period_start"`
	PeriodEnd    string `json:"period_end"`
	Instructions string `json:"payment_instructions,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Attempts     int    `json:"attempts"`
	LastError    string `json:"last_error,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	PaidAt       string `json:"paid_at,omitempty"`
	CreatedAt    string `json:"created_at"`
	Outstanding  bool   `json:"outstanding"`
}

type snapshotPayload struct {
	Plan         planPayload         `json:"plan"`
	Subscription subscriptionPayload `json:"subscription"`
	Invoices     []invoicePayload    `json:"invoices"`
	Outstanding  []invoicePayload    `json:"outstanding"`
	TopUps       []topUpPayload      `json:"top_ups"`
	Entitlement  string              `json:"entitlement"`
	ReadOnly     bool                `json:"read_only"`
	GraceDays    int                 `json:"grace_days"`
}

func wirePlan(plan domain.Plan) planPayload {
	return planPayload{
		Key:             plan.Key,
		Name:            plan.Name,
		MonthlyPriceIDR: plan.MonthlyPriceIDR,
		TokenQuota:      plan.TokenQuota,
		DailyCostMicros: plan.DailyCostMicros,
		PeriodDays:      plan.PeriodDays,
		MaxAgents:       plan.MaxAgents,
		MaxIntegrations: plan.MaxIntegrations,
		MaxMembers:      plan.MaxMembers,
		IsTrial:         plan.IsTrial,
	}
}

func wireInvoice(invoice domain.Invoice) invoicePayload {
	return invoicePayload{
		ID:           invoice.ID.String(),
		Kind:         invoice.Kind,
		Status:       invoice.Status,
		AmountIDR:    invoice.AmountIDR,
		TokenAmount:  invoice.TokenAmount,
		PeriodStart:  timestamp(invoice.PeriodStart),
		PeriodEnd:    timestamp(invoice.PeriodEnd),
		Instructions: invoice.Instructions,
		Provider:     invoice.Provider,
		Attempts:     invoice.Attempts,
		LastError:    invoice.LastError,
		ExpiresAt:    optionalTimestamp(invoice.ExpiresAt),
		PaidAt:       optionalTimestamp(invoice.PaidAt),
		CreatedAt:    timestamp(invoice.CreatedAt),
		Outstanding:  invoice.Outstanding(),
	}
}

func wireSnapshot(snapshot domain.Snapshot) snapshotPayload {
	return snapshotPayload{
		Plan: wirePlan(snapshot.Plan),
		Subscription: subscriptionPayload{
			ID:                 snapshot.Subscription.ID.String(),
			PlanKey:            snapshot.Plan.Key,
			PlanName:           snapshot.Plan.Name,
			PendingPlanKey:     snapshot.PendingPlanKey,
			PendingPlanName:    snapshot.PendingPlanName,
			Status:             snapshot.Subscription.Status,
			Entitlement:        snapshot.Entitlement,
			CurrentPeriodStart: timestamp(snapshot.Subscription.CurrentPeriodStart),
			CurrentPeriodEnd:   timestamp(snapshot.Subscription.CurrentPeriodEnd),
			GraceUntil:         optionalTimestamp(snapshot.Subscription.GraceUntil),
			GraceDays:          snapshot.GraceDays,
			ReadOnly:           snapshot.ReadOnly,
			TokensGranted:      snapshot.Period.TokensGranted,
			TokensToppedUp:     snapshot.Period.TokensToppedUp,
			TokensUsed:         snapshot.Period.TokensUsed,
			TokensRemaining:    snapshot.Period.Remaining(),
			UsedPercent:        snapshot.Period.Used(),
			DailyCostMicros:    snapshot.Plan.DailyCostMicros,
			SpentTodayMicros:   snapshot.UsageToday.CostMicros,
			OutstandingIDR:     owedIDR(snapshot.Outstanding),
			MaxAgents:          snapshot.Limits.MaxAgents,
			MaxIntegrations:    snapshot.Limits.MaxIntegrations,
			MaxMembers:         snapshot.Limits.MaxMembers,
		},
		Entitlement: snapshot.Entitlement,
		ReadOnly:    snapshot.ReadOnly,
		GraceDays:   snapshot.GraceDays,
		Invoices:    wireInvoices(snapshot.Invoices),
		Outstanding: wireInvoices(snapshot.Outstanding),
		TopUps:      wireTopUps(snapshot.TopUps),
	}
}

func wireInvoices(invoices []domain.Invoice) []invoicePayload {
	payload := make([]invoicePayload, 0, len(invoices))
	for _, invoice := range invoices {
		payload = append(payload, wireInvoice(invoice))
	}
	return payload
}

func wireTopUps(packages []domain.TopUpPackage) []topUpPayload {
	payload := make([]topUpPayload, 0, len(packages))
	for _, pack := range packages {
		payload = append(payload, topUpPayload{
			Key:         pack.Key,
			Name:        pack.Name,
			TokenAmount: pack.TokenAmount,
			PriceIDR:    pack.PriceIDR,
		})
	}
	return payload
}

// owedIDR is what the outstanding invoices add up to, which is the number a screen
// shows beside "bayar".
func owedIDR(invoices []domain.Invoice) int64 {
	var total int64
	for _, invoice := range invoices {
		total += invoice.AmountIDR
	}
	return total
}

// timestamp renders a moment as RFC 3339 in UTC, which is what a client parses. The
// workspace's own calendar day is a rendering decision, and the client knows the
// timezone it is in.
func timestamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// optionalTimestamp renders a moment that may be absent as an empty string, because
// a zero time is not a date and a client that parsed one would show 1970.
func optionalTimestamp(at *time.Time) string {
	if at == nil {
		return ""
	}
	return timestamp(*at)
}
