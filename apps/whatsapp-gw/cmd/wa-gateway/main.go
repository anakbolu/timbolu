// Command wa-gateway runs the process that owns the WhatsApp connections.
//
// It is separate from the API because a WhatsApp session is a socket that stays
// open: the API serves requests and may be replaced at any moment, and a linked
// number whose connection dies with the request that touched it is a number that
// is never online. This process holds the sockets, persists the sessions
// encrypted, and answers the API over an internal address.
//
// Two things it does not do: it does not decide when a message may leave — the
// product does, and the pacing, the ceiling, and the approval gate are all above
// it — and it is not publicly reachable, because it speaks for every tenant it
// serves.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/container"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// shutdownGrace is how long in-flight requests get after a signal. A drain tick
// is quick, and a session write is one statement, so it is generous rather than
// tuned.
const shutdownGrace = 15 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "wa-gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	env := flag.String("env", "", "environment: local, staging, production (defaults to $ENVIRONMENT, else local)")
	flag.Parse()
	if *env != "" {
		if err := os.Setenv("ENVIRONMENT", *env); err != nil {
			return fmt.Errorf("set environment: %w", err)
		}
	}

	if err := config.LoadDotEnv(); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// This binary names itself in the traces, so the API and the gateway are
	// separate services in a collector rather than one process that appears
	// everywhere.
	cfg.Telemetry.DefaultServiceName("bolu-wa-gateway")

	// The engine needs the database, the key ring, and Redis, and it reaches the
	// product over the internal seam, so the container is assembled with
	// everything it holds and nothing of the API's.
	c, err := container.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()

	// The operations listener is its own surface: the seam listener answers the
	// API, and the probes answer an orchestrator.
	go c.RunOpsServer(ctx)

	address := strings.TrimSpace(cfg.WhatsApp.Address)
	if address == "" {
		return errors.New("whatsapp: no gateway address is configured (set WA_ADDRESS)")
	}

	server := &http.Server{
		Addr:              address,
		Handler:           gatewayMux(c),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// The engine runs alongside the server rather than inside it: it is a set of
	// loops over the connections, and a request only reports what they know.
	engineErr := make(chan error, 1)
	go func() {
		engineErr <- c.Engine.Run(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		c.Logger.Info("wa-gateway listening", zap.String("address", address))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	case err := <-engineErr:
		if err != nil {
			return fmt.Errorf("engine: %w", err)
		}
	case <-ctx.Done():
	}

	// The engine stops with the context and releases its leases on the way out,
	// which is what makes a clean shutdown a takeover in milliseconds rather than
	// a wait for the TTL. The server is given its grace afterwards so a request
	// already in flight is answered.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	select {
	case err := <-engineErr:
		if err != nil {
			return fmt.Errorf("engine: %w", err)
		}
	case <-shutdownCtx.Done():
		return errors.New("shutdown: the engine did not stop in time")
	}

	return nil
}

// gatewayMux is the seam listener's route table: the surface the API reaches the
// engine through. It is one listener and one route table, and the shared secret
// is checked on every route.
func gatewayMux(c *container.Container) *http.ServeMux {
	mux := http.NewServeMux()
	c.Gateway.Routes(mux)

	return mux
}
