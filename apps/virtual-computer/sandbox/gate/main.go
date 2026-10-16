// Command vcomputer-gate is the only door into a sandbox container.
//
// The sandbox is where this system runs the code a model asked for, and the control
// plane is the only process that may talk to it. The gate is what makes that true
// rather than hopeful: it listens on the container's address (GATE_PORT), refuses
// every request that does not carry the session's secret, and forwards the rest to
// the two loopback services the desktop already runs — Chromium's DevTools endpoint
// on 9222 and noVNC's websockify on NOVNC_PORT. Without it both were reachable by
// anything that could reach the container's address on the sandbox's Docker network
// (entrypoint.sh publishes socat on CDP_PORT with no check at all), and a DevTools
// endpoint is a shell: it navigates, evaluates script, and reads every cookie the bot
// is logged in with.
//
// Two things make this more than a line in a firewall rule, and both are why it is a
// Go process rather than another socat:
//
//   - the secret has to be compared in constant time, because a comparison that
//     answers faster for a wrong prefix hands the secret over one byte at a time;
//   - the proxy has to survive a websocket upgrade, because noVNC's desktop *is* a
//     websocket, and it has to rewrite the Host header, because Chromium refuses a
//     DevTools request whose Host is neither an IP nor localhost.
//
// It reads three variables at startup and nothing else: SANDBOX_SECRET (required —
// the process refuses to start without it, since a gate with no secret is an open
// DevTools endpoint), GATE_PORT (default 8443) and NOVNC_PORT (default 6080). No
// secret is baked into the image; the control plane injects one per session at
// container start, the same way it injects the two ports.
//
// Run: SANDBOX_SECRET=... vcomputer-gate
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// secretHeader is where the control plane presents the session's secret. A header
	// rather than a query parameter, because a query string ends up in logs and this
	// secret is the only thing between the sandbox network and a browser that can be
	// driven.
	secretHeader = "X-Sandbox-Secret"

	// healthPath is answered without the secret. The container healthcheck and a
	// human debugging a container both need a way to ask "is the gate up" that does
	// not require the session's secret, and the answer says nothing about the session.
	healthPath = "/healthz"

	// cdpUpstream is the loopback address Chromium itself listens on, and it is a
	// constant rather than configuration because it never varies: entrypoint.sh starts
	// Chromium with --remote-debugging-port=$CDP_LOCAL_PORT, and the image's
	// CDP_LOCAL_PORT is 9222. The gate is inside the container, so it talks to
	// loopback directly.
	cdpUpstream = "127.0.0.1:9222"

	// cdpPrefix and novncPrefix are the two doors. The prefix is stripped on the way
	// through: /cdp/json/version has to arrive at Chromium as /json/version, since the
	// upstream knows nothing about the gate's routes. The bare prefix without its
	// trailing slash is a door too — the control plane's own route for a session is
	// exactly /v1/sessions/<bot>/cdp — and it forwards as "/" rather than as an empty
	// path, which is not a request line.
	cdpPrefix   = "/cdp/"
	novncPrefix = "/novnc/"

	// upstreamHost is the Host both upstreams are addressed with. Chromium rejects a
	// DevTools request whose Host header is neither an IP nor "localhost" (it answers
	// 500), which is why the connection still goes to 127.0.0.1 while the header says
	// localhost. websockify does not care, but it gets the same treatment so that both
	// upstreams see one shape.
	upstreamHost = "localhost"

	// defaultGatePort and defaultNOVNCPort are the numbers the stack agrees on: the
	// control plane dials the gate on 8443 (VCOMPUTER_SANDBOX_GATE_PORT) and
	// entrypoint.sh runs websockify on 6080. They are defaults, not hardcoded values —
	// the container is passed both per session.
	defaultGatePort  = "8443"
	defaultNOVNCPort = "6080"
)

// config is everything the gate needs to know before it can serve, read once at
// startup so that a missing secret is a refusal to start rather than a 401 on every
// request forever.
type config struct {
	secret    string
	gatePort  string
	novncPort string

	// cdpURL and novncURL are the two upstreams the proxies forward to. They are
	// fields rather than constants so the tests can point the gate at their own
	// httptest servers; in the container parseConfig builds them from the constants
	// above and the NOVNC_PORT that was read.
	cdpURL   *url.URL
	novncURL *url.URL
}

func main() {
	if err := run(os.Args, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "vcomputer-gate: %v\n", err)
		os.Exit(1)
	}
}

// run is main with the exit taken out: the environment is a parameter rather than
// os.Getenv and the failure is a return value rather than an exit, so the whole
// startup path — the refusal in particular — can be tested in-process. It blocks for
// as long as the container lives.
func run(args []string, getenv func(string) string) error {
	logger := log.New(os.Stderr, programName(args)+" ", log.LstdFlags|log.Lmsgprefix)

	cfg, err := parseConfig(getenv)
	if err != nil {
		return err
	}

	// Listen before serving, so a port that is already taken is a startup error the
	// container log shows rather than a process that looks alive and answers nothing.
	addr := "0.0.0.0:" + cfg.gatePort
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	server := &http.Server{
		Handler: newGate(cfg, logger),
		// The only deadline worth having: it bounds how long a half-open connection
		// can hold a slot. There is deliberately no read or write timeout, because a
		// noVNC session is a websocket a human keeps open for as long as they are
		// watching and a CDP call can block until the page answers — any fixed
		// deadline would cut a live session off.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger,
	}

	logger.Printf("listening on %s: %s -> %s, %s -> %s", addr, cdpPrefix, cfg.cdpURL, novncPrefix, cfg.novncURL)
	// The gate holds no state worth draining, so there is no graceful shutdown here:
	// when the container stops, the gate stops with it.
	return server.Serve(listener)
}

// programName is the prefix the gate's log lines carry. args is main's argv: the gate
// takes no flags, but the name of the binary that was actually started is what makes
// a gate line distinguishable from entrypoint.sh and Chromium in the one log stream
// the container produces, which is what the control plane tails.
func programName(args []string) string {
	if len(args) == 0 || args[0] == "" {
		return "vcomputer-gate"
	}
	return filepath.Base(args[0])
}

// parseConfig reads the environment. The secret is the only required value: without
// it the gate would have to either refuse everything (useless) or accept everything
// (an open door to a browser that can be driven), so an empty one is a hard failure.
func parseConfig(getenv func(string) string) (config, error) {
	cfg := config{
		secret:    getenv("SANDBOX_SECRET"),
		gatePort:  portOr(getenv("GATE_PORT"), defaultGatePort),
		novncPort: portOr(getenv("NOVNC_PORT"), defaultNOVNCPort),
	}
	if cfg.secret == "" {
		return config{}, errors.New("SANDBOX_SECRET is required: it is the only thing between the sandbox network and this bot's browser")
	}

	// The two upstreams are assembled here rather than parsed from the environment:
	// both are loopback addresses inside this container, and the only part that varies
	// per session is the noVNC port. Their scheme is http because neither upstream is
	// reachable from anywhere but this container, so there is no certificate to have.
	cfg.cdpURL = &url.URL{Scheme: "http", Host: cdpUpstream}
	cfg.novncURL = &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", cfg.novncPort)}

	return cfg, nil
}

// portOr falls back to the default when a port is missing or is not a number, the
// same way the control plane reads the ports it passes in: a typo should leave the
// gate on the number the stack agrees on rather than produce an address nothing can
// parse, and the container log shows the port that was actually used.
func portOr(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	if _, err := strconv.Atoi(trimmed); err != nil {
		return fallback
	}
	return trimmed
}

// newGate builds the whole surface as one handler: the routing, the secret check, and
// the two proxies. It is a function rather than the body of run so that a test can
// drive every route — the websocket upgrade included — against httptest upstreams,
// with no listener on the container's address.
func newGate(cfg config, logger *log.Logger) http.Handler {
	// One transport for both proxies. It starts as a copy of the standard one, which
	// is where the timeouts and the connection pool worth having come from, and then
	// loses the environment proxy: a sandbox has no egress, and nothing here should
	// ever be routed through something that is not this container.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil

	return &gate{
		secret: cfg.secret,
		cdp:    newUpstreamProxy(cfg.cdpURL, cdpPrefix, transport, logger),
		novnc:  newUpstreamProxy(cfg.novncURL, novncPrefix, transport, logger),
	}
}

// gate is the handler newGate returns: the two proxies and the one secret they share.
type gate struct {
	secret string
	cdp    *httputil.ReverseProxy
	novnc  *httputil.ReverseProxy
}

// ServeHTTP answers the health check and forwards everything else that carries the
// secret. The check comes before the routing, so a caller without the secret cannot
// even learn which routes exist, and no request that fails it can reach an upstream.
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthPath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	}

	if !g.authorized(r) {
		writeUnauthorized(w)
		return
	}

	proxy, ok := g.route(r.URL.Path)
	if !ok {
		// The gate is not a general forwarder: a path that is neither door is a 404
		// rather than a guess at what the caller meant. /json/list, the CDP path
		// without its prefix, is the case this catches.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	proxy.ServeHTTP(w, r)
}

// route picks the proxy for a path and reports whether the path is one of the two
// doors at all.
func (g *gate) route(path string) (*httputil.ReverseProxy, bool) {
	switch {
	case isDoor(path, cdpPrefix):
		return g.cdp, true
	case isDoor(path, novncPrefix):
		return g.novnc, true
	default:
		return nil, false
	}
}

// isDoor reports whether a path belongs to a door. The bare prefix counts as well as
// anything under it: the control plane's route for a session is exactly
// /v1/sessions/<bot>/cdp, so that is the path its proxy sends when a caller asks for
// the route itself, and a 404 there would be a door shut on its own doorstep. What
// that forwards to is the upstream's root, which may answer 404 as well — but it is
// the upstream answering, which is a different fact from the gate not knowing the
// path.
func isDoor(path, prefix string) bool {
	return path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix)
}

// authorized compares the presented secret in constant time. This is the one check
// that keeps the sandbox network away from a browser that can be driven, and a
// comparison that returned early on the first wrong byte would let an attacker find
// the secret one byte at a time; ConstantTimeCompare also answers 0 for a wrong
// length, so a short guess is not distinguished from a long one.
func (g *gate) authorized(r *http.Request) bool {
	presented := r.Header.Get(secretHeader)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(g.secret)) == 1
}

// writeUnauthorized is the answer to a request without the session's secret. JSON,
// so a caller parses one error shape across the control plane and the gate, and it
// says nothing about the routes: a caller that cannot present the secret is not
// allowed to know them.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, "{\"error\":\"unauthorized\"}\n")
}

// newUpstreamProxy builds one of the two proxies. Both do the same three things,
// which is why they are one function: strip the route prefix, present the Host header
// Chromium insists on, and survive a websocket upgrade.
//
// The upgrade needs no websocket library here: httputil.ReverseProxy hijacks the
// downstream connection and copies bytes in both directions once the upstream answers
// 101, which is exactly what noVNC's websockify and CDP's page sockets are. It also
// needs no separate handling of the request body or the method — the proxy clones the
// incoming request, so both survive by construction.
func newUpstreamProxy(target *url.URL, prefix string, transport http.RoundTripper, logger *log.Logger) *httputil.ReverseProxy {
	// What the log calls this proxy, and what comes off the path: the route without its
	// trailing slash. Both are derived from the prefix rather than passed in, so the two
	// call sites cannot disagree with the route they are mounted on.
	name := strings.Trim(prefix, "/")
	route := strings.TrimSuffix(prefix, "/")

	return &httputil.ReverseProxy{
		Transport: transport,
		// -1 flushes to the client after every write instead of buffering, which is
		// what keeps a CDP answer that arrives in pieces from sitting in the proxy
		// while the caller waits for a complete body.
		FlushInterval: -1,
		Rewrite: func(request *httputil.ProxyRequest) {
			// The route comes off the path and off the escaped path together: if the
			// two disagree, the escaped one wins and the request would reach the
			// upstream still carrying /cdp/. What is left keeps its own leading slash
			// — /cdp/json/version has to arrive as /json/version, because a request
			// line whose path does not start with a slash is not a path at all and
			// the upstream answers 400 to it. The query string is left alone, so it
			// travels with the request.
			//
			// A request for the bare door strips to the empty string, and an empty path
			// is not a request line either: it becomes the upstream's root, which is
			// the only thing it could have meant.
			path := strings.TrimPrefix(request.In.URL.Path, route)
			escaped := strings.TrimPrefix(request.In.URL.RawPath, route)
			if path == "" {
				path = "/"
			}
			request.Out.URL.Path = path
			request.Out.URL.RawPath = escaped

			request.Out.URL.Scheme = target.Scheme
			request.Out.URL.Host = target.Host
			// The outgoing Host header is not the address we dial: Chromium refuses a
			// DevTools request whose Host is neither an IP nor localhost.
			request.Out.Host = upstreamHost
		},
		// The proxy's own diagnostics go to the same log as the gate's, so the container
		// has one stream to read when a session misbehaves.
		ErrorLog: logger,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
				// The caller went away, which for a noVNC session is a closed tab.
				// There is nobody left to answer, and logging it would fill the
				// container log with one line per closed tab.
				return
			}
			logger.Printf("%s: %v", name, err)
			http.Error(w, "the sandbox's "+name+" upstream is not reachable", http.StatusBadGateway)
		},
	}
}
