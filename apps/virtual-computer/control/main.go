// Command vcomputer-control is the PoC control plane for a bot's virtual
// computer.
//
// It is one small HTTP process with one job: hold the session lifecycle and the
// four desktop actions a model can ask for — screenshot, click, type, exec — and
// answer them over an internal address the Temporal worker reaches. It is a
// separate module from apps/backend on purpose: the backend's build must not
// change because a PoC exists (docs/adr/0003-virtual-computer.md §3.2), and this
// process has no Temporal, no Postgres, and no product code in it.
//
// The PoC runs the in-memory runtime by default, so the process starts on a laptop
// with no Docker and answers real HTTP. `-runtime docker` selects the skeleton that
// documents the commands it will run and refuses to pretend otherwise.
//
// Run: go run . -addr :8090
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/robzlabz/employeebot/apps/virtual-computer/control/internal/httpapi"
	"github.com/robzlabz/employeebot/apps/virtual-computer/control/internal/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vcomputer-control: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", defaultAddr(), "listen address; keep it on the internal network")
	runtimeName := flag.String("runtime", defaultRuntime(), "sandbox runtime: memory (PoC stub) or docker (skeleton)")
	shutdownGrace := flag.Duration("shutdown-grace", 5*time.Second, "how long to let in-flight requests finish on SIGTERM")
	flag.Parse()

	runtime, err := newRuntime(*runtimeName)
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "vcomputer-control ", log.LstdFlags|log.Lmsgprefix)
	server := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.New(runtime, logger),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a screenshot of a real desktop can take longer than any
		// fixed deadline that would be safe for the other routes, and the PoC would
		// rather report a slow answer than cut one off.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Listen before serving so a port clash is a startup error rather than a silent
	// process that never answers.
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	logger.Printf("listening on %s, runtime=%s", listener.Addr(), runtimeNameOf(runtime))

	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		logger.Print("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-errs
}

// newRuntime picks the implementation. The default is the stub: a PoC that needs a
// Docker socket to start is a PoC nobody runs.
func newRuntime(name string) (sandbox.Runtime, error) {
	switch strings.TrimSpace(strings.ToLower(name)) {
	case "", "memory", "inmemory", "in-memory":
		return sandbox.NewInMemory(), nil
	case "docker":
		return sandbox.NewDocker(), nil
	default:
		return nil, fmt.Errorf("unknown runtime %q: use memory or docker", name)
	}
}

// runtimeNameOf is what the startup line prints. It reads the implementation's own
// name rather than repeating the flag, so the two cannot disagree.
func runtimeNameOf(runtime sandbox.Runtime) string {
	if _, ok := runtime.(*sandbox.InMemoryRuntime); ok {
		return "memory"
	}
	return "docker"
}

// defaultAddr keeps the port out of the public interface unless a deployment says
// otherwise: the control plane speaks for every bot at once, so it is reached from
// the sandbox network only (docs/adr/0003-virtual-computer.md §3.6).
func defaultAddr() string {
	if addr := strings.TrimSpace(os.Getenv("VCOMPUTER_CONTROL_ADDR")); addr != "" {
		return addr
	}
	return ":8090"
}

// defaultRuntime lets the compose file choose without editing the command line.
func defaultRuntime() string {
	if name := strings.TrimSpace(os.Getenv("VCOMPUTER_RUNTIME")); name != "" {
		return name
	}
	return "memory"
}
