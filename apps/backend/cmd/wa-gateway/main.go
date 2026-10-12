// Command wa-gateway runs the process that owns the WhatsApp connections.
//
// It is separate from the API because a WhatsApp session is a socket that stays
// open: the API serves requests and may be replaced at any moment, and a linked
// number whose connection dies with the request that touched it is a number that
// is never online. This process holds the sockets, persists the sessions
// encrypted, and answers the API over an internal address.
//
// Two things it does not do: it does not decide when a message may leave — the
// module does, and the pacing, the ceiling, and the approval gate are all above
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

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
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

	// The engine needs the database, the key ring, and Redis, so the container is
	// assembled with everything except the HTTP surface.
	c, err := container.New(ctx, cfg, container.WithoutHTTP())
	if err != nil {
		return err
	}
	defer c.Close()

	engine, gateway, err := c.OpenWhatsAppRuntime(cfg)
	if err != nil {
		return err
	}

	address := strings.TrimSpace(cfg.WhatsApp.Address)
	if address == "" {
		return errors.New("whatsapp: no gateway address is configured (set WA_ADDRESS)")
	}
	if cfg.WhatsApp.GatewayToken == "" {
		// A gateway with no token would answer anybody who could reach it, and it
		// speaks for every tenant it serves. It refuses to start instead.
		return errors.New("whatsapp: no gateway token is configured (set WA_GATEWAY_TOKEN)")
	}

	server := &http.Server{
		Addr:              address,
		Handler:           c.WhatsAppGatewayMux(gateway),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// The engine runs alongside the server rather than inside it: it is a set of
	// loops over the connections, and a request only reports what they know.
	engineErr := make(chan error, 1)
	go func() {
		engineErr <- engine.Run(ctx)
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
