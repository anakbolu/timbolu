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
// On SIGTERM or SIGINT it stops serving and keeps the sandboxes it created, with their
// volumes: a restart is not a teardown, and a sandbox that went away with the process
// would take the bot's home volume — its browser profile and its files — with it
// (docs/adr/0003-virtual-computer.md §3.5, issue #263). Removing them is opt-in through
// VCOMPUTER_REAP_ON_SHUTDOWN=1, for a run that is tearing the stack down rather than
// restarting it; the documented teardown is scripts/cleanup.sh --running --volumes, which
// works whether or not this process got the chance to run (§3.9).
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
	runtimeName := flag.String("runtime", defaultRuntime(), "sandbox runtime: docker (real containers) or memory (records actions, needs no Docker)")
	shutdownGrace := flag.Duration("shutdown-grace", 5*time.Second, "how long to let in-flight requests finish on SIGTERM")
	reapGrace := flag.Duration("reap-grace", 60*time.Second, "how long the sandboxes this control plane created may take to go away, when VCOMPUTER_REAP_ON_SHUTDOWN=1 asks for it")
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

	// The sandboxes are kept, not reaped. This process is restarted for ordinary reasons —
	// `compose restart`, `up -d --force-recreate` for an image upgrade, a daemon restart —
	// and every one of them is a SIGTERM to this process, not a decision that the sessions
	// are over. Reaping here would delete the bot's home volume with them, which is the
	// browser profile and the files that volume exists for (docs/adr/0003-virtual-computer.md
	// §3.5). A control plane that dies with sessions running is not a teardown.
	//
	// Removing them is therefore opt-in: VCOMPUTER_REAP_ON_SHUTDOWN=1, for a caller that is
	// ending the stack rather than restarting it (scripts/e2e.sh's teardown reaches for
	// scripts/cleanup.sh instead, which works whether or not this process got a SIGTERM at
	// all). Without it the log line names the two ways to remove what is left, so an operator
	// does not have to guess where a sandbox went — or why it is still there.
	//
	// The grace period is separate from the shutdown one and much longer, because it bounds a
	// different thing: stopping a sandbox is a container going down, not an HTTP request
	// finishing. A failure here is reported rather than swallowed, but it does not turn a
	// clean shutdown into a failed one — the server has already stopped, and the operator
	// needs the count, not an exit code.
	if reapOnShutdown() {
		reapCtx, stopReap := context.WithTimeout(context.Background(), *reapGrace)
		defer stopReap()
		reaped, err := runtime.Reap(reapCtx)
		if err != nil {
			logger.Printf("reaping the sandboxes failed: %v", err)
		} else {
			logger.Printf("reaped %d sandbox(es) and %d volume(s)", reaped.RemovedContainers, reaped.RemovedVolumes)
		}
		for _, failure := range reaped.Failed {
			logger.Printf("could not reap: %s", failure)
		}
	} else {
		logger.Print("keeping the sandboxes this process created, with their volumes: sessions outlive a restart. " +
			"To remove them, run scripts/cleanup.sh --running --volumes with STACK set, or start this process with VCOMPUTER_REAP_ON_SHUTDOWN=1 before a teardown")
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

// reapOnShutdown reports whether this process was asked to remove the sandboxes it created on
// the way out. It is opt-in and off by default (#263): the value of a session is its home
// volume, and a SIGTERM is what an ordinary restart looks like, so the destructive answer has
// to be the one somebody asked for rather than the one they get by doing nothing.
//
// Only "1" and "true" count. Anything else — unset, empty, "0", "no" — means keep, so a
// deployment that sets the variable to anything other than an explicit yes cannot lose data by
// typo, and a compose file can pass it through without deciding the question.
func reapOnShutdown() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VCOMPUTER_REAP_ON_SHUTDOWN"))) {
	case "1", "true":
		return true
	}
	return false
}
