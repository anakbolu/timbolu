package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	whatsappservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/service"
	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain/mocks"
)

// The internal listener is the second half of the seam: the API serves the
// gateway on an address the public never reaches. What is under test here is the
// wiring — that a deployment without an address serves nothing, that a
// half-configured one refuses to serve rather than serving unauthenticated, and
// that a complete one answers the gateway's requests and nobody else's.
//
// The handler's own behaviour (the status table, the workspace scope, the
// sentinel mapping) is covered by the seam's package tests; this is about the
// process holding the socket.

// internalApp builds a container whose internal listener is configured the way
// the deployment would.
func internalApp(t *testing.T, address, token string, service whatsappdomain.Service) *Container {
	t.Helper()

	cfg := testConfig()
	cfg.Http.InternalAddress = address
	cfg.WhatsApp.GatewayToken = token

	c := &Container{
		Config: cfg,
		Logger: zap.NewNop(),
	}
	if service != nil {
		c.Services = &Services{WhatsApp: service}
	}

	return c
}

// freeAddress reserves a port and releases it, so a test can name the address the
// listener will serve on and then reach it.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	return address
}

// waitForListener blocks until something accepts a connection at the address, so
// the test does not race the goroutine that starts serving.
func waitForListener(t *testing.T, address string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("nothing is listening at %s", address)
}

// TestTheInternalListenerIsOffUnlessAnAddressIsNamed: a deployment without a
// gateway is unchanged, and RunInternalServer returns instead of blocking.
func TestTheInternalListenerIsOffUnlessAnAddressIsNamed(t *testing.T) {
	internalApp(t, "", "token", nil).RunInternalServer(context.Background())

	var nilContainer *Container
	nilContainer.RunInternalServer(context.Background())
}

// TestTheInternalListenerRefusesWithoutAService: an address with no module would
// be a port that answers nothing, so the listener does not start.
func TestTheInternalListenerRefusesWithoutAService(t *testing.T) {
	done := make(chan struct{})
	go func() {
		internalApp(t, freeAddress(t), "token", nil).RunInternalServer(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the listener started without a service")
	}
}

// TestTheInternalListenerRefusesWithoutAToken: the surface speaks for every
// tenant, so a listener without the shared secret would let anybody who can reach
// it act in any workspace. Refusing to start is the safe reading.
func TestTheInternalListenerRefusesWithoutAToken(t *testing.T) {
	service := whatsappservice.New(whatsappservice.Deps{Repository: mocks.NewStore(t)})

	done := make(chan struct{})
	go func() {
		internalApp(t, freeAddress(t), "", service).RunInternalServer(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the listener started without a gateway token")
	}
}

// TestTheInternalListenerServesTheGatewayAndRefusesEveryoneElse is the end-to-end
// wiring: a real socket, a real request, and the product reached through it.
func TestTheInternalListenerServesTheGatewayAndRefusesEveryoneElse(t *testing.T) {
	store := mocks.NewStore(t)
	service := whatsappservice.New(whatsappservice.Deps{Repository: store})

	workspaceID := uuid.New()
	numberID := uuid.New()
	store.EXPECT().
		Get(mock.Anything, mock.Anything, numberID).
		Return(whatsappdomain.Number{
			ID:          numberID,
			WorkspaceID: workspaceID,
			Phone:       "628123456789",
			Status:      whatsappdomain.StatusConnected,
		}, nil).
		Once()

	address := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go internalApp(t, address, "secret", service).RunInternalServer(ctx)
	waitForListener(t, address)

	base := "http://" + address
	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("no token is unauthorized", func(t *testing.T) {
		response := do(t, client, http.MethodGet, base+"/internal/whatsapp/numbers/"+numberID.String(), "", "")
		defer response.Body.Close()
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	})

	t.Run("a wrong token is unauthorized", func(t *testing.T) {
		response := do(t, client, http.MethodGet, base+"/internal/whatsapp/numbers/"+numberID.String(), "wrong", workspaceID.String())
		defer response.Body.Close()
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	})

	t.Run("a missing workspace is refused", func(t *testing.T) {
		response := do(t, client, http.MethodGet, base+"/internal/whatsapp/numbers/"+numberID.String(), "secret", "")
		defer response.Body.Close()
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
	})

	t.Run("the gateway reads the number", func(t *testing.T) {
		response := do(t, client, http.MethodGet, base+"/internal/whatsapp/numbers/"+numberID.String(), "secret", workspaceID.String())
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)

		var body map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		require.Equal(t, numberID.String(), body["id"])
		require.Equal(t, "628123456789", body["phone"])
		require.Equal(t, whatsappdomain.StatusConnected, body["status"])
	})

	// The address is not a route on the public API: the seam's paths are served
	// only by this listener, so a request to a path it does not own is a 404.
	t.Run("an unknown path is not served", func(t *testing.T) {
		response := do(t, client, http.MethodGet, base+"/api/whatsapp/numbers", "secret", workspaceID.String())
		defer response.Body.Close()
		require.Equal(t, http.StatusNotFound, response.StatusCode)
	})
}

// do issues one request with the seam's own headers.
func do(t *testing.T, client *http.Client, method, url, token, workspace string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	require.NoError(t, err)
	if token != "" {
		request.Header.Set("X-Wa-Gateway-Token", token)
	}
	if workspace != "" {
		request.Header.Set("X-Workspace-Id", workspace)
	}

	response, err := client.Do(request)
	require.NoError(t, err)

	return response
}

// TestTheInternalListenerReportsAMissingNumber proves the sentinel mapping
// crosses the process boundary: the service's ErrNotFound reaches the gateway as
// a 404 rather than a 500, which is what lets the engine tell "no such number"
// from "the product is broken".
func TestTheInternalListenerReportsAMissingNumber(t *testing.T) {
	store := mocks.NewStore(t)
	service := whatsappservice.New(whatsappservice.Deps{Repository: store})

	workspaceID := uuid.New()
	numberID := uuid.New()
	store.EXPECT().
		Get(mock.Anything, mock.Anything, numberID).
		Return(whatsappdomain.Number{}, fmt.Errorf("%w: no number", whatsappdomain.ErrNotFound)).
		Once()

	address := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go internalApp(t, address, "secret", service).RunInternalServer(ctx)
	waitForListener(t, address)

	response := do(t, &http.Client{Timeout: 5 * time.Second}, http.MethodGet,
		"http://"+address+"/internal/whatsapp/numbers/"+numberID.String(), "secret", workspaceID.String())
	defer response.Body.Close()

	require.Equal(t, http.StatusNotFound, response.StatusCode)
}
