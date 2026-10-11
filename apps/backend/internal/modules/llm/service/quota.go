package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// Check rejects a call when the workspace has spent its allowance. It is the
// pre-flight guard: the provider is not called, so nothing is billed.
//
// Two bounds are asked, because the product sells two. The period allowance
// stops a workspace from spending a month's tokens; the daily cost ceiling stops
// one runaway task from spending a month in an afternoon. A zero on either means
// unlimited, and QuotaEnabled=false skips both for local development.
func (s *Service) Check(ctx context.Context, workspaceID uuid.UUID) error {
	if s == nil || !s.settings.QuotaEnabled {
		return nil
	}
	if workspaceID == uuid.Nil {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidRequest)
	}
	if s.deps.Quota == nil {
		// Running with the check enabled but no policy would silently allow
		// everything, so it is an error instead.
		return fmt.Errorf("llm: quota is enabled but no policy is configured")
	}

	if err := s.checkPeriodAllowance(ctx, workspaceID); err != nil {
		return err
	}
	return s.checkDailyCost(ctx, workspaceID)
}

// checkPeriodAllowance enforces the token allowance of the billing period.
func (s *Service) checkPeriodAllowance(ctx context.Context, workspaceID uuid.UUID) error {
	allowance, err := s.deps.Quota.Allowance(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("llm: read quota allowance: %w", err)
	}
	if allowance <= 0 {
		return nil
	}

	spent, err := s.spent(ctx, workspaceID)
	if err != nil {
		return err
	}
	if spent >= allowance {
		return fmt.Errorf("%w: %d dari %d token terpakai pada periode ini", domain.ErrQuotaExceeded, spent, allowance)
	}

	return nil
}

// checkDailyCost enforces the day's cost ceiling.
//
// The ceiling is checked here as well as at the task boundary on purpose: a task
// stops at a clean round boundary, and this is what stops the calls inside a
// round once the day is spent. Without it a workspace could cross its daily
// ceiling by one round's worth of calls, every time.
func (s *Service) checkDailyCost(ctx context.Context, workspaceID uuid.UUID) error {
	allowance, err := s.deps.Quota.DailyCostAllowance(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("llm: read daily cost allowance: %w", err)
	}
	if allowance <= 0 {
		return nil
	}

	spent, err := s.spentToday(ctx, workspaceID)
	if err != nil {
		return err
	}
	if spent >= allowance {
		return fmt.Errorf("%w: %d dari %d micro-rupiah terpakai hari ini", domain.ErrQuotaExceeded, spent, allowance)
	}

	return nil
}

// spentToday reads the cost spent today.
//
// The counter in Redis is the fast path, and the daily aggregation is the
// durable fallback, for the same reason the period check has one: an outage must
// not hand every workspace a fresh ceiling.
func (s *Service) spentToday(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if s.deps.Counter != nil {
		total, err := s.deps.Counter.SpentCostToday(ctx, workspaceID)
		if err == nil {
			return total, nil
		}
		if s.deps.Reader == nil {
			return 0, fmt.Errorf("llm: read today's cost: %w", err)
		}
	}

	if s.deps.Reader == nil {
		return 0, fmt.Errorf("llm: no usage reader is configured")
	}

	total, err := s.deps.Reader.CostSince(ctx, workspaceID, DayStart(s.now()))
	if err != nil {
		return 0, fmt.Errorf("llm: sum the day's cost: %w", err)
	}
	return total, nil
}

// DayStart is the first moment of the current day in Jakarta, which is where the
// customers are and therefore where their day boundary falls.
func DayStart(now time.Time) time.Time {
	jakarta := now.In(Jakarta)
	return time.Date(jakarta.Year(), jakarta.Month(), jakarta.Day(), 0, 0, 0, 0, Jakarta)
}

// spent reads the live token total.
//
// The counter in Redis is the fast path. When it is unavailable the ledger
// answers instead: an outage must not hand every workspace a fresh allowance.
func (s *Service) spent(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if s.deps.Counter != nil {
		total, err := s.deps.Counter.Spent(ctx, workspaceID)
		if err == nil {
			return total, nil
		}
		if s.deps.Reader == nil {
			return 0, fmt.Errorf("llm: read live spend: %w", err)
		}
	}

	if s.deps.Reader == nil {
		return 0, fmt.Errorf("llm: no usage reader is configured")
	}

	total, err := s.deps.Reader.SumSince(ctx, workspaceID, PeriodStart(s.now()))
	if err != nil {
		return 0, fmt.Errorf("llm: sum ledger usage: %w", err)
	}
	return total, nil
}

// PeriodStart is the first moment of the current quota period: the start of the
// month in Jakarta. It is exported so the counter and the check cannot disagree
// about which period a token belongs to.
func PeriodStart(now time.Time) time.Time {
	jakarta := now.In(Jakarta)
	return time.Date(jakarta.Year(), jakarta.Month(), 1, 0, 0, 0, 0, Jakarta)
}
