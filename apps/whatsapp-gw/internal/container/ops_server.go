package container

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// The gateway's operational listener: the surface a process with no public API
// answers probes and scrapes on.
//
// It is separate from the seam listener, which speaks for every tenant and is
// reached with a shared secret. A probe is infrastructure rather than a tenant:
// it has no workspace and no token, so it must not be behind the same guard.
//
//   - `/health` is liveness. It never touches a dependency, so it answers as long
//     as the process is running — the question a restart decision is made on.
//   - `/ready` is readiness. It checks the two dependencies the gateway cannot
//     work without, Postgres and Redis, and answers 503 while either is down. A
//     gateway that cannot reach Postgres must not be told it is ready: it holds
//     sockets, and a rollout that continues would put two clients on one session.
//   - `/metrics` is the scrape endpoint, present only when the deployment named an
//     exporter for it.
//
// It is off unless the deployment names an address, so a gateway run beside a
// local API is unchanged.
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
		// The gateway's job is to hold the connections, and refusing to start
		// because a monitoring port was busy would turn a probe problem into an
		// outage. Said loudly instead, which is what an operator can act on.
		c.Logger.Error("the operations endpoint could not start; the process runs without it",
			zap.String("address", address), zap.Error(err))

		return
	}

	server := &http.Server{
		Handler: c.opsMux(),
		// A probe and a scrape are small reads: the timeouts are short so a stalled
		// client cannot hold a connection the process forgets about.
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

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()

	_ = server.Shutdown(shutdownCtx)
}

// opsMux is the route table of the operations listener.
func (c *Container) opsMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeOps(w, http.StatusOK, "alive", map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		report := c.readiness(r.Context())
		status := http.StatusOK
		if report.Status != "ok" {
			status = http.StatusServiceUnavailable
			c.Logger.Warn("readiness check failed", zap.String("status", report.Status))
		}

		writeOps(w, status, "ready", report)
	})

	// The scrape endpoint exists only when the deployment configured an exporter.
	// A route that answered 503 for a deployment that does not scrape would look
	// like a broken scrape rather than a deployment that does not scrape.
	if c.Metrics != nil && c.Metrics.Handler != nil {
		mux.Handle("GET /metrics", c.Metrics.Handler)
	}

	return mux
}

// readinessReport is what a probe reads back.
type readinessReport struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// readiness answers "can this gateway serve".
//
// The gateway owns a socket and a lease, so the two dependencies it reports on
// are the two that make it safe to run: Postgres, which holds the sessions and
// the directory, and Redis, which holds the lease that keeps a second instance
// off a number.
func (c *Container) readiness(ctx context.Context) readinessReport {
	checks := map[string]string{}
	status := "ok"

	if c.DB == nil {
		checks["database"] = "not_configured"
		status = "not_ready"
	} else if err := c.DB.Ping(ctx); err != nil {
		checks["database"] = err.Error()
		status = "not_ready"
	} else {
		checks["database"] = "ok"
	}

	if c.Redis == nil {
		checks["redis"] = "not_configured"
		status = "not_ready"
	} else if err := c.Redis.Ping(ctx); err != nil {
		checks["redis"] = err.Error()
		status = "not_ready"
	} else {
		checks["redis"] = "ok"
	}

	return readinessReport{Status: status, Checks: checks}
}

// writeOps writes the same envelope the API's probes use, so one monitor reads
// both the same way.
func writeOps(w http.ResponseWriter, status int, message string, data any) {
	envelope := map[string]any{
		"success": status < http.StatusBadRequest,
		"message": message,
		"data":    data,
	}
	if status >= http.StatusBadRequest {
		envelope["error"] = map[string]any{"code": "not_ready"}
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
