package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	agentdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain"
	agentservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/service"
	billingdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	billingrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/repository"
	billingservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/service"
	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	llmservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/service"
	workspacedomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/domain"
	workspaceservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/service"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/mail"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/payment/duitku"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/quota"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// openBilling builds the subscription module: the package a workspace is on, the
// allowance it grants, what it owes, and what happens when it does not pay.
//
// It is assembled after the auth services, because it reads the member list a notice
// is addressed to, and before the model gateway, because the gateway's quota policy
// is this module's answer. That order is not cosmetic: a gateway built first would
// hold the configured fixed allowance for the life of the process.
func (c *Container) openBilling(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.DB == nil {
		c.Logger.Warn("billing is disabled: no database connection")
		return nil
	}

	c.Repositories.Billing = billingrepo.New(c.DB)

	deps := billingservice.Deps{
		Repository: c.Repositories.Billing,
		Usage:      billingUsage{reader: c.Repositories.LLM},
		Counter:    billingCounter{},
		Gateway:    c.billingGateway(cfg),
		Notifier:   c.billingNotifier(),
		Customers:  billingCustomers{workspaces: c.Repositories.Workspace},
		Logger:     c.Logger,
		Config: billingservice.Config{
			GraceDays:    cfg.Billing.GraceDays,
			SyncInterval: cfg.Billing.SyncInterval,
			SweepLimit:   cfg.Billing.SweepLimit,
		},
	}
	if c.Redis != nil {
		deps.Counter = billingCounter{counter: quota.New(c.Redis.Raw())}
	}

	c.Billing = billingservice.New(deps)
	c.Services.Billing = c.Billing

	// A new workspace gets its trial package inside the onboarding transaction, so
	// "exactly one live subscription" is a fact about the data rather than a rule
	// every read has to remember.
	if c.Repositories.Workspace != nil {
		c.Repositories.Workspace.SetSubscriptionProvisioner(c.Repositories.Billing)
	}

	// The two modules assembled before this one learn their caps now. Everything
	// between their construction and this line is a process starting up, before any
	// request is served.
	agentservice.UsePlanLimits(c.Services.Agent, c.maxAgentsLimit())
	workspaceservice.UsePlanLimits(c.Services.Workspace, billingMemberLimits{service: c.Billing})

	return nil
}

// billingGateway picks the payment provider this deployment speaks.
//
// It is configuration rather than a build-time choice, and a deployment that names
// none gets nil: the module then writes invoices and enforces packages, and the
// screen says to transfer and confirm. That is the state the product's first
// customers are actually served in, so it is a real mode rather than a degraded one.
func (c *Container) billingGateway(cfg *config.Config) billingdomain.Gateway {
	switch cfg.Billing.Provider {
	case duitku.Name:
		gateway, err := duitku.New(duitku.Config{
			MerchantCode:  cfg.Billing.Duitku.MerchantCode,
			APIKey:        cfg.Billing.Duitku.APIKey,
			Sandbox:       cfg.Billing.Duitku.Sandbox,
			PaymentMethod: cfg.Billing.Duitku.PaymentMethod,
			ExpiryMinutes: cfg.Billing.Duitku.ExpiryMinutes,
			CallbackURL:   cfg.Billing.Duitku.CallbackURL,
			ReturnURL:     cfg.Billing.Duitku.ReturnURL,
			BaseURL:       cfg.Billing.Duitku.BaseURL,
		})
		if err != nil {
			// A misconfigured provider must not stop the API: the module is still
			// what decides an allowance, and a workspace that cannot be charged is
			// still a workspace that must be paced.
			c.Logger.Error("the payment provider is misconfigured; online payment stays off", zap.Error(err))
			return nil
		}
		return gateway
	default:
		if cfg.Billing.Provider != "" {
			c.Logger.Warn("the configured payment provider is not supported by this build",
				zap.String("provider", cfg.Billing.Provider))
		}
		return nil
	}
}

// billingNotifier tells a workspace's owners that their package needs them.
//
// It is one implementation for two effects — a line in the live stream and an email —
// because they are one decision. The email is the part that reaches somebody who is
// not looking at the screen, and the live line is the part that reaches somebody who
// is.
func (c *Container) billingNotifier() billingdomain.Notifier {
	if c.Repositories == nil || c.Repositories.Workspace == nil {
		return nil
	}

	return billingNotifier{
		workspaces: c.Repositories.Workspace,
		mailer:     c.Mailer,
		activity:   c.billingActivity(),
		logger:     c.Logger,
	}
}

// billingActivity writes a line about a package into the workspace's durable event
// stream, so the dashboard shows it without anybody opening the subscription screen.
func (c *Container) billingActivity() billingdomain.Activity {
	if c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}

	return billingActivity{store: c.Repositories.Chat, logger: c.Logger}
}

// billingActivity is the adapter onto the chat module's event store.
type billingActivity struct {
	store  chatdomain.EventStore
	logger *zap.Logger
}

// Emit implements billingdomain.Activity.
func (a billingActivity) Emit(ctx context.Context, event billingdomain.Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("billing: render event: %w", err)
	}

	if _, err := a.store.AppendEvent(ctx, chatdomain.Event{
		WorkspaceID: event.WorkspaceID,
		Type:        event.Type,
		Payload:     payload,
	}); err != nil {
		return err
	}

	return nil
}

// Compile-time checks that every port this module answers is answered, so a renamed
// method breaks the build here rather than leaving a module with a limit nobody
// enforces.
var (
	_ llmdomain.QuotaPolicy        = billingQuotaPolicy{}
	_ agentdomain.PlanLimits       = billingPlanLimits{}
	_ integrationdomain.PlanLimits = billingIntegrationLimits{}
	_ workspacedomain.PlanLimits   = billingMemberLimits{}
	_ billingservice.Customers     = billingCustomers{}
	_ billingdomain.Gateway        = (*duitku.Gateway)(nil)
	_ billingdomain.UsageReader    = billingUsage{}
	_ billingdomain.SpendCounter   = billingCounter{}
	_ billingdomain.Activity       = billingActivity{}
	_ billingdomain.Notifier       = billingNotifier{}
)

// ---------------------------------------------------------------- adapters

// billingUsage adapts the model gateway's usage reader to the billing port.
//
// The day boundary is not passed in and does not leave the gateway: whether "today"
// starts at Jakarta midnight is the gateway's own rule, and a second implementation
// here would be a second answer that eventually disagrees.
type billingUsage struct {
	reader llmdomain.UsageReader
}

// PeriodTokens implements billingdomain.UsageReader.
func (u billingUsage) PeriodTokens(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error) {
	if u.reader == nil {
		return 0, errNoUsageReader
	}
	return u.reader.SumSince(ctx, workspaceID, since)
}

// DailyCost implements billingdomain.UsageReader.
func (u billingUsage) DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if u.reader == nil {
		return 0, errNoUsageReader
	}
	// The day boundary is the gateway's own rule, so it is asked for rather than
	// recomputed: whether "today" starts at Jakarta midnight is the gateway's
	// answer, and a second implementation here would eventually disagree with it.
	return u.reader.CostSince(ctx, workspaceID, llmservice.DayStart(time.Now()))
}

// errNoUsageReader is what an adapter answers when the gateway is not wired, so the
// billing service falls back to the live counter rather than to zero.
var errNoUsageReader = errors.New("billing: the usage ledger is not configured")

// billingCounter adapts the shared Redis counter to the billing port. A nil counter
// answers the sentinel, which is what tells the service to read the ledger instead.
type billingCounter struct {
	counter *quota.Counter
}

// PeriodTokens implements billingdomain.SpendCounter.
func (c billingCounter) PeriodTokens(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if c.counter == nil {
		return 0, errNoCounter
	}
	return c.counter.Spent(ctx, workspaceID)
}

// DailyCost implements billingdomain.SpendCounter.
func (c billingCounter) DailyCost(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if c.counter == nil {
		return 0, errNoCounter
	}
	return c.counter.SpentCostToday(ctx, workspaceID)
}

// errNoCounter is what the counter adapter answers without Redis.
var errNoCounter = errors.New("billing: the live counter is not configured")

// workspaceDirectory is the small part of the workspace module this file needs: who is in
// a workspace, and what it is called.
//
// It is declared here rather than reached for as a concrete repository for the reason the
// workspace module declares its own provisioners this way: a seam this narrow is one a test
// can stand behind, and the two methods are the only ones either adapter reads.
type workspaceDirectory interface {
	Members(ctx context.Context, scope workspacedomain.Scope) ([]workspacedomain.Member, error)
	Workspace(ctx context.Context, scope workspacedomain.Scope) (workspacedomain.Workspace, error)
}

// billingNotifier is the workspace-facing notifier.
type billingNotifier struct {
	workspaces workspaceDirectory
	mailer     mail.Sender
	activity   billingdomain.Activity
	logger     *zap.Logger
}

// Notify implements billingdomain.Notifier.
func (n billingNotifier) Notify(ctx context.Context, scope billingdomain.Scope, notice billingdomain.Notice) error {
	if n.activity != nil {
		if err := n.activity.Emit(ctx, billingdomain.Event{
			WorkspaceID: scope.WorkspaceID,
			Type:        notice.Kind,
			Payload: map[string]any{
				"plan":        notice.PlanName,
				"threshold":   notice.Threshold,
				"used":        notice.Used,
				"allowance":   notice.Allowance,
				"entitlement": notice.Entitlement,
				"reason":      notice.Reason,
			},
		}); err != nil && n.logger != nil {
			n.logger.Warn("could not publish a billing notice", zap.Error(err))
		}
	}

	if n.mailer == nil || n.workspaces == nil {
		return nil
	}

	// Only the people who can act are emailed on the transitions that need money.
	// A warning at 80% reaches everybody, because anybody using the workspace should
	// know why it is about to stop.
	roles := noticeRoles(notice.Kind)

	members, err := n.workspaces.Members(ctx, workspacedomain.Scope{WorkspaceID: scope.WorkspaceID})
	if err != nil {
		return err
	}

	subject, body := billingMessage(notice)
	for _, member := range members {
		if roles != nil && !roles[member.Role] {
			continue
		}
		if err := n.mailer.Send(ctx, mail.Message{To: member.Email, Subject: subject, Text: body}); err != nil && n.logger != nil {
			n.logger.Warn("could not email a billing notice",
				zap.String("email", member.Email), zap.Error(err))
		}
	}

	return nil
}

// noticeRoles is who a notice is emailed to, or nil for everybody in the workspace.
func noticeRoles(kind string) map[string]bool {
	switch kind {
	case billingdomain.NoticeWarning:
		// A warning is about the workspace stopping, so everybody who works in it is
		// told.
		return nil
	default:
		// The rest ask for money or report that it arrived, which is the owner's and
		// the admin's business.
		return map[string]bool{
			workspacedomain.RoleOwner: true,
			workspacedomain.RoleAdmin: true,
		}
	}
}

// billingMessage writes what a person reads.
func billingMessage(notice billingdomain.Notice) (string, string) {
	switch notice.Kind {
	case billingdomain.NoticeWarning:
		if notice.Threshold >= billingdomain.ExhaustedThreshold {
			return "Kuota token workspace habis", notice.Reason
		}
		return "Kuota token workspace hampir habis", notice.Reason
	case billingdomain.NoticePastDue:
		return "Tagihan langganan Bolu menunggu pembayaran", notice.Reason
	case billingdomain.NoticeReadOnly:
		return "Workspace Bolu masuk mode baca saja", notice.Reason
	case billingdomain.NoticePaid:
		return "Pembayaran langganan diterima", notice.Reason
	case billingdomain.NoticeTopUp:
		return "Paket tambahan token masuk", notice.Reason
	default:
		return "Langganan Bolu", notice.Reason
	}
}

// billingCustomers adapts the workspace module to what a payment is addressed to.
type billingCustomers struct {
	workspaces workspaceDirectory
}

// Customer implements billingservice.Customers.
func (c billingCustomers) Customer(ctx context.Context, scope billingdomain.Scope) (billingdomain.Customer, error) {
	if c.workspaces == nil {
		return billingdomain.Customer{}, nil
	}

	workspace, err := c.workspaces.Workspace(ctx, workspacedomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	})
	if err != nil {
		return billingdomain.Customer{}, err
	}

	customer := billingdomain.Customer{
		WorkspaceID: workspace.ID,
		Workspace:   workspace.Name,
		Name:        workspace.Name,
	}

	// The address the receipt goes to is the owner's, which is the person who pays.
	members, err := c.workspaces.Members(ctx, workspacedomain.Scope{WorkspaceID: scope.WorkspaceID})
	if err != nil {
		// The address is a nicety: an invoice with no email is still an invoice, and
		// the workspace's own name is enough to address it.
		return customer, nil //nolint:nilerr // deliberate: the payment does not depend on the receipt address
	}
	for _, member := range members {
		if member.Role == workspacedomain.RoleOwner {
			customer.Email = member.Email
			break
		}
	}

	return customer, nil
}

// ---------------------------------------------------------------- ports the other modules ask

// billingQuotaPolicy adapts this module to the model gateway's quota port, so the
// gateway's own pre-flight check and the task runtime's round check are answered by
// the subscription rather than by a configured constant.
type billingQuotaPolicy struct {
	service billingdomain.Service
}

// Allowance implements llmdomain.QuotaPolicy: the period's token allowance.
//
// A workspace past its grace window is not reported as an allowance of zero, because
// zero means unlimited in this port and would read as "spend freely". It is refused
// instead, with the reason a person can act on.
func (p billingQuotaPolicy) Allowance(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	check, err := p.service.Check(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, billingdomain.ErrReadOnly) {
			// The gateway will refuse the call with this reason, which is the message
			// a person reads.
			return 0, err
		}
		if errors.Is(err, billingdomain.ErrQuotaExceeded) {
			return 0, err
		}
		// Any other failure — the database is down, the workspace has no row — must
		// not read as unlimited. Reporting it as an error is what makes it a refusal
		// the caller sees rather than an allowance nobody bounded.
		return 0, err
	}

	return check.Period.Allowance(), nil
}

// DailyCostAllowance implements llmdomain.QuotaPolicy: the day's ceiling.
func (p billingQuotaPolicy) DailyCostAllowance(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	check, err := p.service.Check(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, billingdomain.ErrReadOnly) || errors.Is(err, billingdomain.ErrQuotaExceeded) {
			return 0, err
		}
		return 0, err
	}

	return check.DailyCostMicros, nil
}

// billingPlanLimits adapts this module to the Bolu registry's plan port.
type billingPlanLimits struct {
	service billingdomain.Service
}

// MaxAgents implements agentdomain.PlanLimits.
func (l billingPlanLimits) MaxAgents(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	limits, err := l.service.Limits(ctx, workspaceID)
	return packageCap(limits.MaxAgents, err)
}

// billingIntegrationLimits adapts this module to the integration hub's plan port.
type billingIntegrationLimits struct {
	service billingdomain.Service
}

// MaxIntegrations implements integrationdomain.PlanLimits.
func (l billingIntegrationLimits) MaxIntegrations(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	limits, err := l.service.Limits(ctx, workspaceID)
	return packageCap(limits.MaxIntegrations, err)
}

// billingMemberLimits adapts this module to the workspace module's plan port.
type billingMemberLimits struct {
	service billingdomain.Service
}

// MaxMembers implements workspacedomain.PlanLimits.
func (l billingMemberLimits) MaxMembers(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	limits, err := l.service.Limits(ctx, workspaceID)
	return packageCap(limits.MaxMembers, err)
}

// packageCap answers a cap the way a workspace with no package at all should be answered:
// with no cap.
//
// A workspace whose live subscription is missing has had nothing sold to it, so there is
// nothing to enforce. The alternative is worse than it looks — refusing every cap-check
// would make creating a Bolu, connecting an app, and inviting a colleague all fail with an
// internal error, which turns a missing row into an outage for the customer rather than a
// gap in what they were charged for.
//
// It is not a way to spend for free: the *quota* port still refuses, because an allowance
// nobody knows is not an allowance, and the backfill plus the onboarding provisioner are
// what keep the state unreachable in the first place. A failure that is not "no
// subscription" is still a failure, so a database that cannot be read is reported rather
// than read as unlimited.
func packageCap(limit int, err error) (int, error) {
	switch {
	case err == nil:
		return limit, nil
	case errors.Is(err, billingdomain.ErrSubscriptionNotFound):
		return 0, nil
	default:
		return 0, err
	}
}

// planLimits is what the container hands a module whose plan port is asked for. It
// answers "unlimited" when the subscription is not wired, which is what a deployment
// without billing runs with.
func (c *Container) maxAgentsLimit() agentdomain.PlanLimits {
	if c.Billing == nil {
		return nil
	}
	return billingPlanLimits{service: c.Billing}
}

// billingIntegrationLimits is the hub's cap, or nil when billing is not wired.
func (c *Container) billingIntegrationLimits() integrationdomain.PlanLimits {
	if c.Billing == nil {
		return nil
	}
	return billingIntegrationLimits{service: c.Billing}
}

// ---------------------------------------------------------------- read-only

// requireActiveSubscription refuses a write from a workspace that is past its grace
// window.
//
// It is a middleware rather than a check inside each service because the rule is
// about the workspace rather than about any one action: "everything that writes
// stops" is one decision, and thirty copies of it would be thirty places for a new
// endpoint to forget.
//
// Two things are deliberately outside it:
//
//   - reads. The epic's promise is that data is never hidden, so a GET is always
//     answered, and the exports and the history with it.
//   - the billing endpoints themselves. A workspace that cannot pay is a workspace
//     that stays read-only forever, and refusing the one call that fixes it would be
//     a trap rather than a policy.
func (c *Container) requireActiveSubscription() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		if c.Billing == nil {
			return ctx.Next()
		}

		// A write is anything that is not a read. HEAD and OPTIONS are here so a
		// browser's preflight is not answered as a refusal.
		switch ctx.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return ctx.Next()
		}

		// The way out is always open. The prefix is stripped rather than the path
		// compared whole, because the group this middleware is mounted on does not
		// know its own prefix — and a comparison that assumed one would let a
		// deployment with a different API prefix through, which is exactly the kind of
		// silent difference this guard exists to prevent.
		if isBillingPath(strings.TrimPrefix(ctx.Path(), c.apiPrefix())) {
			return ctx.Next()
		}

		workspaceID := middleware.WorkspaceUUID(ctx)
		if workspaceID == uuid.Nil {
			return ctx.Next()
		}

		readOnly, err := c.Billing.ReadOnly(ctx.UserContext(), workspaceID)
		switch {
		case err == nil && readOnly:
			return billingReadOnlyResponse(ctx)
		case err == nil:
			return ctx.Next()
		case errors.Is(err, billingdomain.ErrSubscriptionNotFound):
			// A workspace with no package has not lapsed: there is nothing it stopped
			// paying for. It is the same state the caps above read as "no cap", so
			// refusing writes here would leave a workspace that can create unlimited
			// Bolu and nothing else. The row is what needs fixing, and it is said loudly
			// rather than acted on.
			c.Logger.Error("a workspace has no live subscription; writes are allowed and nothing is capped",
				zap.String("workspace", workspaceID.String()))
			return ctx.Next()
		default:
			// A subscription that cannot be *read* is a different thing from one that is
			// absent: nothing here knows whether the workspace has lapsed, so the write is
			// refused rather than opened on a guess.
			c.Logger.Error("could not read the subscription while guarding a write", zap.Error(err))
			return billingReadOnlyResponse(ctx)
		}
	}
}

// apiPrefix is the mount point of the API, which is what the path carries in front of a
// route.
func (c *Container) apiPrefix() string {
	if c == nil || c.Config == nil {
		return ""
	}
	return c.Config.Http.ApiPrefix
}

// isBillingPath reports whether a path, with the API prefix removed, belongs to the module
// that fixes a read-only workspace.
func isBillingPath(path string) bool {
	const prefix = "/billing"
	if len(path) < len(prefix) {
		return false
	}
	for index := 0; index < len(prefix); index++ {
		if path[index] != prefix[index] {
			return false
		}
	}
	// `/billingfoo` is not `/billing`.
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '/'
}

// billingReadOnlyResponse is what a refused write answers.
//
// 402 rather than 403: nothing is forbidden about who is asking, and the difference
// matters to a screen that has to decide whether to show "ask an admin" or "pay the
// invoice".
func billingReadOnlyResponse(ctx *fiber.Ctx) error {
	return fiber.NewError(fiber.StatusPaymentRequired,
		"langganan workspace sudah lewat masa tenggang, jadi sekarang mode baca saja: "+
			"membaca dan mengekspor tetap jalan, perubahan dan tugas baru ditolak sampai tagihannya dibayar")
}

// ---------------------------------------------------------------- the sweep

// RunBillingSweep reconciles every workspace's allowance on an interval until the
// context ends.
//
// It is a loop in the process rather than a durable workflow because nothing in it
// waits: each pass recomputes everything from the times and the ledger, so a pass that
// never ran is corrected by the next one rather than leaving a workspace in a state
// nobody will revisit. `billing_directory.next_sync_at` is what makes that true — a
// missed pass is still a due pass, and it is picked up as soon as the process is back.
func (c *Container) RunBillingSweep(ctx context.Context) {
	if c.Billing == nil || c.Config == nil {
		return
	}
	interval := c.Config.Billing.SweepInterval
	if interval <= 0 {
		c.Logger.Info("the billing sweep is disabled")
		return
	}

	c.sweepBilling(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepBilling(ctx)
		}
	}
}

// sweepBilling runs one pass and reports what it did.
func (c *Container) sweepBilling(ctx context.Context) {
	result, err := c.Billing.Sweep(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.Logger.Error("the billing sweep failed", zap.Error(err))
		}
		return
	}

	if result == (billingdomain.SweepResult{}) {
		return
	}
	c.Logger.Info("the billing sweep ran",
		zap.Int("periods_synced", result.PeriodsSynced),
		zap.Int("warnings_80", result.Warnings80),
		zap.Int("warnings_100", result.Warnings100),
		zap.Int("invoices_created", result.InvoicesCreated),
		zap.Int("invoices_expired", result.InvoicesExpired),
		zap.Int("past_due", result.PastDue),
		zap.Int("read_only", result.ReadOnly),
	)
}
