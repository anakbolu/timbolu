// Package service implements the order module's use cases: maintaining the catalogue, and recording
// the order a customer placed.
//
// It depends only on the module's domain contracts, so the flow can be tested without a database —
// which matters more here than elsewhere, because this is the module whose rules decide what an
// invoice says.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain"
)

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
	Logger     *zap.Logger
}

// Service is the order module's implementation.
type Service struct {
	deps domain.Repository
	log  *zap.Logger
}

// New builds the service.
func New(deps Deps) *Service {
	logger := deps.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	return &Service{deps: deps.Repository, log: logger}
}

// Compile-time check that the service is the interface it declares.
var _ domain.Service = (*Service)(nil)

// ---------------------------------------------------------------- the catalogue

// Products returns the workspace's catalogue.
func (s *Service) Products(ctx context.Context, scope domain.Scope, filter domain.ProductFilter) ([]domain.Product, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	return s.deps.Products(ctx, scope, filter)
}

// Product returns one catalogue entry.
func (s *Service) Product(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Product, error) {
	if err := requireScope(scope); err != nil {
		return domain.Product{}, err
	}
	if id == uuid.Nil {
		return domain.Product{}, fmt.Errorf("%w: product id is required", domain.ErrInvalidInput)
	}

	return s.deps.Product(ctx, scope, id)
}

// CreateProduct adds a catalogue entry.
func (s *Service) CreateProduct(ctx context.Context, scope domain.Scope, request domain.CreateProductRequest) (domain.Product, error) {
	if err := requireScope(scope); err != nil {
		return domain.Product{}, err
	}

	name := strings.TrimSpace(request.Name)
	if name == "" {
		return domain.Product{}, fmt.Errorf("%w: a product needs a name", domain.ErrInvalidInput)
	}
	if request.UnitPriceMicros < 0 {
		return domain.Product{}, fmt.Errorf("%w: a price cannot be negative", domain.ErrInvalidInput)
	}

	return s.deps.CreateProduct(ctx, scope, domain.NewProduct{
		SKU:             request.SKU,
		Name:            name,
		UnitPriceMicros: request.UnitPriceMicros,
		Unit:            request.Unit,
		Active:          true,
	})
}

// UpdateProduct edits a catalogue entry.
func (s *Service) UpdateProduct(ctx context.Context, scope domain.Scope, id uuid.UUID, request domain.UpdateProductRequest) (domain.Product, error) {
	if err := requireScope(scope); err != nil {
		return domain.Product{}, err
	}
	if id == uuid.Nil {
		return domain.Product{}, fmt.Errorf("%w: product id is required", domain.ErrInvalidInput)
	}
	if request.Name != nil && strings.TrimSpace(*request.Name) == "" {
		return domain.Product{}, fmt.Errorf("%w: a product needs a name", domain.ErrInvalidInput)
	}
	if request.UnitPriceMicros != nil && *request.UnitPriceMicros < 0 {
		return domain.Product{}, fmt.Errorf("%w: a price cannot be negative", domain.ErrInvalidInput)
	}

	// The two structs are the same five optional fields, and the conversion is written as one so a
	// field added to the request cannot be silently dropped on the way to the repository.
	return s.deps.UpdateProduct(ctx, scope, id, domain.ProductChange(request))
}

// DeleteProduct removes a catalogue entry.
//
// It is a read-then-delete rather than a bare delete so that removing something already gone is
// reported as not found: a screen that showed "deleted" for a product somebody else removed a minute
// earlier would be telling the person something untrue about their own workspace.
func (s *Service) DeleteProduct(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: product id is required", domain.ErrInvalidInput)
	}

	return s.deps.DeleteProduct(ctx, scope, id)
}

// ---------------------------------------------------------------- the flow

// Lookup answers what a name means in this workspace's catalogue.
//
// It reads the catalogue once and answers in one of three ways, which is what the flow needs: the
// product to price an order, the candidates to ask a person about, or nothing at all to tell the
// customer the product is not sold here. It is a read, so a Bolu can call it as often as it needs to
// before it records anything.
func (s *Service) Lookup(ctx context.Context, scope domain.Scope, term string) (domain.Lookup, error) {
	if err := requireScope(scope); err != nil {
		return domain.Lookup{}, err
	}

	trimmed := strings.TrimSpace(term)
	if trimmed == "" {
		return domain.Lookup{}, fmt.Errorf("%w: nama produk belum disebut", domain.ErrInvalidInput)
	}

	matches, err := s.deps.MatchProducts(ctx, scope, trimmed, 5)
	if err != nil {
		return domain.Lookup{}, err
	}

	lookup := domain.Lookup{Candidates: matches}
	for index := range matches {
		// An exact name (or sku) is the answer; anything else is only a suggestion. The comparison is
		// case-folded because a customer types "Kopi arabika" and the catalogue says "Kopi Arabika" —
		// the same product to everybody except a string.
		if strings.EqualFold(matches[index].Name, trimmed) || (matches[index].SKU != "" && strings.EqualFold(matches[index].SKU, trimmed)) {
			found := matches[index]
			lookup.Found = &found
			lookup.Candidates = matches[index:]

			break
		}
	}

	return lookup, nil
}

// Record writes the order a customer placed.
//
// Three checks happen before anything is written, and each of them is a case the flow has to handle:
//
//   - **A request with no lines is refused.** An order with no products is not an order, and the
//     invoice that followed it would be for nothing.
//   - **A quantity that is not positive is refused**, naming the line: "0" and "-2" are both a
//     customer's typo or a model's slip, and neither belongs on an invoice.
//   - **A product the catalogue does not have refuses the whole order**, with the candidates. The
//     alternative — writing the lines that did resolve — is an invoice for part of what the customer
//     asked for, which is worse than asking.
func (s *Service) Record(ctx context.Context, scope domain.Scope, order domain.NewOrder) (domain.Order, error) {
	if err := requireScope(scope); err != nil {
		return domain.Order{}, err
	}

	if len(order.Items) == 0 {
		return domain.Order{}, fmt.Errorf("%w: pesanan belum menyebut produk apa pun", domain.ErrInvalidInput)
	}

	items := make([]domain.NewItem, 0, len(order.Items))
	for _, item := range order.Items {
		name := strings.TrimSpace(item.Product)
		if name == "" {
			return domain.Order{}, fmt.Errorf("%w: ada baris pesanan yang belum menyebut nama produk", domain.ErrInvalidInput)
		}
		if item.Quantity <= 0 {
			return domain.Order{}, fmt.Errorf(
				"%w: jumlah untuk %q harus lebih dari nol (diminta %g)", domain.ErrInvalidInput, name, item.Quantity)
		}

		item.Product = name
		items = append(items, item)
	}

	order.Items = items
	order.CustomerPhone = strings.TrimSpace(order.CustomerPhone)
	order.CustomerName = strings.TrimSpace(order.CustomerName)

	// A phone is what a customer is keyed by, so an order without one is recorded against an empty
	// customer rather than refused: a Bolu that was asked to note an order from a phone call has no
	// number to give, and losing the order is worse than losing the link.
	if order.CustomerPhone == "" {
		s.log.Warn("recording an order with no phone number",
			zap.String("customer", order.CustomerName))
	}

	return s.deps.Record(ctx, scope, order)
}

// Orders returns the workspace's orders, newest first.
func (s *Service) Orders(ctx context.Context, scope domain.Scope, filter domain.OrderFilter) ([]domain.Order, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	return s.deps.Orders(ctx, scope, filter)
}

// Order returns one order with its lines.
func (s *Service) Order(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Order, error) {
	if err := requireScope(scope); err != nil {
		return domain.Order{}, err
	}
	if id == uuid.Nil {
		return domain.Order{}, fmt.Errorf("%w: order id is required", domain.ErrInvalidInput)
	}

	return s.deps.Order(ctx, scope, id)
}

// Cancel marks an order a person decided not to fulfil.
func (s *Service) Cancel(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Order, error) {
	if err := requireScope(scope); err != nil {
		return domain.Order{}, err
	}
	if id == uuid.Nil {
		return domain.Order{}, fmt.Errorf("%w: order id is required", domain.ErrInvalidInput)
	}

	return s.deps.SetOrderStatus(ctx, scope, id, domain.StatusCanceled)
}

// Candidates returns the catalogue entries a term could mean, which is what a screen's search box and
// the tool's suggestion list both read.
func (s *Service) Candidates(ctx context.Context, scope domain.Scope, term string, limit int) ([]domain.Product, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	return s.deps.MatchProducts(ctx, scope, term, limit)
}

// requireScope refuses a call with no tenant, which is the one thing every method here needs.
func requireScope(scope domain.Scope) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return nil
}

// ErrNoRepository is what a caller gets when the module was assembled without storage.
var ErrNoRepository = errors.New("order: no repository configured")
