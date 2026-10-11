package container

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	agenthandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/handler"
	authhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/handler"
	chathandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/handler"
	healthhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/handler"
	llmhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/handler"
	taskhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/handler"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacehandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/handler"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
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
		handlers.LLM = llmhandler.New(services.LLM, log)
	}
	if services.Chat != nil {
		handlers.Chat = chathandler.New(services.Chat, log).WithContent(chathandler.ContentConfig{
			FrameAncestors: contentFrameAncestors(cfg),
		})
	}
	if services.Task != nil {
		handlers.Task = taskhandler.New(services.Task, log)
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

	// Authenticated, no active workspace required.
	user := api.Group("", c.requireUser())
	authhandler.ProtectedRoutes(user, c.Handlers.Auth)

	if c.Handlers.Workspace == nil {
		user.All("/workspaces*", unavailable("workspaces are not configured", "workspace_not_configured"))
		return
	}
	workspacehandler.UserRoutes(user, c.Handlers.Workspace)

	// Verified accounts only: onboarding and accepting an invitation create or
	// join tenancy, so the address must be confirmed first.
	verified := api.Group("", c.requireUser(), c.requireVerified())
	workspacehandler.VerifiedRoutes(verified, c.Handlers.Workspace)

	// Active workspace required.
	tenant := api.Group("", c.requireUser(), c.requireTenant())
	workspacehandler.TenantRoutes(tenant, c.Handlers.Workspace)

	// Member management needs a managing role on top of the tenant.
	managers := api.Group("", c.requireUser(), c.requireTenant(), requireRole(domain.RoleOwner, domain.RoleAdmin))
	workspacehandler.ManagerRoutes(managers, c.Handlers.Workspace)

	// Workspace settings are owner-only.
	owners := api.Group("", c.requireUser(), c.requireTenant(), requireRole(domain.RoleOwner))
	workspacehandler.OwnerRoutes(owners, c.Handlers.Workspace)

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
	// the office and the feed are built from them. A task is never opened here:
	// chat, a routine, a webhook, or another task opens one, and each of those
	// has its own entry point.
	if c.Handlers.Task == nil {
		registerTaskFallback(tenant)
	} else {
		taskhandler.Routes(tenant, c.Handlers.Task)
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
	registry := api.Group("", c.requireUser(), c.requireTenant(), requireRole(domain.RoleOwner, domain.RoleAdmin))
	if c.Handlers.Agent != nil {
		agenthandler.ManagerRoutes(registry, c.Handlers.Agent)
	}
	if c.Handlers.Chat != nil {
		chathandler.ManagerRoutes(registry, c.Handlers.Chat)
	}
	if c.Handlers.LLM != nil {
		llmhandler.ManagerRoutes(registry, c.Handlers.LLM)
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
	if c.Handlers.Chat == nil {
		return
	}

	api := c.Config.Http.ApiPrefix
	for _, path := range []string{api + "/events/stream", api + "/events/socket"} {
		c.app.Use(path, c.requireStreamAuth())
	}

	stream := c.app.Group(api)
	chathandler.StreamRoutes(stream, c.Handlers.Chat)

	chathandler.ContentRoutes(c.app, c.Handlers.Chat)
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
