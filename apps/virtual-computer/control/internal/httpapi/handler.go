// Package httpapi is the control plane's HTTP surface: eight routes, no state of
// its own, and one Runtime behind it.
//
// The shape is deliberate. Every route names the bot it acts on, so a caller can
// never reach another bot's session by omission; every action is its own route
// rather than one "run anything" endpoint, because the approval gates depend on
// being able to label them separately (docs/riset-microvm.md §5.1); and the whole
// surface is small enough to read in one sitting, which is what a PoC is for.
//
// Errors are sentinels from the sandbox package mapped to status codes here, so a
// caller can branch on the status and a reader can see the mapping in one table.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/robzlabz/employeebot/apps/virtual-computer/control/internal/sandbox"
)

// maxBody bounds a request. The largest thing that crosses this surface is a typed
// string or a command, so 64 KiB is generous and still refuses a stream.
const maxBody = 64 << 10

// Server is the control plane's handler.
type Server struct {
	runtime sandbox.Runtime
	log     *log.Logger
}

// New builds the handler. It returns an http.Handler rather than a *Server so the
// caller cannot reach past the routes.
func New(runtime sandbox.Runtime, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	s := &Server{runtime: runtime, log: logger}

	mux := http.NewServeMux()
	// health is the only route that says nothing about a bot. The demo page and the
	// compose healthcheck both use it.
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1/sessions", s.ensureSession)
	mux.HandleFunc("GET /v1/sessions/{bot_id}", s.sessionStatus)
	mux.HandleFunc("DELETE /v1/sessions/{bot_id}", s.stopSession)
	// screenshot answers both ways: POST is the action a worker calls, and GET is the
	// same capture so a plain <img> can embed the desktop. An <img> cannot send a
	// POST, and a status page that cannot show the screen would defeat the point of
	// the PoC.
	mux.HandleFunc("POST /v1/sessions/{bot_id}/screenshot", s.screenshot)
	mux.HandleFunc("GET /v1/sessions/{bot_id}/screenshot", s.screenshot)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/click", s.click)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/type", s.typeText)
	mux.HandleFunc("POST /v1/sessions/{bot_id}/exec", s.exec)

	return withCORS(mux)
}

// withCORS lets the static demo page call this API from a file:// origin.
//
// This is a PoC affordance and nothing more: the page has no build step and no
// server, so without these headers its buttons cannot work at all. The control
// plane is reached over the internal Docker network and never published
// (docs/adr/0003-virtual-computer.md §3.6), so the exposure is the sandbox network
// itself. The first thing a real deployment does is delete this function and put
// the control plane behind the same auth as every other internal seam.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
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

// stopSession ends a session. The volume outlives it, so the next ensure reuses
// the browser profile rather than starting a login from scratch.
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

// exec runs a command inside the sandbox.
//
// The PoC answers with the stub's echo and never touches a shell. It is the route
// riset §5.1 labels write_external and the one that must be behind an approval gate
// before it is wired to anything, which is why it is here as a shape rather than as
// a capability.
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

// decode reads a JSON body, and writes the refusal itself so every caller reads the
// same way. An empty body is accepted: ensure has one optional field, and the four
// actions that need one validate their own.
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
		errors.Is(err, sandbox.ErrNoCommand):
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sandbox.ErrNoSession):
		s.writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, sandbox.ErrNotImplemented):
		// 501, not 500: the route exists and the runtime is a skeleton. A caller that
		// reads the body learns which command the skeleton would have run.
		s.writeError(w, http.StatusNotImplemented, err.Error())
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
