package integration

import (
	"os"
	"sync"
	"testing"
)

// The package's containers outlive every test in it, so they are stopped when the
// process ends rather than by one test's cleanup. Without this a container started
// for the first test would be terminated while later tests are still using it.
var (
	shutdownMu       sync.Mutex
	shutdownHandlers []func()
)

// registerShutdown records a container to stop when the package finishes.
func registerShutdown(handler func()) {
	shutdownMu.Lock()
	defer shutdownMu.Unlock()
	shutdownHandlers = append(shutdownHandlers, handler)
}

// TestMain stops what the package started.
func TestMain(m *testing.M) {
	code := m.Run()

	shutdownMu.Lock()
	handlers := append([]func(){}, shutdownHandlers...)
	shutdownMu.Unlock()

	for _, handler := range handlers {
		handler()
	}
	os.Exit(code)
}
