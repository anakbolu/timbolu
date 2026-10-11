// Package handler exposes the integration hub over HTTP: the connected accounts,
// the grants that say which Bolu may use them, and the two halves of an
// authorization flow.
//
// The callback is the only endpoint here that runs without a session. It arrives
// as a browser redirect from the provider, so it learns its tenant from the
// signed state rather than from a header, and it answers with a redirect to the
// app instead of a JSON body — a person is watching it happen.
package handler

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the integration endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
	// redirectBase is the public origin the provider redirects back to, and the
	// origin a finished flow is sent to.
	redirectBase string
	frontendURL  string
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger, frontendURL string) *Handler {
	return &Handler{service: service, log: log, frontendURL: strings.TrimRight(frontendURL, "/")}
}

// Routes registers the endpoints a member of the workspace may use.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/integrations", h.List)
	group.Get("/integrations/adapters", h.Adapters)
	group.Get("/integrations/:integrationID", h.Get)
	group.Post("/integrations/:integrationID/disconnect", h.Disconnect)
	group.Delete("/integrations/:integrationID", h.Delete)
	group.Post("/integrations/:integrationID/refresh", h.Refresh)
	group.Get("/integrations/:app/connect", h.Connect)
	group.Get("/agents/:agentID/grants", h.Grants)
	group.Put("/agents/:agentID/grants/:integrationID", h.SetGrant)
	group.Delete("/agents/:agentID/grants/:integrationID", h.DeleteGrant)
}

// CallbackRoutes registers the provider's redirect.
//
// It is public by necessity: the provider sends the browser here, and the
// browser carries no session header. What makes it safe is the signed,
// single-use state, which names the workspace and the person who started the
// flow.
func CallbackRoutes(group fiber.Router, h *Handler) {
	group.Get("/integrations/oauth/callback", h.Callback)
}

// List returns the workspace's connections.
func (h *Handler) List(c *fiber.Ctx) error {
	connections, err := h.service.List(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list integrations", err)
	}

	payload := make([]integrationPayload, 0, len(connections))
	for _, connection := range connections {
		payload = append(payload, toPayload(connection))
	}
	return response.OK(c, "ok", payload)
}

// Get returns one connection.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := parseID(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "integrationID is not a uuid", "invalid_input", nil)
	}

	connection, err := h.service.Get(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get integration", err)
	}
	return response.OK(c, "ok", toPayload(connection))
}

// Adapters lists the applications this deployment speaks, and what each offers.
//
// It is what the Integrations screen shows before anything is connected: a
// person should be able to see what connecting an account would give them.
func (h *Handler) Adapters(c *fiber.Ctx) error {
	registry := h.service.Registry()
	if registry == nil {
		return response.OK(c, "ok", []adapterPayload{})
	}

	payload := make([]adapterPayload, 0)
	for _, app := range registry.Apps() {
		tools := registry.ToolsFor(app)

		entry := adapterPayload{
			App:   app,
			Tools: make([]toolPayload, 0, len(tools)),
		}
		for _, tool := range tools {
			entry.Tools = append(entry.Tools, toolPayload{
				Name:        tool.Name,
				Label:       tool.Label,
				Description: tool.Description,
				NeedsHuman:  tool.Label == domain.LabelWriteExternal,
			})
		}

		adapter, ok := registry.Adapter(app)
		if ok {
			for _, trigger := range adapter.Triggers() {
				entry.Triggers = append(entry.Triggers, triggerPayload{
					Kind:        trigger.Kind,
					Event:       trigger.Event,
					Description: trigger.Description,
				})
			}
			if oauth, isOAuth := adapter.(domain.OAuthAdapter); isOAuth {
				entry.OAuth = true
				entry.DefaultScopes = oauth.DefaultScopes()
			}
		}
		payload = append(payload, entry)
	}

	return response.OK(c, "ok", payload)
}

// Connect starts an authorization flow and sends the person to the provider.
//
// It redirects rather than returning a URL: the browser has to follow the
// provider's consent page, and doing it here keeps the state out of the client
// where it could be replayed by hand.
func (h *Handler) Connect(c *fiber.Ctx) error {
	app := strings.TrimSpace(c.Params("app"))
	if app == "" {
		return response.Error(c, fiber.StatusBadRequest, "app is required", "invalid_input", nil)
	}

	auth, err := h.service.BeginAuth(c.UserContext(), scope(c), app, c.Query("return_to"))
	if err != nil {
		return h.fail(c, "begin authorization", err)
	}

	return c.Redirect(auth.URL, fiber.StatusFound)
}

// Callback finishes an authorization flow and returns the person to the app.
//
// Every outcome is a redirect, because a person is watching a browser tab: a
// JSON body here would leave them on a blank page after granting access. The
// reason travels as a query parameter the Integrations screen reads.
func (h *Handler) Callback(c *fiber.Ctx) error {
	// The provider reports a refusal as an error parameter rather than a code.
	// It is a person declining, not a failure, and it reads that way.
	if providerErr := c.Query("error"); providerErr != "" {
		h.log.Info("a person declined the authorization",
			zap.String("error", providerErr),
			zap.String("description", c.Query("error_description")),
		)
		return h.redirect(c, "/dashboard/integrations", "declined", providerErr)
	}

	connection, err := h.service.CompleteAuth(c.UserContext(), domain.CompleteAuthRequest{
		Code:        c.Query("code"),
		State:       c.Query("state"),
		RedirectURI: h.callbackURI(c),
	})
	if err != nil {
		reason := classify(err)
		h.log.Warn("an authorization callback failed",
			zap.String("reason", reason),
			zap.Error(err),
		)
		return h.redirect(c, "/dashboard/integrations", "error", reason)
	}

	h.log.Info("an integration was connected",
		zap.String("app", connection.App),
		zap.String("account", connection.AccountLabel),
	)
	return h.redirect(c, "/dashboard/integrations", "connected", connection.App)
}

// Disconnect clears a connection's stored tokens.
func (h *Handler) Disconnect(c *fiber.Ctx) error {
	id, err := parseID(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "integrationID is not a uuid", "invalid_input", nil)
	}

	if err := h.service.Disconnect(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "disconnect integration", err)
	}
	return response.OK(c, "the account was disconnected", nil)
}

// Delete removes a connection.
func (h *Handler) Delete(c *fiber.Ctx) error {
	id, err := parseID(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "integrationID is not a uuid", "invalid_input", nil)
	}

	if err := h.service.Delete(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete integration", err)
	}
	return response.OK(c, "the connection was removed", nil)
}

// Refresh trades a stored refresh token for a new access token.
func (h *Handler) Refresh(c *fiber.Ctx) error {
	id, err := parseID(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "integrationID is not a uuid", "invalid_input", nil)
	}

	connection, err := h.service.RefreshConnection(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "refresh integration", err)
	}
	return response.OK(c, "ok", toPayload(connection))
}

// Grants lists the integrations one Bolu may use.
func (h *Handler) Grants(c *fiber.Ctx) error {
	agentID, err := parseID(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agentID is not a uuid", "invalid_input", nil)
	}

	grants, err := h.service.Grants(c.UserContext(), scope(c), agentID)
	if err != nil {
		return h.fail(c, "list grants", err)
	}

	payload := make([]grantPayload, 0, len(grants))
	for _, grant := range grants {
		payload = append(payload, grantPayload{
			IntegrationID: grant.IntegrationID.String(),
			Permission:    grant.Permission,
		})
	}
	return response.OK(c, "ok", payload)
}

// SetGrant grants or updates one Bolu's permission on one integration.
func (h *Handler) SetGrant(c *fiber.Ctx) error {
	agentID, err := parseID(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agentID is not a uuid", "invalid_input", nil)
	}
	integrationID, err := parseID(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "integrationID is not a uuid", "invalid_input", nil)
	}

	var body grantRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not readable", "invalid_input", nil)
	}

	grant, err := h.service.SetGrant(c.UserContext(), scope(c), agentID, integrationID, body.Permission)
	if err != nil {
		return h.fail(c, "set grant", err)
	}

	return response.OK(c, "the grant was saved", grantPayload{
		IntegrationID: grant.IntegrationID.String(),
		Permission:    grant.Permission,
	})
}

// DeleteGrant removes one grant.
func (h *Handler) DeleteGrant(c *fiber.Ctx) error {
	agentID, err := parseID(c.Params("agentID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agentID is not a uuid", "invalid_input", nil)
	}
	integrationID, err := parseID(c.Params("integrationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "integrationID is not a uuid", "invalid_input", nil)
	}

	if err := h.service.DeleteGrant(c.UserContext(), scope(c), agentID, integrationID); err != nil {
		return h.fail(c, "delete grant", err)
	}
	return response.OK(c, "the grant was removed", nil)
}

// ------------------------------------------------------------------ helpers

// redirect sends the browser back into the app with the outcome in the query.
func (h *Handler) redirect(c *fiber.Ctx, path, status, detail string) error {
	target := h.frontendURL + path + "?integration=" + status
	if detail != "" {
		target += "&detail=" + urlEscape(detail)
	}
	return c.Redirect(target, fiber.StatusFound)
}

// callbackURI is the address this deployment registers with providers. It has to
// match the one the authorization request used, because providers check.
func (h *Handler) callbackURI(c *fiber.Ctx) string {
	if h.redirectBase != "" {
		return h.redirectBase + domain.OAuthRedirectURI
	}
	return c.BaseURL() + domain.OAuthRedirectURI
}

// classify turns an error into a short code the screen can act on.
func classify(err error) string {
	switch {
	case errors.Is(err, domain.ErrInvalidState):
		return "invalid_state"
	case errors.Is(err, domain.ErrExchangeFailed):
		return "exchange_failed"
	case errors.Is(err, domain.ErrUnknownAdapter):
		return "unknown_app"
	default:
		return "failed"
	}
}

// fail maps the module's sentinels onto responses.
func (h *Handler) fail(c *fiber.Ctx, action string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return response.Error(c, fiber.StatusNotFound, "not found", "not_found", nil)
	case errors.Is(err, domain.ErrNoCipher):
		return response.Error(c, fiber.StatusServiceUnavailable,
			"no encryption key is configured, so tokens cannot be stored", "integration_not_configured", nil)
	case errors.Is(err, domain.ErrNoToken):
		return response.Error(c, fiber.StatusConflict,
			"the connection has no usable token; reconnect it", "integration_needs_relogin", nil)
	case errors.Is(err, domain.ErrInvalidInput),
		errors.Is(err, domain.ErrUnknownPermission),
		errors.Is(err, domain.ErrUnknownStatus),
		errors.Is(err, domain.ErrUnknownAdapter),
		errors.Is(err, domain.ErrUnknownTool):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	default:
		h.log.Error(action+" failed", zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// scope reads the tenant from the request, the same way every other module does.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

func parseID(value string) (uuid.UUID, error) { return uuid.Parse(strings.TrimSpace(value)) }

// urlEscape escapes a query value without pulling in a dependency for it.
func urlEscape(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			out.WriteRune(r)
		default:
			for _, b := range []byte(string(r)) {
				out.WriteString("%")
				out.WriteString(hexByte(b))
			}
		}
	}
	return out.String()
}

func hexByte(b byte) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}

// ------------------------------------------------------------------ payloads

type integrationPayload struct {
	ID           string   `json:"integration_id"`
	App          string   `json:"app"`
	AccountLabel string   `json:"account_label"`
	Status       string   `json:"status"`
	StatusReason string   `json:"status_reason,omitempty"`
	Scopes       []string `json:"scopes"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	LastRefresh  string   `json:"last_refreshed_at,omitempty"`
}

type adapterPayload struct {
	App           string           `json:"app"`
	OAuth         bool             `json:"oauth"`
	DefaultScopes []string         `json:"default_scopes,omitempty"`
	Tools         []toolPayload    `json:"tools"`
	Triggers      []triggerPayload `json:"triggers,omitempty"`
}

type toolPayload struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// NeedsHuman is the label restated for a screen: the label is the rule, and
	// this is what it means for a person.
	NeedsHuman bool `json:"needs_human"`
}

type triggerPayload struct {
	Kind        string `json:"kind"`
	Event       string `json:"event"`
	Description string `json:"description"`
}

type grantPayload struct {
	IntegrationID string `json:"integration_id"`
	Permission    string `json:"permission"`
}

type grantRequest struct {
	Permission string `json:"permission"`
}

// toPayload renders a connection for a client.
func toPayload(connection domain.Integration) integrationPayload {
	payload := integrationPayload{
		ID:           connection.ID.String(),
		App:          connection.App,
		AccountLabel: connection.AccountLabel,
		Status:       connection.Status,
		StatusReason: connection.StatusReason,
		Scopes:       connection.Scopes,
	}
	if connection.Scopes == nil {
		payload.Scopes = []string{}
	}
	if connection.ExpiresAt != nil {
		payload.ExpiresAt = connection.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if connection.LastRefreshedAt != nil {
		payload.LastRefresh = connection.LastRefreshedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return payload
}
