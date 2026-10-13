// Package domain defines the order module's contract: the catalogue a workspace sells from, the
// customers it talks to, and the orders it took.
//
// The module exists because the MVP flow needed somewhere to put an order. A Bolu reads a customer's
// message, looks up what was ordered, records it, and composes the invoice a person approves — and the
// first two of those are writes, which need a table and a rule about who may write.
//
// Two rules are the whole reason this package is more than a table definition:
//
//   - **A price comes from the catalogue, never from the model.** `Record` takes product names and
//     quantities; the price is resolved from `products` inside the write. A model cannot pass a price,
//     so an invented one is impossible by construction rather than by instruction — which is what
//     makes "an incomplete order never produces a wrong invoice" a property of the code.
//   - **A line keeps its own copy of what was agreed.** The invoice a person approves and the record
//     it came from must not change when somebody edits a price tomorrow, so the line snapshots the
//     description, the unit, and the unit price while still pointing at the product it came from.
package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors this module returns.
var (
	// ErrInvalidInput is a request that cannot be honoured: a missing product name, a quantity that
	// is not positive, an order with no lines.
	ErrInvalidInput = errors.New("order: invalid input")
	// ErrNotFound is a product, customer, or order that is not in this workspace.
	ErrNotFound = errors.New("order: not found")
	// ErrProductUnknown is what a record request names and the catalogue does not have. It carries
	// the candidates, so the caller can ask a person rather than guess.
	ErrProductUnknown = errors.New("order: the product is not in the catalogue")
	// ErrDuplicateProduct is a product whose name already exists in the workspace.
	ErrDuplicateProduct = errors.New("order: a product with that name already exists")
)

// Order statuses. There is deliberately no `invoiced`: the records of an invoice leaving the building
// are the approval draft and the outbound message, and a status the product never transitions would
// be a column that lies.
const (
	StatusRecorded = "recorded"
	StatusCanceled = "canceled"
)

// Statuses is every legal value, which is what a check constraint and a screen both read.
var Statuses = []string{StatusRecorded, StatusCanceled}

// Scope is the tenant a call runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope names no workspace.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Product is one thing a workspace sells.
type Product struct {
	ID   uuid.UUID
	SKU  string
	Name string
	// UnitPriceMicros is the price of one unit, in the micro-rupiah the usage ledger also uses, so an
	// invoice total and a cost are the same unit rather than two numbers somebody adds together.
	UnitPriceMicros int64
	// Unit is how it is sold: "pcs", "kg", "box". It travels onto the line so an invoice reads "2 kg".
	Unit string
	// Active hides a product from the catalogue without deleting it: what stopped being sold must not
	// disappear from the orders that sold it.
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PriceIDR is the price in rupiah, which is what a person reads and what a model is told.
//
// Both figures travel to the model for the reason the usage report carries both: a model reading
// `15000000` micro-rupiah will quote a customer fifteen million rupiah.
func (p Product) PriceIDR() float64 { return float64(p.UnitPriceMicros) / 1_000_000 }

// Customer is somebody a workspace sells to.
type Customer struct {
	ID    uuid.UUID
	Name  string
	Phone string
	Email string
	Note  string
	// Orders is how many orders this customer has, which is what makes "pelanggan baru" answerable.
	Orders    int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OrderLine is one product on an order, with its own copy of what was agreed.
type OrderLine struct {
	ID          uuid.UUID
	OrderID     uuid.UUID
	ProductID   uuid.UUID
	Description string
	Unit        string
	Quantity    float64
	// UnitPriceMicros is the catalogue's price at the moment the order was taken, copied onto the
	// line rather than read through the product.
	UnitPriceMicros int64
	// LineTotalMicros is computed by the database from the quantity and the unit price, so a line's
	// total cannot disagree with its own two fields.
	LineTotalMicros int64
}

// TotalIDR is the line's total in rupiah.
// PriceIDR is the agreed unit price in rupiah, which is what a person and a model read. The stored
// value is micro-rupiah, so the conversion is one place: a line that divided by a thousand somewhere
// else would be a quote a thousand times off.
func (l OrderLine) PriceIDR() float64 { return float64(l.UnitPriceMicros) / 1_000_000 }

// TotalIDR is the line's total in rupiah, as the database computed it.
func (l OrderLine) TotalIDR() float64 { return float64(l.LineTotalMicros) / 1_000_000 }

// Order is one customer's order.
type Order struct {
	ID       uuid.UUID
	Code     string
	Status   string
	Note     string
	AgentID  uuid.UUID
	TaskID   uuid.UUID
	Customer Customer
	// TotalMicros is the sum of the lines, written by the statement that inserted them.
	TotalMicros int64
	Lines       []OrderLine
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TotalIDR is the order's total in rupiah.
func (o Order) TotalIDR() float64 { return float64(o.TotalMicros) / 1_000_000 }

// UnknownProductError is what recording an order returns for a product the catalogue does not have.
//
// It carries the candidates — with their prices, which is the part that saves a round trip: a model
// that reads "kandidat: Kopi Arabika (Rp 15.000)" can ask the customer which one they meant, while a
// model that reads a bare "not found" tends to try another name it invented.
type UnknownProductError struct {
	// Name is what the caller asked for.
	Name string
	// Candidates are the catalogue entries it could mean, best match first.
	Candidates []Product
}

// Error implements error, and is written for the model to read: it says what was asked for, what the
// catalogue does have, and what to do next.
func (e *UnknownProductError) Error() string {
	if len(e.Candidates) == 0 {
		return fmt.Sprintf(
			"%s: %q tidak ada di katalog workspace ini. Tanyakan ke pemilik usaha untuk menambahkannya; jangan mengarang harga.",
			ErrProductUnknown, e.Name)
	}

	options := make([]string, 0, len(e.Candidates))
	for _, candidate := range e.Candidates {
		options = append(options, fmt.Sprintf("%s (%s)", candidate.Name, Rupiah(candidate.PriceIDR())))
	}

	return fmt.Sprintf(
		"%s: %q tidak ada di katalog. Kandidat: %s. Tanyakan ke pelanggan produk mana yang dimaksud, atau pakai salah satu kandidat kalau memang itu maksudnya; jangan mengarang harga.",
		ErrProductUnknown, e.Name, strings.Join(options, ", "))
}

// Unwrap makes the error match ErrProductUnknown while keeping the candidates readable.
func (e *UnknownProductError) Unwrap() error { return ErrProductUnknown }

// Rupiah renders an amount the way a person writes it: "Rp 15000", and "Rp 15000,50" when there are
// sen. It is exported because the tool adapter writes summaries a person reads, and two renderings of
// the same amount is two answers to what a price looks like — one of which will eventually end a
// sentence with a decimal point.
func Rupiah(amount float64) string {
	whole := int64(amount)
	// Rounded rather than truncated: 12345.999 rupiah is 12346 to everybody except a cast.
	fraction := int64((amount-float64(whole))*100 + 0.5)
	if fraction >= 100 {
		whole++
		fraction = 0
	}

	if fraction == 0 {
		return fmt.Sprintf("Rp %d", whole)
	}

	return fmt.Sprintf("Rp %d,%02d", whole, fraction)
}

// NewProduct is a product to insert.
type NewProduct struct {
	SKU             string
	Name            string
	UnitPriceMicros int64
	Unit            string
	Active          bool
}

// ProductChange is one edit. A nil field is left alone.
type ProductChange struct {
	SKU             *string
	Name            *string
	UnitPriceMicros *int64
	Unit            *string
	Active          *bool
}

// ProductFilter narrows a catalogue read.
type ProductFilter struct {
	// Search matches a name, case insensitively.
	Search string
	// IncludeInactive returns products that are no longer sold.
	IncludeInactive bool
	Limit           int
}

// NewItem is one line of an order, named by the product rather than by a price.
type NewItem struct {
	// Product names a catalogue entry. The price is resolved from it inside the write.
	Product string
	// Quantity may be fractional, because a product sold by weight is.
	Quantity float64
	// Note is anything the customer said about this line.
	Note string
	// Description overrides the catalogue name when the customer used different words.
	Description string
}

// NewOrder is an order to record.
type NewOrder struct {
	// CustomerPhone is the identity of the buyer. It is what a second order from the same person
	// resolves to.
	CustomerPhone string
	CustomerName  string
	AgentID       uuid.UUID
	TaskID        uuid.UUID
	Note          string
	Items         []NewItem
}

// OrderFilter narrows an order read.
type OrderFilter struct {
	// CustomerPhone limits the read to one customer.
	CustomerPhone string
	Limit         int
}

// DefaultLimit bounds a list when the caller does not say how many.
const DefaultLimit = 50

// MaxLimit bounds one page.
const MaxLimit = 200

// Normalised returns the filter with its bounds applied, so every caller reads the same page size.
func (f ProductFilter) Normalised() ProductFilter {
	switch {
	case f.Limit <= 0:
		f.Limit = DefaultLimit
	case f.Limit > MaxLimit:
		f.Limit = MaxLimit
	}

	return f
}

// Normalised returns the filter with its bounds applied.
func (f OrderFilter) Normalised() OrderFilter {
	switch {
	case f.Limit <= 0:
		f.Limit = DefaultLimit
	case f.Limit > MaxLimit:
		f.Limit = MaxLimit
	}

	return f
}

// Repository is the persistence contract.
type Repository interface {
	// --- the catalogue

	Products(ctx context.Context, scope Scope, filter ProductFilter) ([]Product, error)
	Product(ctx context.Context, scope Scope, id uuid.UUID) (Product, error)
	CreateProduct(ctx context.Context, scope Scope, product NewProduct) (Product, error)
	UpdateProduct(ctx context.Context, scope Scope, id uuid.UUID, change ProductChange) (Product, error)
	DeleteProduct(ctx context.Context, scope Scope, id uuid.UUID) error
	// MatchProducts finds the catalogue entries a name could mean, best match first. It is what turns
	// "the product is not in the catalogue" into "did you mean one of these".
	MatchProducts(ctx context.Context, scope Scope, term string, limit int) ([]Product, error)

	// --- the flow

	// Record writes an order, its lines, and the customer in one transaction, and answers with what
	// it stored. A product the catalogue does not have is refused with ErrProductUnknown and the
	// candidates, and nothing is written.
	Record(ctx context.Context, scope Scope, order NewOrder) (Order, error)
	Orders(ctx context.Context, scope Scope, filter OrderFilter) ([]Order, error)
	Order(ctx context.Context, scope Scope, id uuid.UUID) (Order, error)
	SetOrderStatus(ctx context.Context, scope Scope, id uuid.UUID, status string) (Order, error)
}

// Service is what the handler and the tool adapter call.
type Service interface {
	// --- the catalogue, for the screen that maintains it

	Products(ctx context.Context, scope Scope, filter ProductFilter) ([]Product, error)
	Product(ctx context.Context, scope Scope, id uuid.UUID) (Product, error)
	CreateProduct(ctx context.Context, scope Scope, request CreateProductRequest) (Product, error)
	UpdateProduct(ctx context.Context, scope Scope, id uuid.UUID, request UpdateProductRequest) (Product, error)
	DeleteProduct(ctx context.Context, scope Scope, id uuid.UUID) error

	// --- the flow

	// Lookup answers what a name means in this workspace's catalogue: the product, or the candidates
	// it could be, or neither.
	Lookup(ctx context.Context, scope Scope, term string) (Lookup, error)
	// Record writes an order from product names and quantities. The prices come from the catalogue.
	Record(ctx context.Context, scope Scope, order NewOrder) (Order, error)
	Orders(ctx context.Context, scope Scope, filter OrderFilter) ([]Order, error)
	Order(ctx context.Context, scope Scope, id uuid.UUID) (Order, error)
	// Cancel marks an order a person decided not to fulfil.
	Cancel(ctx context.Context, scope Scope, id uuid.UUID) (Order, error)
}

// CreateProductRequest is a catalogue entry from the screen.
type CreateProductRequest struct {
	SKU             string
	Name            string
	UnitPriceMicros int64
	Unit            string
}

// UpdateProductRequest is one edit from the screen.
type UpdateProductRequest struct {
	SKU             *string
	Name            *string
	UnitPriceMicros *int64
	Unit            *string
	Active          *bool
}

// Lookup is what a product name resolved to.
//
// It carries three answers rather than one because the flow needs all three: an exact product to
// price an order, candidates to ask a person about, and "nothing at all" to tell the customer the
// product is not sold.
type Lookup struct {
	// Found is the product whose name matches, when one does.
	Found *Product
	// Candidates are the catalogue entries the term could mean, best match first. It is never empty
	// when Found is nil and the workspace has any product at all.
	Candidates []Product
}
