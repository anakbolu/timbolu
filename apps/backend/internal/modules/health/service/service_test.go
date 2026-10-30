package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/health/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/health/domain/mocks"
)

func TestLivenessNeverTouchesDependencies(t *testing.T) {
	// A failing database must not make the liveness probe fail: otherwise a
	// Postgres outage would restart every container.
	database := mocks.NewRepository(t)

	report := New(database, nil).Liveness()

	if report.Status != domain.StatusOK {
		t.Fatalf("expected %s, got %s", domain.StatusOK, report.Status)
	}
	if len(report.Components) != 0 {
		t.Fatalf("liveness must not check components, got %v", report.Components)
	}
	database.AssertNotCalled(t, "Ping")
}

func TestReadiness(t *testing.T) {
	dbErr := errors.New("connection refused")
	redisErr := errors.New("dial tcp: connection refused")

	tests := []struct {
		name       string
		database   func(t *testing.T) domain.Repository
		redis      func(t *testing.T) domain.Checker
		wantStatus string
		want       map[string]string
	}{
		{
			name: "every dependency healthy",
			database: func(t *testing.T) domain.Repository {
				t.Helper()

				m := mocks.NewRepository(t)
				m.EXPECT().Ping(mock.Anything).Return(nil).Once()
				return m
			},
			redis: func(t *testing.T) domain.Checker {
				t.Helper()

				m := mocks.NewChecker(t)
				m.EXPECT().Ping(mock.Anything).Return(nil).Once()
				return m
			},
			wantStatus: domain.StatusOK,
			want:       map[string]string{"database": domain.StatusOK, "redis": domain.StatusOK},
		},
		{
			name: "missing redis is reported as not configured, not as a failure",
			database: func(t *testing.T) domain.Repository {
				t.Helper()

				m := mocks.NewRepository(t)
				m.EXPECT().Ping(mock.Anything).Return(nil).Once()
				return m
			},
			redis: func(t *testing.T) domain.Checker {
				t.Helper()
				return nil
			},
			wantStatus: domain.StatusOK,
			want:       map[string]string{"database": domain.StatusOK, "redis": domain.StatusNotConfigured},
		},
		{
			name: "database failure makes the process unready",
			database: func(t *testing.T) domain.Repository {
				t.Helper()

				m := mocks.NewRepository(t)
				m.EXPECT().Ping(mock.Anything).Return(dbErr).Once()
				return m
			},
			redis: func(t *testing.T) domain.Checker {
				t.Helper()

				m := mocks.NewChecker(t)
				m.EXPECT().Ping(mock.Anything).Return(nil).Once()
				return m
			},
			wantStatus: domain.StatusUnavailable,
			want:       map[string]string{"database": domain.StatusUnavailable, "redis": domain.StatusOK},
		},
		{
			name: "redis failure makes the process unready",
			database: func(t *testing.T) domain.Repository {
				t.Helper()

				m := mocks.NewRepository(t)
				m.EXPECT().Ping(mock.Anything).Return(nil).Once()
				return m
			},
			redis: func(t *testing.T) domain.Checker {
				t.Helper()

				m := mocks.NewChecker(t)
				m.EXPECT().Ping(mock.Anything).Return(redisErr).Once()
				return m
			},
			wantStatus: domain.StatusUnavailable,
			want:       map[string]string{"database": domain.StatusOK, "redis": domain.StatusUnavailable},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := New(tt.database(t), tt.redis(t)).Readiness(context.Background())

			if report.Status != tt.wantStatus {
				t.Fatalf("expected status %s, got %s", tt.wantStatus, report.Status)
			}
			if len(report.Components) != len(tt.want) {
				t.Fatalf("expected %d components, got %v", len(tt.want), report.Components)
			}
			for _, component := range report.Components {
				if tt.want[component.Name] != component.Status {
					t.Fatalf("component %s: expected %s, got %s", component.Name, tt.want[component.Name], component.Status)
				}
				if component.Status == domain.StatusUnavailable && component.Error == "" {
					t.Fatalf("component %s must explain the failure", component.Name)
				}
			}
		})
	}
}

// TestReadinessWithoutAnyDependency documents the "API boots without Postgres"
// behaviour: nothing is configured, so nothing fails the probe.
func TestReadinessWithoutAnyDependency(t *testing.T) {
	report := New(nil, nil).Readiness(context.Background())

	if report.Status != domain.StatusOK {
		t.Fatalf("expected %s, got %s", domain.StatusOK, report.Status)
	}
	for _, component := range report.Components {
		if component.Status != domain.StatusNotConfigured {
			t.Fatalf("component %s: expected %s, got %s", component.Name, domain.StatusNotConfigured, component.Status)
		}
	}
}
