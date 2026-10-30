package service

import (
	"github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The two halves of one runtime are mutually dependent, and this is where that is
// resolved.
//
// The service sends *through* the transport, and the engine that is the transport
// in the gateway process records what arrives *through* the service. Whichever is
// built first needs the other, so the service is built first — it needs nothing
// from the engine to exist — and the engine's owner installs itself here once it
// exists. Everything between those two moments is a process starting up, before
// any request is served, so there is no window in which a caller sees a
// half-wired runtime.

// UseTransport installs the transport a service sends through.
//
// It is for the gateway process, which is the one that builds the engine. A
// process that reaches the gateway over HTTP passes its transport to `New` and
// never calls this.
func UseTransport(target domain.Service, transport domain.Transport) {
	if concrete, ok := target.(*Service); ok {
		concrete.setTransport(transport)
	}
}

// setTransport installs the transport under the lock the service reads it with.
func (s *Service) setTransport(transport domain.Transport) {
	s.transportMu.Lock()
	defer s.transportMu.Unlock()

	s.deps.Transport = transport
}

// transport is the transport a call runs against, read under the lock.
//
// Every path in this package reads it here rather than from `deps` directly, which
// is the one thing that makes the late install work: the engine the gateway builds
// is reached through this, and a call site that read the field would keep seeing the
// nil it was constructed with.
func (s *Service) transport() domain.Transport {
	s.transportMu.RLock()
	defer s.transportMu.RUnlock()

	return s.deps.Transport
}
