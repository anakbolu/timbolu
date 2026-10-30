package container

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
	"github.com/anakbolu/timbolu/packages/whatsapp/seam"
)

// The internal listener: the second half of the seam, served by the API for the
// gateway process and nobody else.
//
// The gateway owns the socket and the product owns the record, so the four
// things the engine does with the product — it records what arrived, pulls what
// is due, reports what left, and says how a number is doing — come back over an
// address the public never reaches. It is a listener of its own rather than a
// route on the API because it speaks for every tenant at once: it authenticates
// with one shared secret and reads the workspace from a header, so putting it
// behind the tenant middleware would ask it for an identity it does not have.
//
// It is off unless the deployment names an address, so a deployment without a
// gateway is unchanged.
func (c *Container) RunInternalServer(ctx context.Context) {
	if c == nil || c.Config == nil {
		return
	}

	address := c.Config.Http.InternalAddress
	if address == "" {
		return
	}

	// The services are assembled with the container, so a missing module is a
	// missing set rather than a missing field. Both read the same way: the
	// listener does not start.
	var service whatsappdomain.Service
	if c.Services != nil {
		service = c.Services.WhatsApp
	}
	if service == nil {
		c.Logger.Warn("the internal whatsapp listener is configured but the module is not; it will not start",
			zap.String("address", address))

		return
	}

	if c.Config.WhatsApp.GatewayToken == "" {
		// The surface speaks for every tenant, so a listener without the shared
		// secret would let anybody who can reach it open a task in any workspace.
		// Refusing to start is the safe reading of a half-configured deployment.
		c.Logger.Error("the internal whatsapp listener has no gateway token; it will not start",
			zap.String("address", address))

		return
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		// The API's job is to serve people; refusing to start because an internal
		// port was busy would turn a gateway problem into an outage. Said loudly
		// instead, which is what an operator can act on.
		c.Logger.Error("the internal whatsapp listener could not start; the API runs without it",
			zap.String("address", address), zap.Error(err))

		return
	}

	mux := http.NewServeMux()
	seam.NewProduct(seam.ProductDeps{
		Inbox:  service,
		Drain:  service,
		Status: service,
		Links:  service,
	}, seam.ProductConfig{Token: c.Config.WhatsApp.GatewayToken}, c.Logger).Routes(mux)

	server := &http.Server{
		Handler: mux,
		// A message body and a media record are the largest things that cross, so
		// the timeouts are the same order as a request rather than a probe.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	c.Logger.Info("serving the internal whatsapp endpoint", zap.String("address", listener.Addr().String()))

	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		c.Logger.Warn("the internal whatsapp endpoint stopped", zap.Error(err))
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		c.Logger.Warn("the internal whatsapp endpoint did not shut down cleanly", zap.Error(err))
	}
}
