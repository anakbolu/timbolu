// Package handler exposes the auth module over HTTP. It talks to the domain
// service interface only, and never to a repository.
package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// CookieName is the httpOnly cookie that carries the refresh token. Keeping the
// refresh token out of JavaScript is what makes a stolen access token expire
// quickly instead of being refreshable.
const CookieName = "bolu_refresh"

// CookieConfig describes where the refresh cookie is valid.
type CookieConfig struct {
	// Path is the API prefix plus /auth, so the cookie is not sent to every
	// endpoint.
	Path string
	// Domain is optional; empty means the host that served the request.
	Domain string
	// Secure must be true outside local development.
	Secure bool
	// SameSite is "lax" by default, which still allows the OAuth redirect back
	// to the API.
	SameSite string
}

// Config holds the handler settings.
type Config struct {
	Cookie CookieConfig
	// FrontendURL is where the Google callback sends the browser back to.
	FrontendURL string
}

// Handler serves the auth endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
	cfg     Config
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger, cfg Config) *Handler {
	if cfg.Cookie.Path == "" {
		cfg.Cookie.Path = "/"
	}
	if cfg.Cookie.SameSite == "" {
		cfg.Cookie.SameSite = "Lax"
	}
	return &Handler{service: service, log: log, cfg: cfg}
}

// registerRequest is the sign-up payload.
type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginRequest is the sign-in payload.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// tokenRequest carries a one-time token or a refresh token.
type tokenRequest struct {
	Token string `json:"token"`
}

// emailRequest carries just an address.
type emailRequest struct {
	Email string `json:"email"`
}

// resetPasswordRequest carries the reset token and the new password.
type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// userPayload is the public shape of an account.
type userPayload struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Onboarded     bool   `json:"onboarded"`
	HasPassword   bool   `json:"has_password"`
	HasGoogle     bool   `json:"has_google"`
}

// sessionPayload is what a sign-in returns. The refresh token travels in the
// httpOnly cookie, so it is not repeated here.
type sessionPayload struct {
	AccessToken    string      `json:"access_token"`
	AccessExpires  string      `json:"access_expires_at"`
	RefreshExpires string      `json:"refresh_expires_at"`
	User           userPayload `json:"user"`
}

// Register creates a password account and emails a verification link.
func (h *Handler) Register(c *fiber.Ctx) error {
	var req registerRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	user, err := h.service.Register(c.UserContext(), domain.RegisterRequest{Email: req.Email, Password: req.Password})
	if err != nil {
		return h.fail(c, "register", err)
	}

	// The message follows the account's state: a deployment that has turned
	// verification off creates an account that is already usable, and telling its
	// owner to check their email would send them to a link that does not exist.
	message := "account created, check your email to verify it"
	if user.EmailVerified {
		message = "account created"
	}
	return response.Created(c, message, toUserPayload(user))
}

// VerifyEmail consumes the one-time verification token.
func (h *Handler) VerifyEmail(c *fiber.Ctx) error {
	var req tokenRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	user, err := h.service.VerifyEmail(c.UserContext(), req.Token)
	if err != nil {
		return h.fail(c, "verify email", err)
	}

	return response.OK(c, "email verified", toUserPayload(user))
}

// ResendVerification sends a new verification link.
func (h *Handler) ResendVerification(c *fiber.Ctx) error {
	var req emailRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	if err := h.service.ResendVerification(c.UserContext(), req.Email); err != nil {
		return h.fail(c, "resend verification", err)
	}

	// The answer is the same whether or not the address exists, so the endpoint
	// cannot be used to enumerate accounts.
	return response.OK(c, "if the address exists and is unverified, a new link has been sent", nil)
}

// Login verifies credentials, opens a session, and sets the refresh cookie.
func (h *Handler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	session, err := h.service.Login(c.UserContext(), domain.LoginRequest{
		Email:     req.Email,
		Password:  req.Password,
		UserAgent: c.Get(fiber.HeaderUserAgent),
		IP:        c.IP(),
	})
	if err != nil {
		return h.fail(c, "login", err)
	}

	h.setRefreshCookie(c, session)
	return response.OK(c, "signed in", toSessionPayload(session))
}

// Refresh rotates the session using the refresh cookie, or the body token for
// non-browser clients.
func (h *Handler) Refresh(c *fiber.Ctx) error {
	token := c.Cookies(CookieName)
	if token == "" {
		var req tokenRequest
		if err := c.BodyParser(&req); err != nil && !errors.Is(err, fiber.ErrUnprocessableEntity) {
			return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
		}
		token = req.Token
	}

	session, err := h.service.Refresh(c.UserContext(), token)
	if err != nil {
		h.clearRefreshCookie(c)
		return h.fail(c, "refresh session", err)
	}

	h.setRefreshCookie(c, session)
	return response.OK(c, "session refreshed", toSessionPayload(session))
}

// Logout revokes the session and clears the cookie.
func (h *Handler) Logout(c *fiber.Ctx) error {
	token := c.Cookies(CookieName)
	if token == "" {
		var req tokenRequest
		if err := c.BodyParser(&req); err == nil {
			token = req.Token
		}
	}

	if err := h.service.Logout(c.UserContext(), token); err != nil {
		return h.fail(c, "logout", err)
	}

	h.clearRefreshCookie(c)
	return response.OK(c, "signed out", nil)
}

// ForgotPassword emails a reset link.
func (h *Handler) ForgotPassword(c *fiber.Ctx) error {
	var req emailRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	if err := h.service.RequestPasswordReset(c.UserContext(), req.Email); err != nil {
		return h.fail(c, "request password reset", err)
	}

	return response.OK(c, "if the address exists, a reset link has been sent", nil)
}

// ResetPassword consumes the reset token and stores the new password.
func (h *Handler) ResetPassword(c *fiber.Ctx) error {
	var req resetPasswordRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	err := h.service.ResetPassword(c.UserContext(), domain.ResetPasswordRequest{
		Token:       req.Token,
		NewPassword: req.Password,
	})
	if err != nil {
		return h.fail(c, "reset password", err)
	}

	h.clearRefreshCookie(c)
	return response.OK(c, "password updated, please sign in again", nil)
}

// Me returns the authenticated account.
func (h *Handler) Me(c *fiber.Ctx) error {
	user, err := h.service.CurrentUser(c.UserContext(), middleware.CurrentUserID(c))
	if err != nil {
		return h.fail(c, "current user", err)
	}
	return response.OK(c, "ok", toUserPayload(user))
}

// GoogleStart redirects the browser to Google's consent screen.
func (h *Handler) GoogleStart(c *fiber.Ctx) error {
	url, err := h.service.GoogleAuthURL(c.UserContext())
	if err != nil {
		return h.fail(c, "google start", err)
	}
	return c.Redirect(url, fiber.StatusFound)
}

// GoogleCallback completes the flow and returns the browser to the frontend
// with the refresh cookie set. The access token is not put in the URL: the
// frontend calls /auth/refresh, which reads the cookie.
func (h *Handler) GoogleCallback(c *fiber.Ctx) error {
	session, err := h.service.GoogleCallback(c.UserContext(), domain.GoogleCallbackRequest{
		Code:      c.Query("code"),
		State:     c.Query("state"),
		UserAgent: c.Get(fiber.HeaderUserAgent),
		IP:        c.IP(),
	})
	if err != nil {
		h.log.Warn("google sign-in failed", zap.Error(err))
		return c.Redirect(h.frontendTarget("login", "google_error=1"), fiber.StatusFound)
	}

	h.setRefreshCookie(c, session)

	target := "login"
	if !session.User.Onboarded {
		target = "onboarding"
	}
	return c.Redirect(h.frontendTarget(target, "google=ok"), fiber.StatusFound)
}

// Routes registers the auth endpoints. Public endpoints live under
// /auth/public so the container can apply the auth middleware to everything
// else without listing exceptions.
func Routes(group fiber.Router, h *Handler) {
	group.Post("/auth/register", h.Register)
	group.Post("/auth/login", h.Login)
	group.Post("/auth/refresh", h.Refresh)
	group.Post("/auth/logout", h.Logout)
	group.Post("/auth/verify-email", h.VerifyEmail)
	group.Post("/auth/resend-verification", h.ResendVerification)
	group.Post("/auth/password/forgot", h.ForgotPassword)
	group.Post("/auth/password/reset", h.ResetPassword)
	group.Get("/auth/google/start", h.GoogleStart)
	group.Get("/auth/google/callback", h.GoogleCallback)
}

// ProtectedRoutes registers the endpoints that require a signed-in user. The
// container applies the auth middleware to this group.
func ProtectedRoutes(group fiber.Router, h *Handler) {
	group.Get("/auth/me", h.Me)
}

func (h *Handler) setRefreshCookie(c *fiber.Ctx, session domain.Session) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    session.RefreshToken,
		Path:     h.cfg.Cookie.Path,
		Domain:   h.cfg.Cookie.Domain,
		Expires:  session.RefreshTokenExp,
		MaxAge:   int(time.Until(session.RefreshTokenExp).Seconds()),
		Secure:   h.cfg.Cookie.Secure,
		HTTPOnly: true,
		SameSite: h.cfg.Cookie.SameSite,
	})
}

func (h *Handler) clearRefreshCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     h.cfg.Cookie.Path,
		Domain:   h.cfg.Cookie.Domain,
		Expires:  time.Now().Add(-time.Hour),
		MaxAge:   -1,
		Secure:   h.cfg.Cookie.Secure,
		HTTPOnly: true,
		SameSite: h.cfg.Cookie.SameSite,
	})
}

func (h *Handler) frontendTarget(path, query string) string {
	base := strings.TrimRight(h.cfg.FrontendURL, "/")
	if base == "" {
		base = ""
	}
	return base + "/" + path + "?" + query
}

// fail maps a domain error to a status code, so the mapping is defined once.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrEmailTaken):
		return response.Error(c, fiber.StatusConflict, "email is already registered", "email_taken", nil)
	case errors.Is(err, domain.ErrInvalidCredentials):
		return response.Error(c, fiber.StatusUnauthorized, "email or password is incorrect", "invalid_credentials", nil)
	case errors.Is(err, domain.ErrEmailNotVerified):
		return response.Error(c, fiber.StatusForbidden, "verify your email first", "email_not_verified", nil)
	case errors.Is(err, domain.ErrInvalidToken):
		return response.Error(c, fiber.StatusBadRequest, "token is invalid, expired, or already used", "invalid_token", nil)
	case errors.Is(err, domain.ErrTooManyAttempts):
		return response.Error(c, fiber.StatusTooManyRequests, err.Error(), "too_many_attempts", nil)
	case errors.Is(err, domain.ErrWeakPassword):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "weak_password", nil)
	case errors.Is(err, domain.ErrGoogleNotConfigured):
		return response.Error(c, fiber.StatusServiceUnavailable, "google sign-in is not configured", "google_not_configured", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, domain.ErrUserNotFound):
		return response.Error(c, fiber.StatusNotFound, "account not found", "user_not_found", nil)
	default:
		h.log.Error("auth request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

func toUserPayload(user domain.User) userPayload {
	return userPayload{
		ID:            user.ID.String(),
		Email:         user.Email,
		EmailVerified: user.EmailVerified,
		Onboarded:     user.Onboarded,
		HasPassword:   user.HasPassword,
		HasGoogle:     user.HasGoogle,
	}
}

func toSessionPayload(session domain.Session) sessionPayload {
	payload := sessionPayload{
		AccessToken: session.AccessToken,
		User:        toUserPayload(session.User),
	}
	if !session.AccessTokenExp.IsZero() {
		payload.AccessExpires = session.AccessTokenExp.UTC().Format(time.RFC3339)
	}
	if !session.RefreshTokenExp.IsZero() {
		payload.RefreshExpires = session.RefreshTokenExp.UTC().Format(time.RFC3339)
	}
	return payload
}

// ParseUserID parses a user id from a request, used by tests and by the
// container middleware.
func ParseUserID(value string) (uuid.UUID, error) {
	return uuid.Parse(value)
}
