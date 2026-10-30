package service_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/order/service"
)

// This file is the module's own rules rather than Postgres's: what a name resolves to, what makes an
// order refusable before the first insert, and what a call without a tenant is told. The repository is
// a mock precisely because none of that is storage's job — a test that needed a database here would
// be testing the wrong layer.

// harness is the service over a scripted repository.
type harness struct {
	repo  *mocks.Repository
	svc   *service.Service
	scope domain.Scope
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessWithLogger(t, zap.NewNop())
}

func newHarnessWithLogger(t *testing.T, logger *zap.Logger) *harness {
	t.Helper()

	repo := mocks.NewRepository(t)

	return &harness{
		repo:  repo,
		svc:   service.New(service.Deps{Repository: repo, Logger: logger}),
		scope: domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// TestACallWithoutAWorkspaceIsRefused: the workspace is the one thing every method needs, so a call
// that names none must not reach storage — a read or write against a scope-less query would either
// see every tenant's rows or none, and both are worse than a refusal.
func TestACallWithoutAWorkspaceIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Products(t.Context(), domain.Scope{}, domain.ProductFilter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Record(t.Context(), domain.Scope{}, domain.NewOrder{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestALookupWithoutATermIsRefused: an empty term is not a search, and matching it against the
// catalogue would hand back the whole catalogue as "candidates" — advice a model would read as an
// answer.
func TestALookupWithoutATermIsRefused(t *testing.T) {
	h := newHarness(t)

	for _, term := range []string{"", "   "} {
		_, err := h.svc.Lookup(t.Context(), h.scope, term)
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	}
}

// TestLookupFindsAnExactNameCaseInsensitively: a customer types "Kopi arabika" and the catalogue says
// "Kopi Arabika" — the same product to everybody except a string. The candidates start at the match,
// so the found product is not repeated as a suggestion.
func TestLookupFindsAnExactNameCaseInsensitively(t *testing.T) {
	h := newHarness(t)

	products := []domain.Product{
		{ID: uuid.New(), Name: "Kopi Susu"},
		{ID: uuid.New(), Name: "Kopi Arabika", SKU: "KOPI-ARB"},
		{ID: uuid.New(), Name: "Kopi Tubruk"},
	}
	h.repo.EXPECT().MatchProducts(mock.Anything, h.scope, "kopi arabika", 5).Return(products, nil)

	lookup, err := h.svc.Lookup(t.Context(), h.scope, "  kopi arabika  ")
	require.NoError(t, err)
	require.NotNil(t, lookup.Found)
	require.Equal(t, products[1], *lookup.Found)
	require.Equal(t, products[1:], lookup.Candidates)
}

// TestLookupFindsBySKU: the catalogue's own handle is how a repeat order names a product, so a sku
// that folds to the same letters is the same product too.
func TestLookupFindsBySKU(t *testing.T) {
	h := newHarness(t)

	products := []domain.Product{
		{ID: uuid.New(), Name: "Air Mineral", SKU: "AQUA-600"},
		{ID: uuid.New(), Name: "Air Galon", SKU: "AQUA-19"},
	}
	h.repo.EXPECT().MatchProducts(mock.Anything, h.scope, "aqua-600", 5).Return(products, nil)

	lookup, err := h.svc.Lookup(t.Context(), h.scope, "aqua-600")
	require.NoError(t, err)
	require.NotNil(t, lookup.Found)
	require.Equal(t, products[0], *lookup.Found)
}

// TestAFuzzyLookupOffersCandidatesWithoutAFoundProduct: a partial term is a question, not an answer,
// so `Found` stays nil and every fuzzy match travels back as a candidate for the model to ask about.
func TestAFuzzyLookupOffersCandidatesWithoutAFoundProduct(t *testing.T) {
	h := newHarness(t)

	products := []domain.Product{
		{ID: uuid.New(), Name: "Kopi Arabika"},
		{ID: uuid.New(), Name: "Kopi Susu"},
	}
	h.repo.EXPECT().MatchProducts(mock.Anything, h.scope, "kopi", 5).Return(products, nil)

	lookup, err := h.svc.Lookup(t.Context(), h.scope, "kopi")
	require.NoError(t, err)
	require.Nil(t, lookup.Found)
	require.Equal(t, products, lookup.Candidates)
}

// TestRecordRefusesAnOrderWithoutLines: an order with no products is not an order, and the invoice
// that followed it would be for nothing.
func TestRecordRefusesAnOrderWithoutLines(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Record(t.Context(), h.scope, domain.NewOrder{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestRecordRefusesALineBeforeWritingAnything: a blank name and a quantity that is not positive are a
// customer's typo or a model's slip, and neither belongs on an invoice — so the whole order is refused
// before the repository is told to write, which is why none of these set an expectation.
func TestRecordRefusesALineBeforeWritingAnything(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name         string
		item         domain.NewItem
		wantContains string
	}{
		{"blank product name", domain.NewItem{Product: "   ", Quantity: 1}, ""},
		{"zero quantity", domain.NewItem{Product: "Kopi Arabika", Quantity: 0}, "Kopi Arabika"},
		{"negative quantity", domain.NewItem{Product: "Kopi Arabika", Quantity: -2}, "Kopi Arabika"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.svc.Record(t.Context(), h.scope, domain.NewOrder{Items: []domain.NewItem{tt.item}})
			require.ErrorIs(t, err, domain.ErrInvalidInput)
			if tt.wantContains != "" {
				require.ErrorContains(t, err, tt.wantContains)
			}
		})
	}
}

// TestRecordTrimsWhatItWrites: the model wraps names in whitespace and a person types an extra space
// before their number; what reaches storage has to be the value, not the rendering of it.
func TestRecordTrimsWhatItWrites(t *testing.T) {
	h := newHarness(t)

	agentID, taskID := uuid.New(), uuid.New()
	recorded := domain.Order{ID: uuid.New(), Code: "ORD-1", Status: domain.StatusRecorded, TotalMicros: 30000000}
	h.repo.EXPECT().Record(mock.Anything, h.scope, domain.NewOrder{
		CustomerPhone: "08123456789",
		CustomerName:  "Budi",
		Note:          "antar pagi",
		AgentID:       agentID,
		TaskID:        taskID,
		Items: []domain.NewItem{
			{Product: "Kopi Arabika", Quantity: 2, Note: "panas"},
		},
	}).Return(recorded, nil)

	got, err := h.svc.Record(t.Context(), h.scope, domain.NewOrder{
		CustomerPhone: "  08123456789 ",
		CustomerName:  "  Budi  ",
		Note:          "antar pagi",
		AgentID:       agentID,
		TaskID:        taskID,
		Items: []domain.NewItem{
			{Product: "  Kopi Arabika ", Quantity: 2, Note: "panas"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, recorded, got)
}

// TestAnOrderWithoutAPhoneIsRecordedAnyway: a phone is how a customer is keyed, so an order without
// one is recorded against an empty customer rather than refused — losing the order is worse than
// losing the link — and it is worth a warning because the operator may be able to fill the gap.
func TestAnOrderWithoutAPhoneIsRecordedAnyway(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	h := newHarnessWithLogger(t, zap.New(core))

	h.repo.EXPECT().Record(mock.Anything, h.scope, mock.MatchedBy(func(order domain.NewOrder) bool {
		return order.CustomerPhone == "" && order.CustomerName == "Budi"
	})).Return(domain.Order{Code: "ORD-2"}, nil)

	_, err := h.svc.Record(t.Context(), h.scope, domain.NewOrder{
		CustomerPhone: "   ",
		CustomerName:  " Budi ",
		Items:         []domain.NewItem{{Product: "Kopi Arabika", Quantity: 1}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, logs.Len())
	require.Contains(t, logs.All()[0].Message, "no phone number")
}

// TestDeletingSomethingAlreadyGoneIsNotFound: a screen that showed "deleted" for a product somebody
// else removed a minute earlier would be telling the person something untrue about their workspace.
func TestDeletingSomethingAlreadyGoneIsNotFound(t *testing.T) {
	h := newHarness(t)

	id := uuid.New()
	h.repo.EXPECT().DeleteProduct(mock.Anything, h.scope, id).Return(domain.ErrNotFound)

	err := h.svc.DeleteProduct(t.Context(), h.scope, id)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestAnIDTheCallerCannotNameIsRefused: without an id there is no product or order to act on, and a
// bare delete or cancel would be a write against the zero uuid.
func TestAnIDTheCallerCannotNameIsRefused(t *testing.T) {
	h := newHarness(t)

	require.ErrorIs(t, h.svc.DeleteProduct(t.Context(), h.scope, uuid.Nil), domain.ErrInvalidInput)

	_, err := h.svc.Order(t.Context(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Cancel(t.Context(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestCancelMarksTheOrderCanceled: cancelling is a status, not a delete — what a person decided not to
// fulfil stays readable, which is why the call goes through `SetOrderStatus`.
func TestCancelMarksTheOrderCanceled(t *testing.T) {
	h := newHarness(t)

	id := uuid.New()
	canceled := domain.Order{ID: id, Code: "ORD-3", Status: domain.StatusCanceled}
	h.repo.EXPECT().SetOrderStatus(mock.Anything, h.scope, id, domain.StatusCanceled).Return(canceled, nil)

	got, err := h.svc.Cancel(t.Context(), h.scope, id)
	require.NoError(t, err)
	require.Equal(t, canceled, got)
	require.Equal(t, domain.StatusCanceled, got.Status)
}

// TestTheListLimitsAreNormalised is the page size every list read shares: a caller that says nothing
// gets the default, a caller that asks for more than a page gets a page, and anything in between is
// honoured. The service leaves the filter alone and the repository applies these bounds, so the test
// is on the domain function both of them read.
func TestTheListLimitsAreNormalised(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"a zero limit takes the default", 0, domain.DefaultLimit},
		{"a negative limit takes the default", -5, domain.DefaultLimit},
		{"a limit above the ceiling is clamped", domain.MaxLimit + 50, domain.MaxLimit},
		{"a limit inside the bounds is kept", 10, 10},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, domain.ProductFilter{Limit: tt.in}.Normalised().Limit)
			require.Equal(t, tt.want, domain.OrderFilter{Limit: tt.in}.Normalised().Limit)
		})
	}
}
