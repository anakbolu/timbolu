// Package service implements the health module use cases. It depends only on
// the module's domain contracts.
package service

import (
	"context"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/health/domain"
)

type service struct {
	database domain.Repository
	redis    domain.Checker
}

// New builds the health service. Both dependencies are optional: a nil
// dependency is reported as not_configured instead of failing the probe, which
// keeps the API usable while Postgres or Redis is down.
func New(database domain.Repository, redis domain.Checker) domain.Service {
	return &service{database: database, redis: redis}
}

// Liveness reports the process state without touching any dependency.
func (s *service) Liveness() domain.Report {
	return domain.Report{Status: domain.StatusOK}
}

// Readiness checks every configured dependency and reports per-component state.
func (s *service) Readiness(ctx context.Context) domain.Report {
	report := domain.Report{Status: domain.StatusOK}

	report.Components = append(report.Components, check("database", s.database, ctx))
	if s.redis == nil {
		report.Components = append(report.Components, domain.Component{
			Name:   "redis",
			Status: domain.StatusNotConfigured,
		})
	} else {
		report.Components = append(report.Components, check("redis", s.redis, ctx))
	}

	for _, component := range report.Components {
		if component.Status == domain.StatusUnavailable {
			report.Status = domain.StatusUnavailable
			break
		}
	}

	return report
}

func check(name string, checker domain.Checker, ctx context.Context) domain.Component {
	if checker == nil {
		return domain.Component{Name: name, Status: domain.StatusNotConfigured}
	}
	if err := checker.Ping(ctx); err != nil {
		return domain.Component{Name: name, Status: domain.StatusUnavailable, Error: err.Error()}
	}
	return domain.Component{Name: name, Status: domain.StatusOK}
}
