package container

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	agenthandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/handler"
	approvalhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/handler"
	authhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/handler"
	billinghandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/handler"
	chathandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/handler"
	checklisthandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/handler"
	healthhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/handler"
	integrationhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/handler"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/handler"
	memoryhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/handler"
	orderhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/order/handler"
	retentiondomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
	retentionhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/handler"
	routinehandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/handler"
	taskhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/handler"
	whatsapphandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/handler"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacehandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/handler"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// Handlers aggregates the HTTP handlers wired into the router. A nil handler
// means its module is not configured in this deployment; the routes for it are
// then registered as 503 instead of being skipped silently.
type Handlers struct {
	Health    *healthhandler.Handler
	Auth      *authhandler.Handler
	Workspace *workspacehandler.Handler
	Agent     *agenthandler.Handler
	LLM       *llmhandler.Handler
	Chat      *chathandler.Handler
	Task      *taskhandler.Handler
	// Retention serves the erasure endpoints: a workspace owner asking for their data to be
	// removed, and reading back the record of what was removed.
	Retention *retentionhandler.Handler
	Approval  *approvalhandler.Handler
	// Integration serves the connected accounts and the authorization flow.
	Integration *integrationhandler.Handler
	// Memory serves the two layers and the knowledge base.
	Memory *memoryhandler.Handler
	// Routine serves the routines, their schedules, and their run history.
	Routine *routinehandler.Handler
	// WhatsApp serves the linked numbers, the link flow, and what they carried.
	WhatsApp *whatsapphandler.Handler
	// Billing serves the package, the allowance, and the invoices.
	Billing *billinghandler.Handler
	// Order serves the workspace's own commerce data: the catalogue a person maintains and the orders
	// a Bolu recorded.
	Order *orderhandler.Handler
	// Checklist serves the plans Bolus keep for themselves, which is a read: the tools are how one is
	// written and moved, and the Bolu is the only thing that moves it.
	Checklist *checklisthandler.Handler
}

// newHandlers builds every handler from the service set. Handlers receive the
// logger through the constructor, never from a package variable.
func newHandlers(services *Services, log *zap.Logger, cfg *config.Config) *Handlers {
	handlers := &Handlers{
		Health: healthhandler.New(services.Health, log),
	}

	if services.Auth != nil {
		handlers.Auth = authhandler.New(services.Auth, log, authhandler.Config{
			FrontendURL: cfg.Application.FrontendURL,
			Cookie: authhandler.CookieConfig{
				Path:     cookiePath(cfg.Http.ApiPrefix),
				Domain:   cfg.Auth.CookieDomain,
				Secure:   cfg.Auth.CookieSecure,
				SameSite: "Lax",
			},
		})
	}
	if services.Workspace != nil {
		handlers.Workspace = workspacehandler.New(services.Workspace, log)
	}
	if services.Agent != nil {
		handlers.Agent = agenthandler.New(services.Agent, log)
	}
	if services.LLM != nil {
		// The usage screen's read is installed explicitly: the handler is built from the
		// gateway interface, and the container is the only place that knows the same object
		// answers the reporting port too.
		handlers.LLM = llmhandler.New(services.LLM, log)
		if reporter, ok := services.LLM.(llmdomain.UsageReporter); ok {
			handlers.LLM = handlers.LLM.WithReporter(reporter)
		}
	}
	if erasure, ok := services.Retention.(retentiondomain.ErasureService); ok {
		handlers.Retention = retentionhandler.New(erasure, log)
	}
	if services.Chat != nil {
		handlers.Chat = chathandler.New(services.Chat, log).WithContent(chathandler.ContentConfig{
			FrameAncestors: contentFrameAncestors(cfg),
		})
	}
	if services.Task != nil {
		// The deployment's Temporal UI: a stuck task's link is a deployment fact, and this is
		// the only place that knows it. An empty base leaves the link off the payload rather
		// than pointing at a host that does not answer.
		handlers.Task = taskhandler.New(services.Task, log).
			WithTemporalUI(cfg.Temporal.UIBaseURL, cfg.Temporal.Namespace)
	}
	if services.Approval != nil {
		handlers.Approval = approvalhandler.New(services.Approval, log)
	}

	if services.Memory != nil {
		handlers.Memory = memoryhandler.New(services.Memory, log)
	}

	if services.Routine != nil {
		handlers.Routine = routinehandler.New(services.Routine, log)
	}
	if services.WhatsApp != nil {
		handlers.WhatsApp = whatsapphandler.New(services.WhatsApp, log)
	}
	if services.Order != nil {
		handlers.Order = orderhandler.New(services.Order, log)
	}
	if services.Checklist != nil {
		handlers.Checklist = checklisthandler.New(services.Checklist, log)
	}
	if services.Billing != nil {
		handlers.Billing = billinghandler.New(services.Billing, log)
	}

	if services.Integration != nil {
		handlers.Integration = integrationhandler.New(services.Integration, log, integrationhandler.Config{
			FrontendURL:  cfg.Application.FrontendURL,
			RedirectBase: cfg.Integrations.OAuthRedirectBase,
		})
	}

	return handlers
}

// registerRoutes mounts every module router. This is the only place that knows
// the URL layout, and the only place that decides which middleware guards which
// group:
//
//	/api                     public: health and the auth entry points
//	/api        + requireUser       : account endpoints, workspace list, invites
//	/api        + requireTenant     : everything scoped to the active workspace
func (c *Container) registerRoutes() {
	api := c.app.Group(c.Config.Http.ApiPrefix)

	healthhandler.Routes(c.app, c.Handlers.Health, c.Config.Http.ApiPrefix)

	if c.Handlers.Auth == nil {
		// Without a database the auth routes still exist, but they report that
		// the feature is unavailable instead of returning a confusing 404. The
		// registry fallbacks below are registered for the same reason, so this
		// branch falls through instead of returning.
		api.All("/auth/*", unavailable("authentication is not configured", "auth_not_configured"))
		c.registerUnavailableModules(api)
		return
	}
	authhandler.Routes(api, c.Handlers.Auth)

	// The endpoints a provider reaches directly are registered here, with the
	// other public routes. Where they are registered is what makes them public:
	// an empty-path group below applies its middleware to the whole prefix, so a
	// route added after it would be asked for a session it cannot have.
	//
	// What makes them safe instead is what each carries: a signed, single-use
	// state for the redirect, and a signed reference plus the adapter's own
	// signature for a webhook.
	if c.Handlers.Integration != nil {
		integrationhandler.CallbackRoutes(api, c.Handlers.Integration)
	}

	// A payment notification is a provider reaching us, so it is registered here
	// with the other public routes and for the same reason: an empty-path group
	// below applies its middleware to the whole prefix, so a route added after it
	// would be asked for a session the provider cannot have.
	//
	// What makes it safe instead is the adapter's own signature check, which is the
	// only thing standing between a stranger and a paid package.
	if c.Handlers.Billing != nil {
		billinghandler.WebhookRoutes(api, c.Handlers.Billing)
	}

	// Authenticated, no active workspace required.
	user := api.Group("", c.requireUser())
	authhandler.ProtectedRoutes(user, c.Handlers.Auth)

	// The reads that describe the deployment rather than a workspace live here:
	// the profiles a Bolu can be created from and the catalogue of tools. Asking
	// for a workspace to answer them is what left the profile picker empty on the
	// screen that offers it.
	if c.Handlers.Agent != nil {
		agenthandler.CatalogRoutes(user, c.Handlers.Agent)
	}

	if c.Handlers.Workspace == nil {
		user.All("/workspaces*", unavailable("workspaces are not configured", "workspace_not_configured"))
		return
	}
	workspacehandler.UserRoutes(user, c.Handlers.Workspace)

	// Verified accounts only: onboarding and accepting an invitation create or
	// join tenancy, so the address must be confirmed first.
	verified := api.Group("", c.requireUser(), c.requireVerified())
	workspacehandler.VerifiedRoutes(verified, c.Handlers.Workspace)

	// Active workspace required. The subscription guard runs here rather than inside
	// each service, because "everything that writes stops" is one decision about the
	// workspace rather than thirty about its endpoints — and thirty copies would be
	// thirty places for a new endpoint to forget.
	tenant := api.Group("", c.requireUser(), c.requireTenant(), c.requireActiveSubscription())
	workspacehandler.TenantRoutes(tenant, c.Handlers.Workspace)

	// Member management needs a managing role on top of the tenant. The subscription
	// guard is repeated on each of these groups because each is a group of its own:
	// middleware registered on one does not reach another.
	managers := api.Group("", c.requireUser(), c.requireTenant(), c.requireActiveSubscription(), requireRole(domain.RoleOwner, domain.RoleAdmin))
	workspacehandler.ManagerRoutes(managers, c.Handlers.Workspace)

	// Workspace settings are owner-only.
	owners := api.Group("", c.requireUser(), c.requireTenant(), c.requireActiveSubscription(), requireRole(domain.RoleOwner))
	workspacehandler.OwnerRoutes(owners, c.Handlers.Workspace)

	// Erasing a workspace's data is the most destructive thing this API offers, so it sits on the
	// owner group with the other settings — one role, an active subscription, and an active
	// workspace, which is what makes "who asked" answerable from the session rather than from the
	// body of the request.
	if c.Handlers.Retention != nil {
		retentionhandler.OwnerRoutes(owners, c.Handlers.Retention)
	} else {
		owners.All("/workspaces/current/erasure", unavailable(
			"penghapusan data tidak dikonfigurasi di deployment ini", "erasure_not_configured"))
	}

	if c.Handlers.Agent == nil {
		registerAgentFallback(tenant)
	} else {
		// The registry: everyone in the workspace may read it.
		agenthandler.Routes(tenant, c.Handlers.Agent)
	}

	// Conversations: everyone in the workspace reads and writes them, because
	// talking to a Bolu is the product's main surface.
	if c.Handlers.Chat == nil {
		registerChatFallback(tenant)
	} else {
		chathandler.Routes(tenant, c.Handlers.Chat)
	}

	// Tasks: everyone in the workspace reads the history and the steps, because
	// the feed is built from them. A task is never opened here:
	// chat, a routine, a webhook, or another task opens one, and each of those
	// has its own entry point.
	if c.Handlers.Task == nil {
		registerTaskFallback(tenant)
	} else {
		taskhandler.Routes(tenant, c.Handlers.Task)
	}

	// Memory: what a Bolu remembers and what it knows. Every member reads it and
	// every member edits it, because the person who notices a wrong fact is not
	// always the owner.
	if c.Handlers.Memory == nil {
		registerMemoryFallback(tenant)
	} else {
		memoryhandler.Routes(tenant, c.Handlers.Memory)
	}

	// Routines: the work the team runs on its own. Every member reads and edits them,
	// for the same reason memory is open: the person who notices a routine firing at
	// the wrong hour is not always the owner.
	if c.Handlers.Routine == nil {
		registerRoutineFallback(tenant)
	} else {
		routinehandler.Routes(tenant, c.Handlers.Routine)
	}

	// WhatsApp: the numbers the workspace linked and who answers them. Every
	// member reads and edits them, because a shop's line going quiet is noticed
	// by whoever is on shift rather than by the owner.
	if c.Handlers.WhatsApp == nil {
		registerWhatsAppFallback(tenant)
	} else {
		whatsapphandler.Routes(tenant, c.Handlers.WhatsApp)
	}

	// Billing: the package, the allowance, and the invoices. Every member reads
	// and every member pays, because the person who notices the workspace has gone
	// read-only is not always the owner — and the money arriving is what fixes it.
	//
	// It is deliberately outside the read-only middleware applied to the rest of
	// this group. See `requireActiveSubscription` for why.
	if c.Handlers.Billing == nil {
		registerBillingFallback(tenant)
	} else {
		billinghandler.Routes(tenant, c.Handlers.Billing)
	}

	// Approvals: every member reads the drafts and decides on them, because the
	// person who notices a wrong invoice is not always the owner. Changing what
	// the workspace allows unattended needs a managing role, the way the provider
	// keys do.
	if c.Handlers.Approval == nil {
		registerApprovalFallback(tenant)
	} else {
		approvalhandler.Routes(tenant, c.Handlers.Approval)
	}

	// Integrations: every member reads the connections and their grants, because
	// a screen has to show what a Bolu can reach. Starting a connection flow is
	// open to every member for the same reason deciding a draft is: the person
	// who notices the account is broken is not always the owner.
	if c.Handlers.Integration == nil {
		registerIntegrationFallback(tenant)
	} else {
		integrationhandler.Routes(tenant, c.Handlers.Integration)
	}

	// The catalogue and the orders: every member reads them, because the person
	// answering "what did we sell today" is not always the owner. Maintaining the
	// catalogue needs a managing role, the way the provider keys do — a price is
	// what every quote to every customer will be based on.
	if c.Handlers.Order == nil {
		registerOrderFallback(tenant)
	} else {
		orderhandler.Routes(tenant, c.Handlers.Order)
	}

	// The plans Bolus keep: every member reads them, because the person who notices a Bolu going
	// round in circles is not always the owner. Nothing is written here — a plan is written and moved
	// by the Bolu through its tools, and a client that could tick a line could make it believe work
	// was done that nobody did.
	if c.Handlers.Checklist == nil {
		registerChecklistFallback(tenant)
	} else {
		checklisthandler.Routes(tenant, c.Handlers.Checklist)
	}

	// Model configuration is read by every member, because the Bolu screens show
	// which model a Bolu uses, and written by a managing role, because it holds
	// the provider keys.
	if c.Handlers.LLM == nil {
		registerModelFallback(tenant)
	} else {
		llmhandler.Routes(tenant, c.Handlers.LLM)
	}

	// Changing the registry, a group, or a provider needs a managing role.
	registry := api.Group("", c.requireUser(), c.requireTenant(), c.requireActiveSubscription(), requireRole(domain.RoleOwner, domain.RoleAdmin))
	if c.Handlers.Agent != nil {
		agenthandler.ManagerRoutes(registry, c.Handlers.Agent)
	}
	if c.Handlers.Approval != nil {
		approvalhandler.ManagerRoutes(registry, c.Handlers.Approval)
	}
	if c.Handlers.Chat != nil {
		chathandler.ManagerRoutes(registry, c.Handlers.Chat)
	}
	if c.Handlers.LLM != nil {
		llmhandler.ManagerRoutes(registry, c.Handlers.LLM)
	}
	if c.Handlers.Order != nil {
		orderhandler.ManagerRoutes(registry, c.Handlers.Order)
	}
}

// registerStreamRoutes mounts the live stream and the sandboxed content origin.
//
// The stream carries its own authentication because a browser cannot set a
// header on a WebSocket or an EventSource: the token and the workspace travel in
// the query string instead. Two details make that work:
//
//   - the middleware is registered on the two stream paths only, so a public
//     route is never asked for a token;
//   - it is registered *before* the tenant group, so the identity is resolved
//     from the query before requireUser looks for a header, and requireUser then
//     accepts the identity it finds.
//
// The content route is mounted on the app rather than under the API prefix and
// is deliberately unauthenticated: an iframe without allow-same-origin sends no
// cookie, so the reference in the URL is the only capability, and it names one
// document.
func (c *Container) registerStreamRoutes() {
	api := c.Config.Http.ApiPrefix

	if c.Handlers.Chat != nil {
		for _, path := range []string{api + "/events/stream", api + "/events/socket"} {
			c.app.Use(path, c.requireStreamAuth("chat_not_configured"))
		}

		stream := c.app.Group(api)
		chathandler.StreamRoutes(stream, c.Handlers.Chat)

		chathandler.ContentRoutes(c.app, c.Handlers.Chat)
	}

	// The link attempt has its own socket for a reason the workspace stream cannot
	// serve: a QR is a capability, and it must reach the person who started the
	// attempt rather than every member of the workspace.
	if c.Handlers.WhatsApp != nil {
		c.app.Use(api+"/whatsapp/links/:linkID/socket", c.requireStreamAuth("whatsapp_not_configured"))
		whatsapphandler.LinkStreamRoutes(c.app.Group(api), c.Handlers.WhatsApp)
	}
}

// registerUnavailableModules registers the fallbacks for the modules that are
// not configured in this deployment, so every documented route exists and
// answers 503 rather than 404.
func (c *Container) registerUnavailableModules(api fiber.Router) {
	if c.Handlers.Workspace == nil {
		api.All("/workspaces*", unavailable("workspaces are not configured", "workspace_not_configured"))
		api.All("/invitations*", unavailable("workspaces are not configured", "workspace_not_configured"))
	}
	if c.Handlers.Agent == nil {
		registerAgentFallback(api)
	}
	if c.Handlers.LLM == nil {
		registerModelFallback(api)
	}
	if c.Handlers.Chat == nil {
		registerChatFallback(api)
	}
	if c.Handlers.Task == nil {
		registerTaskFallback(api)
	}
	if c.Handlers.Memory == nil {
		registerMemoryFallback(api)
	}
	if c.Handlers.Routine == nil {
		registerRoutineFallback(api)
	}
	if c.Handlers.Approval == nil {
		registerApprovalFallback(api)
	}
	if c.Handlers.Integration == nil {
		registerIntegrationFallback(api)
	}
}

// registerAgentFallback answers 503 for the registry routes.
func registerAgentFallback(router fiber.Router) {
	message := "the agent registry is not configured"
	router.All("/agents*", unavailable(message, "agent_not_configured"))
	router.All("/teams*", unavailable(message, "agent_not_configured"))
}

// contentFrameAncestors is the list of origins allowed to frame a sandboxed
// content document.
//
// It is the application's own origin, and it has to be named rather than left as
// `'self'`: the document is served from a different host than the app by design,
// so a self-only policy would refuse every frame and break the block entirely.
func contentFrameAncestors(cfg *config.Config) []string {
	if len(cfg.Storage.ContentFrameAncestors) > 0 {
		return cfg.Storage.ContentFrameAncestors
	}
	if frontend := strings.TrimSpace(cfg.Application.FrontendURL); frontend != "" {
		return []string{frontend}
	}
	// Without a configured frontend the safest useful default is the API itself,
	// which is where a local deployment serves the app from.
	return []string{"'self'"}
}

// registerApprovalFallback answers 503 for the approval routes.
func registerApprovalFallback(router fiber.Router) {
	message := "the approval gate is not configured"
	router.All("/drafts*", unavailable(message, "approval_not_configured"))
	router.All("/approval-policies*", unavailable(message, "approval_not_configured"))
	router.All("/audit*", unavailable(message, "approval_not_configured"))
}

// registerOrderFallback answers 503 for the catalogue and order routes.
//
// It is a fallback rather than an omission because the frontend ships against these endpoints: a
// screen that asks for the catalogue should be told the deployment has no storage, not that the route
// does not exist.
// registerChecklistFallback answers 503 for the checklist routes.
func registerChecklistFallback(router fiber.Router) {
	message := "the checklist module is not configured"
	router.All("/checklists*", unavailable(message, "checklist_not_configured"))
}

func registerOrderFallback(router fiber.Router) {
	message := "the catalogue is not configured"
	router.All("/orders*", unavailable(message, "order_not_configured"))
}

// registerIntegrationFallback answers 503 for the integration routes.
//
// The callback is left out on purpose: it is reached by a browser redirect with
// no session, and the fallback group requires a tenant. A deployment with no
// integration module has no adapters, so nothing can start a flow to reach it.
func registerIntegrationFallback(router fiber.Router) {
	message := "the integration hub is not configured"
	router.All("/integrations*", unavailable(message, "integration_not_configured"))
	router.All("/agents/:agentID/grants*", unavailable(message, "integration_not_configured"))
}

// registerTaskFallback answers 503 for the task routes.
func registerTaskFallback(router fiber.Router) {
	message := "the task runtime is not configured"
	router.All("/tasks*", unavailable(message, "task_not_configured"))
}

// registerChatFallback answers 503 for the conversation routes.
func registerChatFallback(router fiber.Router) {
	message := "conversations are not configured"
	router.All("/conversations*", unavailable(message, "chat_not_configured"))
	router.All("/attachments*", unavailable(message, "chat_not_configured"))
	router.All("/events*", unavailable(message, "chat_not_configured"))
}

// registerMemoryFallback answers 503 for the memory routes.
func registerMemoryFallback(router fiber.Router) {
	message := "memory is not configured"
	router.All("/memories*", unavailable(message, "memory_not_configured"))
	router.All("/knowledge*", unavailable(message, "memory_not_configured"))
}

// registerBillingFallback answers 503 for the billing routes.
func registerBillingFallback(router fiber.Router) {
	for _, route := range []struct {
		method string
		path   string
	}{
		{fiber.MethodGet, "/billing/subscription"},
		{fiber.MethodGet, "/billing/plans"},
		{fiber.MethodPost, "/billing/subscription"},
		{fiber.MethodGet, "/billing/invoices"},
		{fiber.MethodPost, "/billing/invoices/:invoiceID/pay"},
		{fiber.MethodPost, "/billing/topups"},
	} {
		router.Add(route.method, route.path, unavailable("billing is not configured", "billing_not_configured"))
	}
}

// registerWhatsAppFallback answers 503 for the WhatsApp routes.
func registerWhatsAppFallback(router fiber.Router) {
	for _, route := range []struct {
		method string
		path   string
	}{
		{fiber.MethodGet, "/whatsapp/numbers"},
		{fiber.MethodPost, "/whatsapp/numbers"},
		{fiber.MethodGet, "/whatsapp/numbers/:numberID"},
		{fiber.MethodPatch, "/whatsapp/numbers/:numberID"},
		{fiber.MethodDelete, "/whatsapp/numbers/:numberID"},
		{fiber.MethodPost, "/whatsapp/numbers/:numberID/terms"},
		{fiber.MethodPost, "/whatsapp/numbers/:numberID/link"},
		{fiber.MethodPost, "/whatsapp/links/:linkID/pair-code"},
		{fiber.MethodGet, "/whatsapp/numbers/:numberID/messages"},
		{fiber.MethodGet, "/whatsapp/numbers/:numberID/outbox"},
		{fiber.MethodGet, "/whatsapp/links/:linkID"},
		{fiber.MethodDelete, "/whatsapp/links/:linkID"},
		{fiber.MethodGet, "/whatsapp/links/:linkID/socket"},
		{fiber.MethodGet, "/whatsapp/messages/:messageID/media"},
	} {
		router.Add(route.method, route.path, unavailable("whatsapp is not configured", "whatsapp_not_configured"))
	}
}

// registerRoutineFallback answers 503 for the routine routes.
func registerRoutineFallback(router fiber.Router) {
	message := "routines are not configured"
	router.All("/routines*", unavailable(message, "routine_not_configured"))
	router.All("/routine-runs*", unavailable(message, "routine_not_configured"))
}

// registerModelFallback answers 503 for the model configuration routes.
func registerModelFallback(router fiber.Router) {
	message := "the model gateway is not configured"
	router.All("/llm/*", unavailable(message, "llm_not_configured"))
}

// cookiePath scopes the refresh cookie to the auth endpoints, so it is not sent
// with every API call.
func cookiePath(apiPrefix string) string {
	if apiPrefix == "" {
		return "/auth"
	}
	return apiPrefix + "/auth"
}

// unavailable answers 503 for a route whose module is not configured.
func unavailable(message, code string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		return response.Unavailable(c, message, code)
	}
}
