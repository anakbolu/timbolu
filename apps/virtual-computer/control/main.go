// Command vcomputer-control is the PoC control plane for a bot's virtual
// computer.
//
// It is one small HTTP process with one job: hold the session lifecycle and the
// desktop actions a model can ask for — screenshot, click, type, navigate, exec —
// and answer them over an address the Temporal worker and a browser reach. It is a
// separate module from apps/backend on purpose: the backend's build must not
// change because a PoC exists (docs/adr/0003-virtual-computer.md §3.2), and this
// process has no Temporal, no Postgres, and no product code in it.
//
// Two runtimes, one surface. `-runtime memory` answers without Docker and records
// what it was asked to do, which is how the surface is tested and how the process
// starts on a laptop. `-runtime docker` runs the real sandbox containers through the
// docker CLI, which is the configuration the compose file ships.
//
// The control plane can start a sandbox and run a command inside it, so it is not
// open: VCOMPUTER_CONTROL_TOKEN is required, and the process refuses to start
// without it rather than coming up as an unauthenticated way to reach every bot's
// desktop. Only /health is answered without it.
//
// Run: VCOMPUTER_CONTROL_TOKEN=... go run . -runtime docker
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

	"github.com/anakbolu/timbolu/apps/virtual-computer/control/internal/httpapi"
	"github.com/anakbolu/timbolu/apps/virtual-computer/control/internal/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vcomputer-control: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", defaultAddr(), "listen address; keep it on the internal network")
	runtimeName := flag.String("runtime", defaultRuntime(), "sandbox runtime: docker (real containers) or memory (records actions, needs no Docker)")
	shutdownGrace := flag.Duration("shutdown-grace", 5*time.Second, "how long to let in-flight requests finish on SIGTERM")
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("VCOMPUTER_CONTROL_TOKEN"))
	if !httpapi.ValidToken(token) {
		return errors.New("VCOMPUTER_CONTROL_TOKEN is required: it is the only thing between the network and every bot's desktop")
	}

	runtime, err := newRuntime(*runtimeName)
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "vcomputer-control ", log.LstdFlags|log.Lmsgprefix)
	server := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.New(runtime, logger, httpapi.WithToken(token)),
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

// newRuntime picks the implementation. Both are real: the memory runtime answers the
// same surface without a container, which is what makes the surface testable on any
// machine, and the docker runtime runs the sandbox the compose file builds.
//
// The docker runtime reads its own configuration (image, network, ports, docker
// binary, exec timeout) from the environment, so there is nothing to pass here.
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
