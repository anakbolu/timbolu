// Package httpapi is the control plane's HTTP surface: no state of its own, and one
// Runtime behind it.
//
// The shape is deliberate. Every route names the bot it acts on, so a caller can
// never reach another bot's session by omission; every action is its own route
// rather than one "run anything" endpoint, because the approval gates depend on
// being able to label them separately (docs/riset-microvm.md §5.1); and the whole
// surface is small enough to read in one sitting, which is what a PoC is for.
//
// Two things cross this surface that are not JSON: a screenshot, which is PNG bytes,
// and two proxied doors into a sandbox — the noVNC client, so a browser can watch the
// desktop, and the DevTools endpoint, so a worker can drive the browser. Both go to
// the sandbox's gate, which is the only thing inside that network allowed to reach the
// desktop's ports; the control plane attaches the sandbox's secret to each request.
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
	"strconv"
	"strings"

	"github.com/robzlabz/employeebot/apps/virtual-computer/control/internal/sandbox"
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
	// proxy forwards the noVNC client and the DevTools endpoint to a session's gate.
	// One proxy serves every session and both doors: the target, the prefix to strip,
	// and the secret to attach are per request, and they travel in the request context.
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
	s.proxy = s.newGateProxy()

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
	// the sandbox's gate, which is the only thing inside the sandbox network that may
	// reach the desktop's ports.
	mux.Handle("GET /v1/sessions/{bot_id}/novnc/{rest...}", http.HandlerFunc(s.novnc))
	// The DevTools endpoint is proxied the same way, so a worker holding the control
	// token can drive the browser without ever knowing the container's address or
	// holding the sandbox's secret. The bare path answers too: it is what Session.CDPURL
	// hands a caller, and the DevTools API is then reached under it.
	//
	// No method is named, and that is deliberate: the DevTools HTTP API is not
	// method-uniform — /json/list and /json/version are GETs, /json/new is a PUT, and a
	// target's /json/close is a GET — so a door that listed methods would hand a caller
	// a 405 for a request it is entitled to make. Whatever arrives is forwarded as it
	// arrived; the gate is what decides, and its upstream answers the 405 when there
	// should be one.
	mux.Handle("/v1/sessions/{bot_id}/cdp", http.HandlerFunc(s.cdp))
	mux.Handle("/v1/sessions/{bot_id}/cdp/{rest...}", http.HandlerFunc(s.cdp))
	// Cleanup is the one route that is not about a single bot: it removes what a
	// control plane that died mid-session left behind.
	mux.HandleFunc("POST /v1/admin/cleanup", s.cleanup)

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
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.open || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		if token := r.URL.Query().Get("token"); token != "" && isNoVNCPath(r.URL.Path) {
			if !s.tokenMatches(token) {
				s.writeError(w, http.StatusUnauthorized, "the token is not valid")
				return
			}
			s.setTokenCookie(w, r, token)
			next.ServeHTTP(w, r)
			return
		}

		if s.tokenMatches(bearerToken(r)) || s.tokenMatches(cookieToken(r)) {
			next.ServeHTTP(w, r)
			return
		}
		s.writeError(w, http.StatusUnauthorized, "a bearer token is required")
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
//
// `?purge=true` is the other half of that bargain: it removes the container and the
// home volume, which is what "this bot is gone" needs and what makes a session
// reclaimable once its profile has grown or gone stale. The two are one route because
// they are one intent — stop this session — with one flag deciding whether the browser
// profile survives it.
func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	botID := r.PathValue("bot_id")
	if r.URL.Query().Get("purge") == "true" {
		if err := s.runtime.Purge(r.Context(), botID); err != nil {
			s.fail(w, "purge session", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "purged": true})
		return
	}

	if err := s.runtime.Stop(r.Context(), botID); err != nil {
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

// navigate points the session's browser at a URL, and answers where the browser
// actually ended up.
//
// The scheme rule lives in the runtime, which is where it is enforced for both
// runtimes: the memory runtime records the navigation and refuses the same URLs the
// docker runtime refuses, so a caller cannot discover the difference by trying.
//
// The answer is not "the command was sent": the runtime waits until the document has
// finished loading, so a page that ended on the browser's error page, that answered
// 4xx/5xx, or that never finished, is a failure the caller can see. `final_url` is
// where the browser went rather than what was asked for, because a redirect is a
// normal outcome and a caller holding only the request URL cannot tell that one
// happened. `http_status` is the status of the response the document came from, which
// is what makes "the fixture served the page" checkable rather than assumed: 0 means
// the browser reported none, e.g. an about: or data: document.
func (s *Server) navigate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !s.decode(w, r, &body) {
		return
	}

	navigation, err := s.runtime.Navigate(r.Context(), r.PathValue("bot_id"), body.URL)
	if err != nil {
		s.fail(w, "navigate", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"navigated":   true,
		"final_url":   navigation.FinalURL,
		"title":       navigation.Title,
		"http_status": navigation.HTTPStatus,
	})
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

// novnc forwards the noVNC client to the session's sandbox, through its gate.
//
// The browser reaches one origin — this one — and the sandbox network stays
// unreachable from outside (docs/adr/0003-virtual-computer.md §3.8). The proxy is
// httputil's, so the websocket upgrade that carries the framebuffer goes through the
// same path as the page itself.
func (s *Server) novnc(w http.ResponseWriter, r *http.Request) {
	s.proxyToGate(w, r, novncDoor, false)
}

// cdp forwards the DevTools HTTP API to the session's browser, through its gate.
//
// It exists so that a worker holding the control token can drive the browser without
// ever learning the container's address or holding the sandbox's secret: the gate
// demands that secret, and the control plane is the only thing that has it. The
// answers are passed through as they are, except that the addresses Chromium names for
// itself are rewritten to point back here — otherwise a caller would be handed a
// loopback address inside the sandbox network that it cannot dial.
func (s *Server) cdp(w http.ResponseWriter, r *http.Request) {
	s.proxyToGate(w, r, cdpDoor, true)
}

// cleanup removes the sandboxes a control plane that died mid-session left behind.
//
// `?volumes=true` extends it to the home volumes, which is the more destructive half
// and so is not the default. Running sessions are never touched: a sandbox that is up
// belongs to somebody, and a cleanup that killed it would be the opposite of what the
// word means.
func (s *Server) cleanup(w http.ResponseWriter, r *http.Request) {
	volumes := r.URL.Query().Get("volumes") == "true"

	result, err := s.runtime.Cleanup(r.Context(), volumes)
	if err != nil {
		s.fail(w, "cleanup", err)
		return
	}

	answer := map[string]any{
		"removed_containers": result.RemovedContainers,
		"removed_volumes":    result.RemovedVolumes,
	}
	// The key is absent when nothing failed, so a caller can tell "everything I
	// selected was removed" from "some of it was not" without reading a list.
	if len(result.Failed) > 0 {
		answer["failed"] = result.Failed
	}
	writeJSON(w, http.StatusOK, answer)
}

// The two doors the sandbox's gate publishes. They are also the suffixes of the
// control-plane routes that reach them, which is what lets one prefix strip serve
// both.
const (
	novncDoor = "/novnc"
	cdpDoor   = "/cdp"
)

// gateRoute is where one proxied request is going: the session's gate, the
// control-plane prefix to strip off the path, the secret that gate demands, and —
// for the DevTools door — where the caller thinks it is, which is what the rewritten
// debugger addresses have to point at.
type gateRoute struct {
	target *url.URL
	prefix string
	secret string
	host   string
	secure bool
	cdp    bool
}

// gateRouteKey is the context key the proxy's rewrite step reads.
type gateRouteKey struct{}

// controlPrefix is the path on this control plane under which one gate door is
// reached for one bot. It is also the prefix the proxy strips before the request
// goes out, which is why it is built in one place.
func controlPrefix(botID, door string) string { return "/v1/sessions/" + botID + door }

// isNoVNCPath reports whether a path is the proxied noVNC route, which is the only
// place a token in a query parameter is accepted.
func isNoVNCPath(path string) bool {
	return strings.Contains(path, "/novnc/") || strings.HasSuffix(path, "/novnc")
}

// proxyToGate resolves the session's gate and hands the request to the proxy.
//
// Both doors go through here so that the secret is attached in exactly one place: a
// route that forgot it would be a route the gate answers 401 to, which is a bug that
// would only show up in a deployment.
func (s *Server) proxyToGate(w http.ResponseWriter, r *http.Request, door string, cdp bool) {
	botID := r.PathValue("bot_id")

	target, err := s.runtime.GateTarget(r.Context(), botID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNoSession) {
			// There is no session to proxy to, which is a 404 and the caller's mistake.
			s.fail(w, "gate target", err)
			return
		}
		// The session is there but its gate is not — stopped, or never came up. That
		// is the same answer as a gate that refuses the connection, because it is the
		// same fact: the browser behind this route cannot be reached.
		s.log.Printf("proxy: %v", err)
		s.writeError(w, http.StatusBadGateway, "the session's browser is not reachable")
		return
	}

	address, err := url.Parse(target.URL)
	if err != nil || address.Host == "" {
		s.log.Printf("proxy: the runtime answered an unusable gate target %q: %v", target.URL, err)
		s.writeError(w, http.StatusBadGateway, "the session's browser is not reachable")
		return
	}
	// The gate publishes each door under its own prefix, so the target carries it and
	// the stripped remainder is appended to it.
	address.Path = door

	route := gateRoute{
		target: address,
		prefix: controlPrefix(botID, door),
		secret: target.Secret,
		host:   r.Host,
		secure: r.TLS != nil,
		cdp:    cdp,
	}
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), gateRouteKey{}, route)))
}

// newGateProxy builds the reverse proxy both doors share. It is one proxy for every
// session: the target comes from the request, which is why the handler puts it in the
// context before it calls this.
func (s *Server) newGateProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			route, ok := request.In.Context().Value(gateRouteKey{}).(gateRoute)
			if !ok {
				// Unreachable through the handler; leaving the outbound URL alone would
				// send the request to the control plane itself, so it is refused loudly.
				request.Out.URL = &url.URL{Path: "/"}
				return
			}

			request.Out.URL.Scheme = route.target.Scheme
			request.Out.URL.Host = route.target.Host
			request.Out.URL.Path = joinPath(route.target.Path, strings.TrimPrefix(request.In.URL.Path, route.prefix))
			// The incoming request's own escaping described the path that was just
			// replaced, so it is dropped rather than left to disagree with it.
			request.Out.URL.RawPath = ""
			// The token the browser put in the URL is dropped here. It is the control
			// plane's own secret, the sandbox has no use for it, and a query string
			// ends up in logs.
			request.Out.URL.RawQuery = stripToken(request.In.URL.Query()).Encode()
			request.Out.Host = route.target.Host
			request.SetXForwarded()

			// The sandbox's own secret is what its gate asks for, and the control
			// plane is the only thing holding it.
			request.Out.Header.Set(sandbox.SecretHeader, route.secret)

			// The control plane's own credentials do not travel into a sandbox: the
			// desktop needs neither the bearer token nor the cookie, and a sandbox is
			// the one place in this system that runs code a model asked for.
			request.Out.Header.Del("Authorization")
			request.Out.Header.Del("Cookie")
		},
		ModifyResponse: func(response *http.Response) error {
			// The gate refusing the secret is a sandbox-side failure, and it is the one
			// status that must not be passed through: the control plane attached that
			// secret itself, so a 401 from behind it says nothing about the caller's
			// token — and a caller that read it as "my token is wrong" would go looking
			// in the wrong place. It is the same fact as a gate that is not listening,
			// so it gets the same answer.
			//
			// This reads every 401 as the gate's own, which is true because nothing
			// behind it authenticates: Chromium's DevTools endpoint has no credentials,
			// and websockify is started without --auth, so the gate is the only thing
			// on that side that can answer 401. A deployment that put a password on
			// websockify would have to tell the two apart here.
			if response.StatusCode == http.StatusUnauthorized {
				s.log.Printf("proxy: the session's gate refused the sandbox secret")
				return replaceBody(response, http.StatusBadGateway, `{"error":"the session's browser is not reachable"}`)
			}

			route, ok := response.Request.Context().Value(gateRouteKey{}).(gateRoute)
			if !ok || !route.cdp {
				return nil
			}
			return rewriteDebuggerURLs(response, route)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
				// The browser went away mid-upgrade, which is what closing a tab looks
				// like. There is nobody left to answer.
				return
			}
			s.log.Printf("proxy: %v", err)
			s.writeError(w, http.StatusBadGateway, "the session's browser is not reachable")
		},
	}
}

// replaceBody swaps a response for one this surface produced itself: the status, a
// fixed body, and the headers that describe it. It is used where an answer from
// behind the proxy must not reach the caller as it is, and it closes the original
// body rather than leaving the connection behind it unread.
func replaceBody(response *http.Response, status int, body string) error {
	if response.Body != nil {
		_ = response.Body.Close()
	}
	response.StatusCode = status
	response.Status = strconv.Itoa(status) + " " + http.StatusText(status)
	response.Body = io.NopCloser(strings.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Type", "application/json; charset=utf-8")
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// maxDebuggerBody bounds what the DevTools rewrite will read. Every answer it applies
// to is a small JSON document — a target list, one target, the browser's version — and
// anything larger is passed through untouched rather than buffered.
const maxDebuggerBody = 1 << 20

// rewriteDebuggerURLs replaces the addresses Chromium names for itself with the route
// the caller actually reached this through.
//
// `/json/list`, `/json/new`, and `/json/version` each answer a `webSocketDebuggerUrl`
// pointing at the browser's own loopback address — `ws://127.0.0.1:9222/devtools/...`
// — which is inside the sandbox network and dialable by nothing outside it. A caller
// that took that address at face value would be handed a connection it cannot make, so
// the path is kept (it is what names the target) and the address becomes this route on
// the host the caller used.
//
// Anything that is not a small JSON body is passed through byte for byte: a body this
// function does not understand is one it must not touch.
func rewriteDebuggerURLs(response *http.Response, route gateRoute) error {
	if response.Body == nil || !strings.Contains(response.Header.Get("Content-Type"), "json") {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxDebuggerBody+1))
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	// A read that hit the cap means the answer was larger than this rewrite is willing
	// to buffer, so it is passed on exactly as it came.
	if len(body) > maxDebuggerBody {
		response.Body = io.NopCloser(io.MultiReader(strings.NewReader(string(body)), response.Body))
		response.ContentLength = -1
		response.Header.Del("Content-Length")
		return nil
	}

	restored := body
	var document any
	if err := json.Unmarshal(body, &document); err == nil {
		if rewritten, err := json.Marshal(rewriteAddresses(document, route)); err == nil {
			restored = rewritten
		}
	}

	response.Body = io.NopCloser(strings.NewReader(string(restored)))
	response.ContentLength = int64(len(restored))
	response.Header.Set("Content-Length", strconv.Itoa(len(restored)))
	return nil
}

// rewriteAddresses walks a decoded JSON document and rewrites every string that is a
// debugger websocket address. It walks rather than reading known fields because the
// addresses appear at different depths in the different answers, and one added by a
// future Chromium would otherwise be passed through unreachable.
func rewriteAddresses(value any, route gateRoute) any {
	switch typed := value.(type) {
	case string:
		return rewriteAddress(typed, route)
	case []any:
		for i := range typed {
			typed[i] = rewriteAddresses(typed[i], route)
		}
		return typed
	case map[string]any:
		for key, entry := range typed {
			typed[key] = rewriteAddresses(entry, route)
		}
		return typed
	default:
		return value
	}
}

// rewriteAddress rewrites one address, and answers anything that is not a debugger
// websocket address unchanged.
func rewriteAddress(value string, route gateRoute) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return value
	}
	if !strings.HasPrefix(parsed.Path, "/devtools/") {
		return value
	}

	scheme := "ws"
	if route.secure {
		scheme = "wss"
	}
	return scheme + "://" + route.host + route.prefix + parsed.Path
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
//
// A failed navigation is its own case because it is the one failure that carries
// facts the caller needs: which URL was asked for, where the browser ended up
// instead, and — when the server answered at all — the status it answered with.
// 502 rather than 400 because the request was well formed — the browser could not
// carry it out, which is not the caller's mistake.
func (s *Server) fail(w http.ResponseWriter, action string, err error) {
	var navigation *sandbox.NavigationError

	switch {
	case errors.As(err, &navigation):
		s.log.Printf("%s: %v", action, err)
		answer := map[string]any{
			"error":     "navigation failed",
			"url":       navigation.URL,
			"final_url": navigation.FinalURL,
		}
		// Left out when the browser reported none rather than answered 0: a caller
		// branching on the status has to be able to tell "the server said 404" from
		// "nothing here knows a status", and an omitted key says the second.
		if navigation.HTTPStatus != 0 {
			answer["http_status"] = navigation.HTTPStatus
		}
		writeJSON(w, http.StatusBadGateway, answer)
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
