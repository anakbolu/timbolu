package handler

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/health/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/health/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

func newTestApp(t *testing.T, service domain.Service) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	Routes(app, New(service, zap.NewNop()), "/api")
	return app
}

func TestLive(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().Liveness().Return(domain.Report{Status: domain.StatusOK}).Twice()

	app := newTestApp(t, service)

	for _, path := range []string{"/health", "/api/health"} {
		req := httptest.NewRequest(fiber.MethodGet, path, nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, resp.StatusCode)
		}

		// Decoded rather than string-matched: the point is that the caller can
		// read the build identity, which is how a deploy is verified from
		// outside.
		var body struct {
			Success bool `json:"success"`
			Data    struct {
				Status  string  `json:"status"`
				Commit  *string `json:"commit"`
				BuiltAt *string `json:"built_at"`
				Build   *string `json:"build"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s: decode body: %v", path, err)
		}
		if !body.Success || body.Data.Status != domain.StatusOK {
			t.Fatalf("%s: expected a live envelope, got %+v", path, body)
		}
		for name, value := range map[string]*string{
			"commit":   body.Data.Commit,
			"built_at": body.Data.BuiltAt,
			"build":    body.Data.Build,
		} {
			if value == nil || *value == "" {
				t.Fatalf("%s: expected data.%s in the payload, got %+v", path, name, body.Data)
			}
		}
	}
}

func TestReady(t *testing.T) {
	tests := []struct {
		name       string
		report     domain.Report
		wantStatus int
		wantCode   string
	}{
		{
			name:       "ready",
			report:     domain.Report{Status: domain.StatusOK},
			wantStatus: fiber.StatusOK,
		},
		{
			name: "not ready",
			report: domain.Report{
				Status: domain.StatusUnavailable,
				Components: []domain.Component{
					{Name: "database", Status: domain.StatusUnavailable, Error: "connection refused"},
				},
			},
			wantStatus: fiber.StatusServiceUnavailable,
			wantCode:   "not_ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := mocks.NewService(t)
			service.EXPECT().Readiness(mock.Anything).Return(tt.report).Once()

			app := newTestApp(t, service)

			resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ready", nil))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("expected %d, got %d", tt.wantStatus, resp.StatusCode)
			}

			var body response.APIResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if tt.wantCode == "" {
				if !body.Success {
					t.Fatalf("expected success envelope, got %+v", body)
				}
				return
			}
			if body.Error == nil || body.Error.Code != tt.wantCode {
				t.Fatalf("expected error code %s, got %+v", tt.wantCode, body.Error)
			}
		})
	}
}

// TestReadyReportsDependencyDetail keeps the per-component detail in the
// response: an operator must see which dependency is down without reading logs.
func TestReadyReportsDependencyDetail(t *testing.T) {
	service := mocks.NewService(t)
	service.EXPECT().Readiness(mock.Anything).Return(domain.Report{
		Status: domain.StatusUnavailable,
		Components: []domain.Component{
			{Name: "redis", Status: domain.StatusUnavailable, Error: errors.New("dial tcp: refused").Error()},
		},
	}).Once()

	app := newTestApp(t, service)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ready", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body struct {
		Error struct {
			Details domain.Report `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Error.Details.Components) != 1 || body.Error.Details.Components[0].Name != "redis" {
		t.Fatalf("expected the failing component in the payload, got %+v", body.Error.Details)
	}
}
