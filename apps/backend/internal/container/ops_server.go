package container

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	healthdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/health/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// The operational listener: the surface a process with no HTTP API of its own answers probes and
// scrapes on.
//
// The API answers `/health` and `/ready` through Fiber, because it already has a listener and a
// route table. A worker has neither — it polls Temporal and records most of the product's measures —
// so without this an orchestrator has nothing to probe and a scraper nothing to read:

//   - `/health` is liveness. It never touches a dependency, and it answers as long as the process is
//     running, which is the question a restart decision is made on.
//   - `/ready` is readiness. It checks every configured dependency and answers 503 while one is down,
//     which is the question a load balancer or a rollout is decided on. A worker that cannot reach
//     Postgres must not be told it is ready: a rollout that continues would put more tasks on a
//     process that cannot run them.
//   - `/metrics` is the scrape endpoint, present only when the deployment named an exporter for it.
//
// It is off unless the deployment names an address, so a deployment whose workers have no probe, or
// which pushes its measurements, is unchanged. What it serves is deliberately not the API: a worker
// that grew a general HTTP surface would be a second API with none of the guards.
func (c *Container) RunOpsServer(ctx context.Context) {
	if c == nil || c.Config == nil {
		return
	}

	address := c.Config.Http.OpsAddress
	if address == "" {
		return
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		// The process's job is to run tasks, and refusing to start because a port was busy would
		// turn a monitoring problem into an outage. Said loudly instead, which is what an operator
		// can act on.
		c.Logger.Error("the operations endpoint could not start; the process runs without it",
			zap.String("address", address), zap.Error(err))

		return
	}

	mux := c.opsMux()
	server := &http.Server{
		Handler: mux,
		// A probe and a scrape are small reads: the timeouts are short so a stalled client cannot
		// hold a connection the process forgets about.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	c.Logger.Info("serving the operations endpoint", zap.String("address", listener.Addr().String()))

	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		c.Logger.Warn("the operations endpoint stopped", zap.Error(err))
	case <-ctx.Done():
	}

	// Its own bounded moment: the process is already stopping, and a probe in flight should be
	// answered rather than cut off — a restart that fails its own liveness check is a restart the
	// orchestrator counts as a crash.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()

	_ = server.Shutdown(shutdownCtx)
}

// opsMux is the route table of the operations listener.
func (c *Container) opsMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		c.writeOps(w, http.StatusOK, "alive", c.liveness())
	})

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		report := c.readiness(r.Context())
		status := http.StatusOK
		if report.Status != healthdomain.StatusOK {
			status = http.StatusServiceUnavailable
			c.Logger.Warn("readiness check failed", zap.String("status", report.Status))
		}

		c.writeOps(w, status, "ready", report)
	})

	// The scrape endpoint exists only when the deployment configured an exporter for it. A route
	// that answered 503 for a deployment that does not scrape would look like a broken scrape.
	if c.Metrics != nil && c.Metrics.Handler != nil {
		mux.Handle("GET /metrics", c.Metrics.Handler)
	}

	return mux
}

// liveness answers "is this process alive", and never touches a dependency.
func (c *Container) liveness() healthdomain.Report {
	if c.Services != nil && c.Services.Health != nil {
		return c.Services.Health.Liveness()
	}

	// A process assembled without the health module — a worker whose database is not configured —
	// is still alive, and saying so is the honest answer: what is missing is a dependency, which is
	// readiness rather than liveness.
	return healthdomain.Report{Status: healthdomain.StatusOK}
}

// readiness answers "can this process serve", which is the health module's own report.
func (c *Container) readiness(ctx context.Context) healthdomain.Report {
	if c.Services != nil && c.Services.Health != nil {
		return c.Services.Health.Readiness(ctx)
	}

	return healthdomain.Report{Status: healthdomain.StatusOK}
}

// writeOps writes the same envelope the API's own probes use, so one monitor reads both the same way.
func (c *Container) writeOps(w http.ResponseWriter, status int, message string, data any) {
	envelope := response.APIResponse{Success: status < http.StatusBadRequest, Message: message, Data: data}
	if status >= http.StatusBadRequest {
		envelope.Error = &response.ErrorPayload{Code: "not_ready"}
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		http.Error(w, "the probe could not be rendered", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
