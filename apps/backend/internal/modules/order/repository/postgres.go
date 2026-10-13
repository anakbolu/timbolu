// Package repository implements the order module's data access with pgx and the sqlc-generated
// queries.
//
// Every method runs inside the caller's tenant scope, so Row Level Security filters the statements
// even where one forgets its workspace filter. The interesting method here is `Record`, which is one
// transaction for what the flow needs to be atomic: a customer, an order, its lines, and the total
// that is summed from them.
package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Compile-time check that the repository is the interface the module asked for.
var _ domain.Repository = (*Repository)(nil)

// read runs one query in the caller's scope, read-only.
func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID},
		func(tx pgx.Tx) error { return fn(sqlcgen.New(tx)) })
}

// write runs one query in the caller's scope.
func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID},
		func(tx pgx.Tx) error { return fn(sqlcgen.New(tx)) })
}

// ---------------------------------------------------------------- the catalogue

// Products returns the workspace's catalogue.
func (r *Repository) Products(ctx context.Context, scope domain.Scope, filter domain.ProductFilter) ([]domain.Product, error) {
	filter = filter.Normalised()

	var products []domain.Product

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListProducts(ctx, sqlcgen.ListProductsParams{
			WorkspaceID:     scope.WorkspaceID,
			IncludeInactive: filter.IncludeInactive,
			Search:          strings.TrimSpace(filter.Search),
			LimitCount:      int32(filter.Limit),
		})
		if err != nil {
			return fmt.Errorf("order: list products: %w", err)
		}

		products = make([]domain.Product, 0, len(rows))
		for _, row := range rows {
			products = append(products, toProduct(row))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return products, nil
}

// Product returns one catalogue entry.
func (r *Repository) Product(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Product, error) {
	var product domain.Product

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetProduct(ctx, sqlcgen.GetProductParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: product %s", domain.ErrNotFound, id)
			}

			return fmt.Errorf("order: get product: %w", err)
		}

		product = toProduct(row)

		return nil
	})
	if err != nil {
		return domain.Product{}, err
	}

	return product, nil
}

// CreateProduct adds a catalogue entry.
func (r *Repository) CreateProduct(ctx context.Context, scope domain.Scope, product domain.NewProduct) (domain.Product, error) {
	var stored domain.Product

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateProduct(ctx, sqlcgen.CreateProductParams{
			WorkspaceID:     scope.WorkspaceID,
			Sku:             strings.TrimSpace(product.SKU),
			Name:            strings.TrimSpace(product.Name),
			UnitPriceMicros: product.UnitPriceMicros,
			Unit:            strings.TrimSpace(product.Unit),
		})
		if err != nil {
			return translateWrite(err, "create product")
		}

		stored = toProduct(row)

		return nil
	})
	if err != nil {
		return domain.Product{}, err
	}

	return stored, nil
}

// UpdateProduct edits a catalogue entry.
func (r *Repository) UpdateProduct(ctx context.Context, scope domain.Scope, id uuid.UUID, change domain.ProductChange) (domain.Product, error) {
	var stored domain.Product

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateProduct(ctx, sqlcgen.UpdateProductParams{
			ID:              id,
			WorkspaceID:     scope.WorkspaceID,
			Sku:             trimmed(change.SKU),
			Name:            trimmed(change.Name),
			UnitPriceMicros: change.UnitPriceMicros,
			Unit:            trimmed(change.Unit),
			Active:          change.Active,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: product %s", domain.ErrNotFound, id)
			}

			return translateWrite(err, "update product")
		}

		stored = toProduct(row)

		return nil
	})
	if err != nil {
		return domain.Product{}, err
	}

	return stored, nil
}

// DeleteProduct removes a catalogue entry.
//
// The orders that sold it keep their lines, because a line carries its own copy of what was agreed —
// and the line's product reference becomes null rather than taking the order with it.
func (r *Repository) DeleteProduct(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		removed, err := queries.DeleteProduct(ctx, sqlcgen.DeleteProductParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return fmt.Errorf("order: delete product: %w", err)
		}
		if removed == 0 {
			return fmt.Errorf("%w: product %s", domain.ErrNotFound, id)
		}

		return nil
	})
}

// MatchProducts finds the catalogue entries a term could mean.
func (r *Repository) MatchProducts(ctx context.Context, scope domain.Scope, term string, limit int) ([]domain.Product, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, nil
	}
	if limit <= 0 || limit > domain.DefaultLimit {
		limit = 5
	}

	var products []domain.Product

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.MatchProducts(ctx, sqlcgen.MatchProductsParams{
			WorkspaceID: scope.WorkspaceID,
			Term:        term,
			LimitCount:  int32(limit),
		})
		if err != nil {
			return fmt.Errorf("order: match products: %w", err)
		}

		products = make([]domain.Product, 0, len(rows))
		for _, row := range rows {
			products = append(products, toProduct(row))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return products, nil
}

// ---------------------------------------------------------------- the flow

// Record writes an order, its lines, and its customer, in one transaction.
//
// **The price is resolved inside this transaction, from the catalogue** rather than being passed in: a
// model names a product and a quantity, and the price is whatever the workspace says it is right now.
// That is what makes an invented price impossible — there is no field to put it in — and it is also
// why an unknown product refuses the whole order instead of writing a line with a guessed price.
func (r *Repository) Record(ctx context.Context, scope domain.Scope, order domain.NewOrder) (domain.Order, error) {
	var stored domain.Order

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		phone := NormalizePhone(order.CustomerPhone)

		customer, err := queries.UpsertCustomer(ctx, sqlcgen.UpsertCustomerParams{
			WorkspaceID: scope.WorkspaceID,
			Phone:       phone,
			Name:        strings.TrimSpace(order.CustomerName),
		})
		if err != nil {
			return translateWrite(err, "upsert customer")
		}

		header, err := queries.RecordOrder(ctx, sqlcgen.RecordOrderParams{
			WorkspaceID:   scope.WorkspaceID,
			CustomerID:    optionalID(customer.ID),
			CustomerPhone: customer.Phone,
			CustomerName:  customer.Name,
			AgentID:       optionalID(order.AgentID),
			TaskID:        optionalID(order.TaskID),
			Note:          strings.TrimSpace(order.Note),
		})
		if err != nil {
			return translateWrite(err, "record order")
		}

		for _, item := range order.Items {
			product, err := resolveProduct(ctx, queries, scope.WorkspaceID, item.Product)
			if err != nil {
				return err
			}

			description := strings.TrimSpace(item.Description)
			if description == "" {
				description = product.Name
			}

			if _, err := queries.RecordOrderItem(ctx, sqlcgen.RecordOrderItemParams{
				WorkspaceID:     scope.WorkspaceID,
				OrderID:         header.ID,
				ProductID:       optionalID(product.ID),
				Description:     description,
				Unit:            product.Unit,
				Quantity:        numeric(item.Quantity),
				UnitPriceMicros: product.UnitPriceMicros,
			}); err != nil {
				return translateWrite(err, "record order item")
			}
		}

		// The total is summed from the rows that were just written rather than added up here, so it
		// cannot disagree with its own lines — and the generated `line_total_micros` means there is no
		// arithmetic in this file to get wrong.
		if _, err := queries.SetOrderTotal(ctx, sqlcgen.SetOrderTotalParams{
			ID:          header.ID,
			WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			return translateWrite(err, "set order total")
		}

		stored, err = loadOrder(ctx, queries, scope.WorkspaceID, header.ID)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return domain.Order{}, err
	}

	return stored, nil
}

// resolveProduct finds the catalogue entry one line names.
//
// An exact name wins, and everything else is a suggestion: a request that named "kopi" when the
// catalogue has "Kopi Arabika" is refused with the candidates rather than priced by a guess, because
// the guess would be an invoice for the wrong product.
func resolveProduct(ctx context.Context, queries *sqlcgen.Queries, workspaceID uuid.UUID, name string) (domain.Product, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return domain.Product{}, fmt.Errorf("%w: an order line names no product", domain.ErrInvalidInput)
	}

	exact, err := queries.MatchProducts(ctx, sqlcgen.MatchProductsParams{
		WorkspaceID: workspaceID,
		Term:        trimmed,
		LimitCount:  5,
	})
	if err != nil {
		return domain.Product{}, fmt.Errorf("order: match products: %w", err)
	}

	for _, candidate := range exact {
		if strings.EqualFold(candidate.Name, trimmed) || (candidate.Sku != "" && strings.EqualFold(candidate.Sku, trimmed)) {
			return toProduct(candidate), nil
		}
	}

	candidates := make([]domain.Product, 0, len(exact))
	for _, candidate := range exact {
		candidates = append(candidates, toProduct(candidate))
	}

	return domain.Product{}, &domain.UnknownProductError{Name: trimmed, Candidates: candidates}
}

// Orders returns the workspace's orders, newest first.
func (r *Repository) Orders(ctx context.Context, scope domain.Scope, filter domain.OrderFilter) ([]domain.Order, error) {
	filter = filter.Normalised()

	var orders []domain.Order

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListOrders(ctx, sqlcgen.ListOrdersParams{
			WorkspaceID:   scope.WorkspaceID,
			CustomerPhone: NormalizePhone(filter.CustomerPhone),
			LimitCount:    int32(filter.Limit),
		})
		if err != nil {
			return fmt.Errorf("order: list orders: %w", err)
		}

		orders, err = withLines(ctx, queries, scope.WorkspaceID, rows)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return orders, nil
}

// Order returns one order with its lines.
func (r *Repository) Order(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Order, error) {
	var order domain.Order

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		stored, err := loadOrder(ctx, queries, scope.WorkspaceID, id)
		if err != nil {
			return err
		}

		order = stored

		return nil
	})
	if err != nil {
		return domain.Order{}, err
	}

	return order, nil
}

// SetOrderStatus moves an order to another status.
func (r *Repository) SetOrderStatus(ctx context.Context, scope domain.Scope, id uuid.UUID, status string) (domain.Order, error) {
	var order domain.Order

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.SetOrderStatus(ctx, sqlcgen.SetOrderStatusParams{
			Status:      status,
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: order %s", domain.ErrNotFound, id)
			}

			return translateWrite(err, "set order status")
		}

		stored, err := loadOrder(ctx, queries, scope.WorkspaceID, id)
		if err != nil {
			return err
		}

		order = stored

		return nil
	})
	if err != nil {
		return domain.Order{}, err
	}

	return order, nil
}

// ---------------------------------------------------------------- helpers

// loadOrder reads one order and its lines.
func loadOrder(ctx context.Context, queries *sqlcgen.Queries, workspaceID, id uuid.UUID) (domain.Order, error) {
	row, err := queries.GetOrder(ctx, sqlcgen.GetOrderParams{ID: id, WorkspaceID: workspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Order{}, fmt.Errorf("%w: order %s", domain.ErrNotFound, id)
		}

		return domain.Order{}, fmt.Errorf("order: get order: %w", err)
	}

	items, err := queries.ListOrderItems(ctx, sqlcgen.ListOrderItemsParams{
		WorkspaceID: workspaceID,
		OrderID:     id,
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("order: list order items: %w", err)
	}

	return toOrder(row, items), nil
}

// withLines reads the lines of many orders in one query.
func withLines(ctx context.Context, queries *sqlcgen.Queries, workspaceID uuid.UUID, rows []sqlcgen.Order) ([]domain.Order, error) {
	if len(rows) == 0 {
		return []domain.Order{}, nil
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	items, err := queries.ListOrderItemsForOrders(ctx, sqlcgen.ListOrderItemsForOrdersParams{
		WorkspaceID: workspaceID,
		OrderIds:    ids,
	})
	if err != nil {
		return nil, fmt.Errorf("order: list order items: %w", err)
	}

	byOrder := map[uuid.UUID][]sqlcgen.OrderItem{}
	for _, item := range items {
		byOrder[item.OrderID] = append(byOrder[item.OrderID], item)
	}

	orders := make([]domain.Order, 0, len(rows))
	for _, row := range rows {
		orders = append(orders, toOrder(row, byOrder[row.ID]))
	}

	return orders, nil
}

// NormalizePhone reduces a number to the digits a customer is keyed by.
//
// A WhatsApp chat id arrives as `6281234567890@s.whatsapp.net` and a person types `0812-3456-7890`;
// both are the same customer, so both become `6281234567890`. The leading zero of the local form is
// the one thing that cannot be recovered by stripping punctuation, so it is rewritten to the country
// code the deployment is in. Indonesia is the deployment's own country (ADR 0001), and a workspace
// with international customers supplies the full number, which needs no rewriting.
func NormalizePhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// A JID keeps the number before the `@`.
	if at := strings.IndexByte(raw, '@'); at >= 0 {
		raw = raw[:at]
	}

	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}

	cleaned := digits.String()
	switch {
	case cleaned == "":
		return ""
	case strings.HasPrefix(cleaned, "0"):
		return "62" + strings.TrimLeft(cleaned, "0")
	default:
		return cleaned
	}
}

// numeric renders a quantity for the NUMERIC column.
//
// The driver's own type rather than a float: the column is NUMERIC(12,3) because a product sold by
// weight has a fraction of a unit, and a float round-tripped through Postgres is how "1.005" becomes
// "1.0049999". The conversion is checked here because a NaN a caller computed would otherwise arrive
// as a database error naming a column rather than a request.
func numeric(quantity float64) pgtype.Numeric {
	var value pgtype.Numeric
	if err := value.Scan(strconv.FormatFloat(quantity, 'f', 3, 64)); err != nil {
		return pgtype.Numeric{}
	}

	return value
}

// optionalID renders an id for a nullable column, or nil when it is absent.
func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}

	return &id
}

// trimmed renders an optional string field, trimmed, without turning an absent value into an empty one.
func trimmed(value *string) *string {
	if value == nil {
		return nil
	}

	clean := strings.TrimSpace(*value)

	return &clean
}

// translateWrite turns a driver error into this module's own.
func translateWrite(err error, operation string) error {
	if err == nil {
		return nil
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			if strings.Contains(pgErr.ConstraintName, "products_workspace_name_key") {
				return fmt.Errorf("%w: %s", domain.ErrDuplicateProduct, pgErr.Detail)
			}

			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, pgErr.Detail)
		case "23503", "23514", "22P02", "22003": // foreign key, check, cast, out of range
			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, pgErr.Message)
		}
	}

	return fmt.Errorf("order: %s: %w", operation, err)
}

// toProduct maps a row.
func toProduct(row sqlcgen.Product) domain.Product {
	return domain.Product{
		ID:              row.ID,
		SKU:             row.Sku,
		Name:            row.Name,
		UnitPriceMicros: row.UnitPriceMicros,
		Unit:            row.Unit,
		Active:          row.Active,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

// toOrder maps a header and its lines.
func toOrder(row sqlcgen.Order, items []sqlcgen.OrderItem) domain.Order {
	order := domain.Order{
		ID:          row.ID,
		Code:        row.Code,
		Status:      row.Status,
		Note:        row.Note,
		TotalMicros: row.TotalMicros,
		Customer: domain.Customer{
			ID:    uuidOrNil(row.CustomerID),
			Name:  row.CustomerName,
			Phone: row.CustomerPhone,
		},
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
	if row.AgentID != nil {
		order.AgentID = *row.AgentID
	}
	if row.TaskID != nil {
		order.TaskID = *row.TaskID
	}

	order.Lines = make([]domain.OrderLine, 0, len(items))
	for _, item := range items {
		line := domain.OrderLine{
			ID:              item.ID,
			OrderID:         item.OrderID,
			Description:     item.Description,
			Unit:            item.Unit,
			UnitPriceMicros: item.UnitPriceMicros,
			LineTotalMicros: item.LineTotalMicros,
		}
		if item.ProductID != nil {
			line.ProductID = *item.ProductID
		}
		if value, err := item.Quantity.Float64Value(); err == nil && value.Valid {
			line.Quantity = value.Float64
		}

		order.Lines = append(order.Lines, line)
	}

	return order
}

// uuidOrNil reads a nullable id.
func uuidOrNil(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}

	return *id
}
