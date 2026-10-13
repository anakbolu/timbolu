package repository

import (
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/repository/sqlcgen"
)

// The rows the generated code produces are turned into domain values here and
// nowhere else, so a column added to the schema is a compile error rather than a
// field a screen silently loses.

func toPlan(row sqlcgen.Plan) domain.Plan {
	return domain.Plan{
		ID:              row.ID,
		Key:             row.Key,
		Name:            row.Name,
		MonthlyPriceIDR: row.MonthlyPriceIdr,
		TokenQuota:      row.TokenQuota,
		DailyCostMicros: row.DailyCostMicros,
		PeriodDays:      int(row.PeriodDays),
		MaxAgents:       int(row.MaxAgents),
		MaxIntegrations: int(row.MaxIntegrations),
		MaxMembers:      int(row.MaxMembers),
		IsTrial:         row.IsTrial,
		SortOrder:       int(row.SortOrder),
	}
}

func toSubscription(row sqlcgen.Subscription) domain.Subscription {
	subscription := domain.Subscription{
		ID:                 row.ID,
		WorkspaceID:        row.WorkspaceID,
		PlanID:             row.PlanID,
		Status:             row.Status,
		CurrentPeriodStart: row.CurrentPeriodStart,
		CurrentPeriodEnd:   row.CurrentPeriodEnd,
		GraceUntil:         row.GraceUntil,
		PastDueSince:       row.PastDueSince,
		PastDueNotifiedAt:  row.PastDueNotifiedAt,
		ReadOnlyNotifiedAt: row.ReadOnlyNotifiedAt,
		CanceledAt:         row.CanceledAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
	// The pending plan is nullable, and a nil stays a nil id rather than becoming
	// the zero uuid — which would name a package that is not there.
	if row.PendingPlanID != nil {
		subscription.PendingPlanID = *row.PendingPlanID
	}
	return subscription
}

func toPeriod(row sqlcgen.QuotaPeriod) domain.QuotaPeriod {
	return domain.QuotaPeriod{
		ID:               row.ID,
		WorkspaceID:      row.WorkspaceID,
		PeriodStart:      row.PeriodStart,
		PeriodEnd:        row.PeriodEnd,
		TokensGranted:    row.TokensGranted,
		TokensToppedUp:   row.TokensToppedUp,
		TokensUsed:       row.TokensUsed,
		Warning80SentAt:  row.Warning80SentAt,
		Warning100SentAt: row.Warning100SentAt,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

func toInvoice(row sqlcgen.BillingInvoice) domain.Invoice {
	invoice := domain.Invoice{
		ID:                row.ID,
		WorkspaceID:       row.WorkspaceID,
		Kind:              row.Kind,
		Provider:          row.Provider,
		AmountIDR:         row.AmountIdr,
		TokenAmount:       row.TokenAmount,
		Status:            row.Status,
		PeriodStart:       row.PeriodStart,
		PeriodEnd:         row.PeriodEnd,
		Instructions:      row.PaymentInstructions,
		Attempts:          int(row.Attempts),
		LastError:         row.LastError,
		ExpiresAt:         row.ExpiresAt,
		PaidAt:            row.PaidAt,
		CreatedAt:         row.CreatedAt,
		ProviderReference: row.ProviderReference,
	}
	// The subscription is nullable on the row: a top-up bought before the workspace
	// had a live subscription is not a state the product reaches, but the column
	// allows it, so a nil stays a nil id rather than becoming the zero uuid — which
	// would name a subscription that is not there.
	if row.SubscriptionID != nil {
		invoice.SubscriptionID = *row.SubscriptionID
	}
	return invoice
}

func toTopUp(row sqlcgen.TopupPackage) domain.TopUpPackage {
	return domain.TopUpPackage{
		ID:          row.ID,
		Key:         row.Key,
		Name:        row.Name,
		TokenAmount: row.TokenAmount,
		PriceIDR:    row.PriceIdr,
		SortOrder:   int(row.SortOrder),
		Active:      row.Active,
	}
}
