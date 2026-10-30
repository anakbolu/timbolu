// Package httpapi is the control plane's HTTP surface: ten routes, no state of its
// own, and one Runtime behind it.
//
// The shape is deliberate. Every route names the bot it acts on, so a caller can
// never reach another bot's session by omission; every action is its own route
// rather than one "run anything" endpoint, because the approval gates depend on
// being able to label them separately (docs/riset-microvm.md §5.1); and the whole
// surface is small enough to read in one sitting, which is what a PoC is for.
//
// Two things cross this surface that are not JSON: a screenshot, which is PNG
// bytes, and the noVNC client, which is proxied through to the session's sandbox so
// that a browser only ever has to reach the control plane. Everything else is an
// object in and an object out.
//
// Errors are sentinels from the sandbox package mapped to status codes here, so a
// caller can branch on the status and a reader can see the mapping in one table.
// Nothing on this surface answers 501: every route here is implemented by every
// runtime, and a runtime that cannot do something says so as 502 or 500 rather than
// pretending the route does not exist.
//
// Access is a bearer token (VCOMPUTER_CONTROL_TOKEN). The token is required, and
// /health is the only route that does not ask for it. A browser cannot send an
// Authorization header for a page load or a websocket, so a noVNC URL may carry the
// token as a query parameter once, which trades it for an HttpOnly cookie scoped to
// this API's session paths.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/anakbolu/timbolu/apps/virtual-computer/control/internal/sandbox"
)

// maxBody bounds a request. The largest thing that crosses this surface is a typed
// string, a URL, or a command, so 64 KiB is generous and still refuses a stream.
const maxBody = 64 << 10

// tokenCookie is the cookie a browser carries after it was handed the token in a
// noVNC URL. It is scoped to the session paths rather than to the whole API so that
// nothing else on the origin receives it.
const (
	tokenCookie    = "vc_token"
	tokenCookieDir = "/v1/sessions/"
)

// The two 401 sentences. They are constants because a caller classifies on them: the
// backend carries whichever one it was answered into the error a task reports, and
// "the token is not valid" is what sends an operator to compare two secrets rather
// than to look for a variable they never set.
const (
	tokenMissing = "a bearer token is required"
	tokenRefused = "the token is not valid"
)

// Server is the control plane's handler.
type Server struct {
	runtime sandbox.Runtime
	log     *log.Logger
	// token is the bearer token every /v1 route requires. Empty means the surface is
	// open, which only the in-memory runtime in tests may ask for.
	token string
	// open is that request, made explicitly: WithOpenAccess says so at the call site
	// instead of leaving an empty token to mean two different things.
	open bool
	// proxy forwards the noVNC client to a session's sandbox. One proxy serves every
	// session: the target is per request, and it travels in the request context.
	proxy *httputil.ReverseProxy
}

// ValidToken reports whether a token can be used as both a bearer token and a cookie
// value.
//
// A token is one secret presented three ways: an Authorization header, a query
// parameter, and the cookie that parameter is traded for. The last two impose the
// narrow rule — printable ASCII without the separators a cookie grammar reserves —
// and a token that broke it would fail silently in a browser, so the control plane
// checks it at startup instead.
func ValidToken(token string) bool {
	if token == "" {
		return false
	}
	for i := range len(token) {
		switch c := token[i]; {
		case c < 0x21 || c > 0x7e:
			return false
		case c == '"', c == ';', c == ',', c == '\\':
			return false
		}
	}
	return true
}

// Option configures the handler. Options exist so that "this surface is protected"
// and "this surface is deliberately open" are both visible where the handler is
// built, rather than inferred from an empty string.
type Option func(*Server)

// WithToken requires the given bearer token on every /v1 route. It is what main
// passes after reading VCOMPUTER_CONTROL_TOKEN.
func WithToken(token string) Option {
	return func(s *Server) { s.token = token }
}

// WithOpenAccess drops the token requirement.
//
// It exists for one caller: a test that drives the in-memory runtime with no
// deployment around it. A real deployment never uses it, because the control plane
// can start a sandbox and run a command inside it.
func WithOpenAccess() Option {
	return func(s *Server) { s.open = true }
}

// New builds the handler. It returns an http.Handler rather than a *Server so the
// caller cannot reach past the routes.
func New(runtime sandbox.Runtime, logger *log.Logger, options ...Option) http.Handler {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	s := &Server{runtime: runtime, log: logger}
	for _, option := range options {
		option(s)
	}
	s.proxy = s.newNoVNCProxy()

	mux := http.NewServeMux()
	// health is the only route that says nothing about a bot, and the only one that
	// does not ask for the token: the compose healthcheck and the e2e script both
	// probe it before anything else exists.
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1/sessions", s.ensureSession)
	mux.HandleFunc("GET /v1/sessions/{bot_id}", s.sessionStatus)
	mux.HandleFunc("DELETE /v1/sessions/{bot_id}", s.stopSession)
	// screenshot answers both ways: POST is the action a worker calls, and GET is the
	// same capture so a plain <img> can embed the desktop. An <img> cannot send a
	// POST, and a page that cannot show the screen would defeat the point of the PoC.
	mux.HandleFunc("POST /v1/sessions/{bot_id}/screenshot", s.screenshot)
	mux.HandleFunc("GET /v1/sessions/{bot_id}/screenshot", s.screenshot)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/click", s.click)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/type", s.typeText)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/navigate", s.navigate)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/exec", s.exec)
	// The noVNC client and its websocket both arrive here, and both are forwarded to
	// the sandbox that serves them.
	mux.Handle("GET /v1/sessions/{bot_id}/novnc/{rest...}", http.HandlerFunc(s.novnc))

	return s.authenticate(mux)
}

// authenticate requires the bearer token on everything but /health.
//
// Three ways to present it, in the order a caller tries them: the Authorization
// header for a worker, the cookie for a browser that already traded the token in,
// and — on the noVNC route only — the token as a query parameter, because a page
// load and a websocket handshake cannot carry a header. A query parameter reaches
// logs and history, so it is accepted exactly where a browser leaves no choice, and
// the answer sets the cookie so the next request does not need it.
//
// The two refusals are different mistakes and the answer says which one it is: no
// credential at all means the caller has nothing to present, while a credential that
// does not match means the two sides were started with different secrets. A worker
// reports this sentence to a person — "a bearer token is required" for a token that
// was sent and merely wrong is how a wrong secret gets looked for in the wrong place.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.open || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		if token := r.URL.Query().Get("token"); token != "" && isNoVNCPath(r.URL.Path) {
			if !s.tokenMatches(token) {
				s.writeError(w, http.StatusUnauthorized, tokenRefused)
				return
			}
			s.setTokenCookie(w, r, token)
			next.ServeHTTP(w, r)
			return
		}

		bearer, cookie := bearerToken(r), cookieToken(r)
		if s.tokenMatches(bearer) || s.tokenMatches(cookie) {
			next.ServeHTTP(w, r)
			return
		}
		if bearer == "" && cookie == "" {
			s.writeError(w, http.StatusUnauthorized, tokenMissing)
			return
		}
		s.writeError(w, http.StatusUnauthorized, tokenRefused)
	})
}

// tokenMatches compares in constant time, so that a wrong token cannot be narrowed
// down by how long the answer took.
func (s *Server) tokenMatches(presented string) bool {
	if s.token == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

// setTokenCookie hands the browser the token as an HttpOnly cookie scoped to the
// session paths. HttpOnly because no script needs it, and SameSite=Strict because no
// other site should ever be able to make a browser act on a bot's desktop.
func (s *Server) setTokenCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     tokenCookieDir,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
}

// health answers whether the process is up. It does not claim any sandbox is
// running: that is per bot and read from the runtime.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "vcomputer-control",
		"poc":     true,
	})
}

// ensureSession creates the session if it is not there and answers it either way,
// which is what makes it safe for a worker to call before every task.
func (s *Server) ensureSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BotID string `json:"bot_id"`
	}
	if !s.decode(w, r, &body) {
		return
	}

	session, err := s.runtime.Ensure(r.Context(), body.BotID)
	if err != nil {
		s.fail(w, "ensure session", err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// sessionStatus reports where a session stands.
func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	session, err := s.runtime.Status(r.Context(), r.PathValue("bot_id"))
	if err != nil {
		s.fail(w, "session status", err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// stopSession ends a session. The volume outlives it, so the next ensure reuses the
// browser profile rather than starting a login from scratch.
func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	if err := s.runtime.Stop(r.Context(), r.PathValue("bot_id")); err != nil {
		s.fail(w, "stop session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

// screenshot answers PNG bytes, over POST for a worker and GET for an <img>. It
// captures on every call rather than serving a cached frame: the caller is asking
// what the desktop looks like now, and a cached answer would be a lie.
func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	png, err := s.runtime.Screenshot(r.Context(), r.PathValue("bot_id"))
	if err != nil {
		s.fail(w, "screenshot", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(png); err != nil {
		s.log.Printf("screenshot: write: %v", err)
	}
}

// click presses a point. The body carries coordinates rather than a selector
// because the model sees a picture, not a DOM.
func (s *Server) click(w http.ResponseWriter, r *http.Request) {
	var body struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	if !s.decode(w, r, &body) {
		return
	}

	if err := s.runtime.Click(r.Context(), r.PathValue("bot_id"), body.X, body.Y); err != nil {
		s.fail(w, "click", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clicked": true, "x": body.X, "y": body.Y})
}

// typeText sends text to the focused window.
func (s *Server) typeText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !s.decode(w, r, &body) {
		return
	}

	if err := s.runtime.Type(r.Context(), r.PathValue("bot_id"), body.Text); err != nil {
		s.fail(w, "type", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"typed": true, "length": len([]rune(body.Text))})
}

// navigate points the session's browser at a URL.
//
// The scheme rule lives in the runtime, which is where it is enforced for both
// runtimes: the memory runtime records the navigation and refuses the same URLs the
// docker runtime refuses, so a caller cannot discover the difference by trying.
func (s *Server) navigate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !s.decode(w, r, &body) {
		return
	}

	if err := s.runtime.Navigate(r.Context(), r.PathValue("bot_id"), body.URL); err != nil {
		s.fail(w, "navigate", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"navigated": true, "url": strings.TrimSpace(body.URL)})
}

// exec runs a command inside the sandbox.
//
// It is the route riset §5.1 labels write_external and the one that must be behind an
// approval gate before it is wired to a model, which is why it is here as a route with
// no caller above it rather than as a capability.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !s.decode(w, r, &body) {
		return
	}

	result, err := s.runtime.Exec(r.Context(), r.PathValue("bot_id"), body.Command)
	if err != nil {
		s.fail(w, "exec", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// novnc forwards the noVNC client to the session's sandbox.
//
// The browser reaches one origin — this one — and the sandbox network stays
// unreachable from outside (docs/adr/0003-virtual-computer.md §3.6). The proxy is
// httputil's, so the websocket upgrade that carries the framebuffer goes through the
// same path as the page itself.
func (s *Server) novnc(w http.ResponseWriter, r *http.Request) {
	botID := r.PathValue("bot_id")

	target, err := s.runtime.NoVNCTarget(r.Context(), botID)
	if err != nil {
		s.fail(w, "novnc", err)
		return
	}

	address, err := url.Parse(target)
	if err != nil {
		s.log.Printf("novnc: the runtime answered an unusable target %q: %v", target, err)
		s.writeError(w, http.StatusBadGateway, "the sandbox address is not usable")
		return
	}

	route := novncRoute{target: address, prefix: novncPrefix(botID)}
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), novncRouteKey{}, route)))
}

// novncRoute is where one noVNC request is going: the sandbox that serves it and the
// control-plane prefix to strip off the path.
type novncRoute struct {
	target *url.URL
	prefix string
}

// novncRouteKey is the context key the proxy's rewrite step reads.
type novncRouteKey struct{}

// novncPrefix is the path the control plane serves noVNC on for one bot.
func novncPrefix(botID string) string { return "/v1/sessions/" + botID + "/novnc" }

// isNoVNCPath reports whether a path is the proxied noVNC route, which is the only
// place a token in a query parameter is accepted.
func isNoVNCPath(path string) bool {
	return strings.Contains(path, "/novnc/") || strings.HasSuffix(path, "/novnc")
}

// newNoVNCProxy builds the reverse proxy. It is one proxy for every session: the
// target comes from the request, which is why the handler puts it in the context
// before it calls this.
func (s *Server) newNoVNCProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			route, ok := request.In.Context().Value(novncRouteKey{}).(novncRoute)
			if !ok {
				// Unreachable through the handler; leaving the outbound URL alone would
				// send the request to the control plane itself, so it is refused loudly.
				request.Out.URL = &url.URL{Path: "/"}
				return
			}

			request.Out.URL.Scheme = route.target.Scheme
			request.Out.URL.Host = route.target.Host
			request.Out.URL.Path = joinPath(route.target.Path, strings.TrimPrefix(request.In.URL.Path, route.prefix))
			// The token the browser put in the URL is dropped here. It is the control
			// plane's own secret, the desktop has no use for it, and a query string
			// ends up in logs.
			request.Out.URL.RawQuery = stripToken(request.In.URL.Query()).Encode()
			request.Out.Host = route.target.Host
			request.SetXForwarded()

			// The control plane's own credentials do not travel into a sandbox: the
			// desktop needs neither the bearer token nor the cookie, and a sandbox is
			// the one place in this system that runs code a model asked for.
			request.Out.Header.Del("Authorization")
			request.Out.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
				// The browser went away mid-upgrade, which is what closing a tab looks
				// like. There is nobody left to answer.
				return
			}
			s.log.Printf("novnc: proxy: %v", err)
			s.writeError(w, http.StatusBadGateway, "the session's desktop is not reachable")
		},
	}
}

// joinPath appends a stripped path to the target's base path, keeping exactly one
// slash between them.
func joinPath(base, path string) string {
	switch {
	case base == "" || base == "/":
		if path == "" {
			return "/"
		}
		return path
	case strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/"):
		return base + strings.TrimPrefix(path, "/")
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(path, "/"):
		return base + "/" + path
	default:
		return base + path
	}
}

// stripToken removes the control plane's token from the query a sandbox receives.
func stripToken(query url.Values) url.Values {
	if query.Has("token") {
		query.Del("token")
	}
	return query
}

// bearerToken reads the Authorization header, and answers "" for anything that is not
// a bearer token.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// cookieToken reads the cookie a noVNC page was handed.
func cookieToken(r *http.Request) string {
	cookie, err := r.Cookie(tokenCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// decode reads a JSON body, and writes the refusal itself so every caller reads the
// same way. An empty body is accepted: ensure has one optional field, and the actions
// that need one validate their own.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	limited := http.MaxBytesReader(w, r.Body, maxBody)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		s.writeError(w, http.StatusBadRequest, "the request body is not valid JSON for this route: "+err.Error())
		return false
	}
	return true
}

// fail maps a runtime error to a status code and a message. The mapping is the
// whole error contract of this surface, so it lives in one place.
func (s *Server) fail(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, sandbox.ErrNoBotID),
		errors.Is(err, sandbox.ErrInvalidBotID),
		errors.Is(err, sandbox.ErrNoCommand),
		errors.Is(err, sandbox.ErrNoURL),
		errors.Is(err, sandbox.ErrInvalidURL):
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sandbox.ErrNoSession):
		s.writeError(w, http.StatusNotFound, err.Error())
	default:
		s.log.Printf("%s: %v", action, err)
		s.writeError(w, http.StatusInternalServerError, "the sandbox runtime failed")
	}
}

// writeError answers one error object.
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// writeJSON answers one object. A failure here is unactionable: the status line is
// already on the wire, so the only thing left to do is stop writing.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
