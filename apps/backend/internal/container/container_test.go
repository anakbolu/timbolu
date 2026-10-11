package container

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
)

// testConfig is the smallest configuration that assembles a container: no
// database, no Redis, no Temporal, console-free logging.
func testConfig() *config.Config {
	return &config.Config{
		Application: config.AppConfig{Environment: "test"},
		Http:        config.HttpConfig{ApiPrefix: "/api"},
		Logging:     config.LoggingConfig{Level: "error", Format: "json"},
	}
}

// newTestContainer assembles the container with every external dependency
// injected as nil, so the test never touches Postgres, Redis or Temporal.
func newTestContainer(t *testing.T, opts ...Option) *Container {
	t.Helper()

	all := append([]Option{
		WithLogger(zap.NewNop()),
		WithDB(nil),
		WithRedis(nil),
		WithTemporal(nil),
	}, opts...)

	c, err := New(context.Background(), testConfig(), all...)
	if err != nil {
		t.Fatalf("assemble container: %v", err)
	}
	t.Cleanup(c.Close)

	return c
}

func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(context.Background(), nil); err == nil {
		t.Fatal("expected an error for a nil config")
	}
}

// TestContainerAssemblesEveryDependency is the container test the architecture
// document asks for: every constructor runs, and the routes it registered
// answer.
func TestContainerAssemblesEveryDependency(t *testing.T) {
	c := newTestContainer(t)

	if c.Logger == nil {
		t.Fatal("logger must be injected")
	}
	if c.Repositories == nil || c.Services == nil || c.Handlers == nil {
		t.Fatal("every layer must be assembled")
	}
	if c.App() == nil {
		t.Fatal("the HTTP app must be built")
	}

	resp, err := c.App().Test(httptest.NewRequest(fiber.MethodGet, "/health", nil))
	if err != nil {
		t.Fatalf("call /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected /health to answer 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("the request context middleware must be installed")
	}
}

// TestReadyWithoutDependencies keeps the documented behaviour: the API starts
// and serves traffic while Postgres or Redis is unavailable.
func TestReadyWithoutDependencies(t *testing.T) {
	c := newTestContainer(t)

	resp, err := c.App().Test(httptest.NewRequest(fiber.MethodGet, "/ready", nil))
	if err != nil {
		t.Fatalf("call /ready: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200 while nothing is configured, got %d", resp.StatusCode)
	}

	var body struct {
		Data struct {
			Status     string `json:"status"`
			Components []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"components"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /ready: %v", err)
	}
	if body.Data.Status != "ok" {
		t.Fatalf("expected an ok report, got %+v", body.Data)
	}
	if len(body.Data.Components) != 2 {
		t.Fatalf("expected database and redis to be reported, got %+v", body.Data.Components)
	}
	for _, component := range body.Data.Components {
		if component.Status != "not_configured" {
			t.Fatalf("component %s: expected not_configured, got %s", component.Name, component.Status)
		}
	}
}

func TestWithoutHTTPSkipsTheApp(t *testing.T) {
	c := newTestContainer(t, WithoutHTTP())

	if c.App() != nil {
		t.Fatal("WithoutHTTP must skip building the Fiber app")
	}
}

// TestErrorHandlerUsesTheEnvelope proves an unknown route answers with the JSON
// envelope instead of Fiber's plain-text default.
func TestErrorHandlerUsesTheEnvelope(t *testing.T) {
	c := newTestContainer(t)

	resp, err := c.App().Test(httptest.NewRequest(fiber.MethodGet, "/does-not-exist", nil))
	if err != nil {
		t.Fatalf("call unknown route: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	var body struct {
		Success bool `json:"success"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("expected a JSON envelope, got %v", err)
	}
	if body.Success || body.Error.Code != "http_error" {
		t.Fatalf("unexpected envelope %+v", body)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := newTestContainer(t)

	c.Close()
	c.Close()

	if c.DB != nil || c.Redis != nil || c.Temporal != nil {
		t.Fatal("Close must release every dependency it owns")
	}
}

// TestDatabaseURLFailureIsReported keeps a broken DATABASE_URL a startup error
// instead of a mystery on the first query.
func TestDatabaseURLFailureIsReported(t *testing.T) {
	cfg := testConfig()
	cfg.Database.URL = "postgres://user:pass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"

	c, err := New(context.Background(), cfg, WithLogger(zap.NewNop()))
	if err == nil {
		c.Close()
		t.Fatal("expected an error for an unreachable database")
	}
	if !strings.Contains(err.Error(), "open database") {
		t.Fatalf("expected an open database error, got %v", err)
	}
}
