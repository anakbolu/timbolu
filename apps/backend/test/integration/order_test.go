package integration

import (
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	authrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/repository"
	orderadapter "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/adapter"
	orderdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain"
	orderrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/repository"
	orderservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/order/service"
	workspacedomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/repository"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// This file is EPIC 14's proof against a real Postgres. The claims are the ones a mock would only
// restate, because they are made by the schema rather than by the service:
//
//   - the catalogue and the orders are tenant data, isolated by policy rather than by a careful query;
//   - a line carries the catalogue's price and its own copy of the fields an invoice is read from,
//     and editing the catalogue afterwards does not rewrite an order that was already taken;
//   - a line's total is the database's `round(quantity * price)` and an order's total is the sum of
//     its lines, so no Go arithmetic can disagree with either;
//   - a line naming a product the catalogue does not have refuses the whole order and leaves no row
//     behind — not the header, not the customer;
//   - a customer is one row per phone however the number arrives, and that phone is how the flow's
//     second question ("what did this customer order") is answered;
//   - the tool catalogue in the database and the adapter's own declaration are the same three tools
//     with the same labels, which is the agreement the adapter's comments promise.

// orderFixture is a workspace of its own with the order module over the real database.
//
// The workspace is onboarded through the module that owns it rather than inserted, so its rows live in
// a tenant the product itself could have created — which is what makes the policy assertions here
// claims about this deployment rather than about a test's hand-written insert.
type orderFixture struct {
	repository *orderrepo.Repository
	service    *orderservice.Service
	pool       *database.Pool

	userID    uuid.UUID
	workspace uuid.UUID
	scope     orderdomain.Scope
}

// newOrderFixture onboards a workspace of its own for one test.
func newOrderFixture(t *testing.T, dsn string) *orderFixture {
	t.Helper()

	return orderFixtureOver(t, poolFor(t, dsn))
}

// orderFixtureOver builds the fixture over a pool that is already open, so one test can hold two
// workspaces of the same database and compare them — which is what the isolation claim needs.
func orderFixtureOver(t *testing.T, pool *database.Pool) *orderFixture {
	t.Helper()

	ctx := t.Context()
	user, err := authrepo.New(pool.PgxPool()).CreateUser(ctx, uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := workspacerepo.New(pool).Onboard(ctx, user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Order",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	repository := orderrepo.New(pool)

	return &orderFixture{
		repository: repository,
		service:    orderservice.New(orderservice.Deps{Repository: repository}),
		pool:       pool,
		userID:     user.ID,
		workspace:  result.Workspace.ID,
		scope:      orderdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID},
	}
}

// sell adds a catalogue entry through the service, which is the path the screen takes.
func (f *orderFixture) sell(t *testing.T, product orderdomain.CreateProductRequest) orderdomain.Product {
	t.Helper()

	created, err := f.service.CreateProduct(t.Context(), f.scope, product)
	require.NoError(t, err)

	return created
}

// record writes an order and reads it back through the repository, so an assertion is about what was
// stored rather than about what the call happened to return.
func (f *orderFixture) record(t *testing.T, order orderdomain.NewOrder) orderdomain.Order {
	t.Helper()

	recorded, err := f.service.Record(t.Context(), f.scope, order)
	require.NoError(t, err)

	return f.read(t, recorded.ID)
}

// read re-reads one order from the database.
func (f *orderFixture) read(t *testing.T, id uuid.UUID) orderdomain.Order {
	t.Helper()

	stored, err := f.repository.Order(t.Context(), f.scope, id)
	require.NoError(t, err)

	return stored
}

// orders lists the workspace's orders the way a screen or the read tool does.
func (f *orderFixture) orders(t *testing.T, filter orderdomain.OrderFilter) []orderdomain.Order {
	t.Helper()

	listed, err := f.repository.Orders(t.Context(), f.scope, filter)
	require.NoError(t, err)

	return listed
}

// countInWorkspace reads a count inside the workspace's scope, because every order table carries Row
// Level Security: a count outside a scope returns zero whatever the table holds, which would make an
// assertion pass for the wrong reason.
func (f *orderFixture) countInWorkspace(t *testing.T, query string, args ...any) int {
	t.Helper()

	var count int
	require.NoError(t, f.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      f.userID,
		WorkspaceID: f.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), query, args...).Scan(&count)
	}))

	return count
}

// lineFor returns the line one product was recorded on.
//
// The lines come back ordered by their own ids — which are generated, not sequential — rather than by
// the order they were written in, so a test must name the line it means.
func lineFor(t *testing.T, order orderdomain.Order, productID uuid.UUID) orderdomain.OrderLine {
	t.Helper()

	for _, line := range order.Lines {
		if line.ProductID == productID {
			return line
		}
	}

	t.Fatalf("order %s has no line for product %s", order.Code, productID)

	return orderdomain.OrderLine{}
}

// roundMicros is the database's own formula for a line total, written out so the assertion reads as
// the rule the generated column implements: `round(quantity * unit_price_micros)`.
func roundMicros(quantity float64, unitPriceMicros int64) int64 {
	return int64(math.Round(quantity * float64(unitPriceMicros)))
}

// TestTheOrderCatalogueIsTenantIsolated is the policy rather than the query: a product another
// workspace sells cannot be read from this one even when it is named by id, and a write into another
// workspace is refused by the policy rather than by the statement's own filter.
func TestTheOrderCatalogueIsTenantIsolated(t *testing.T) {
	dsn := appDatabase(t)
	pool := poolFor(t, dsn)
	mine := orderFixtureOver(t, pool)
	theirs := orderFixtureOver(t, pool)
	ctx := t.Context()

	product := theirs.sell(t, orderdomain.CreateProductRequest{
		Name: "Kopi Arabika", UnitPriceMicros: 15_000_000, Unit: "kg",
	})

	// Naming the row by its id is not enough: the policy filters it out of the read.
	_, err := mine.repository.Product(ctx, mine.scope, product.ID)
	require.ErrorIs(t, err, orderdomain.ErrNotFound)

	listed, err := mine.repository.Products(ctx, mine.scope, orderdomain.ProductFilter{})
	require.NoError(t, err)
	require.Empty(t, listed, "another workspace's catalogue is not this one's")

	// The refusal above is the isolation rather than an empty database: the workspace that owns the
	// product reads it back.
	owned, err := theirs.repository.Products(ctx, theirs.scope, orderdomain.ProductFilter{})
	require.NoError(t, err)
	require.Len(t, owned, 1)
	require.Equal(t, product.ID, owned[0].ID)

	// An unscoped read matches no tenant at all, which is what makes a forgotten tenant setting fail
	// closed instead of open.
	var visible int
	require.NoError(t, pool.InScopeRead(ctx, database.Scope{UserID: mine.userID}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM products`).Scan(&visible)
	}))
	require.Zero(t, visible, "a read with no active workspace sees nothing")

	// And a write from outside is refused by the policy: the insert names their workspace.
	require.Error(t, pool.InScope(ctx, database.Scope{
		UserID:      mine.userID,
		WorkspaceID: mine.workspace,
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO products (workspace_id, name, unit_price_micros, unit)
			VALUES ($1, 'Kopi Selundup', 1, 'pcs')`, theirs.workspace)
		return err
	}), "a product cannot be written into another workspace")
}

// TestARecordedOrderKeepsTheCataloguesPrice is the rule the module exists for: the line's description,
// unit, and price are the catalogue's, its total is the database's `round(quantity * price)`, and the
// order's total is the sum of its lines — never the caller's arithmetic, because a caller (a model)
// has no field to pass a price in.
func TestARecordedOrderKeepsTheCataloguesPrice(t *testing.T) {
	fixture := newOrderFixture(t, appDatabase(t))

	coffee := fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Kopi Arabika", UnitPriceMicros: 15_000_000, Unit: "kg",
	})
	sugar := fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Gula Aren", UnitPriceMicros: 3_333_333, Unit: "kg",
	})

	order := fixture.record(t, orderdomain.NewOrder{
		CustomerPhone: "0812-3456-7890",
		CustomerName:  "Bu Sari",
		Items: []orderdomain.NewItem{
			{Product: "Kopi Arabika", Quantity: 2.5},
			// The name is typed in another case on purpose: what the customer calls the product is not
			// what the invoice is spelled from.
			{Product: "gula aren", Quantity: 1.5},
		},
	})
	require.Len(t, order.Lines, 2)

	coffeeLine := lineFor(t, order, coffee.ID)
	require.Equal(t, coffee.Name, coffeeLine.Description, "the line is spelled from the catalogue")
	require.Equal(t, coffee.Unit, coffeeLine.Unit, "and sold in the unit the catalogue says")
	require.Equal(t, coffee.UnitPriceMicros, coffeeLine.UnitPriceMicros)
	require.Equal(t, 2.5, coffeeLine.Quantity)
	require.Equal(t, roundMicros(2.5, coffee.UnitPriceMicros), coffeeLine.LineTotalMicros)
	require.EqualValues(t, 37_500_000, coffeeLine.LineTotalMicros)

	sugarLine := lineFor(t, order, sugar.ID)
	require.Equal(t, sugar.UnitPriceMicros, sugarLine.UnitPriceMicros)
	// round(1.5 * 3_333_333) = round(4_999_999.5) = 5_000_000: the generated column rounds rather than
	// truncates, so a line never quietly loses micro-rupiah.
	require.Equal(t, roundMicros(1.5, sugar.UnitPriceMicros), sugarLine.LineTotalMicros)
	require.EqualValues(t, 5_000_000, sugarLine.LineTotalMicros)

	// The order's total is the sum of the line totals the database wrote, which is the number an
	// invoice quotes.
	var summed int64
	for _, line := range order.Lines {
		summed += line.LineTotalMicros
	}
	require.Equal(t, summed, order.TotalMicros)
	require.EqualValues(t, 42_500_000, order.TotalMicros)
}

// TestAPriceChangeDoesNotRewriteARecordedOrder is the second half of the line's snapshot: an invoice is
// a record of what was agreed, so a catalogue edit tomorrow must not change what a customer was quoted
// today. The line still points at the product it came from.
func TestAPriceChangeDoesNotRewriteARecordedOrder(t *testing.T) {
	fixture := newOrderFixture(t, appDatabase(t))
	ctx := t.Context()

	coffee := fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Kopi Arabika", UnitPriceMicros: 15_000_000, Unit: "kg",
	})

	order := fixture.record(t, orderdomain.NewOrder{
		CustomerPhone: "0812-3456-7890",
		CustomerName:  "Bu Sari",
		Items:         []orderdomain.NewItem{{Product: "Kopi Arabika", Quantity: 2}},
	})
	before := lineFor(t, order, coffee.ID)
	require.EqualValues(t, 30_000_000, before.LineTotalMicros)

	higher := int64(25_000_000)
	updated, err := fixture.service.UpdateProduct(ctx, fixture.scope, coffee.ID, orderdomain.UpdateProductRequest{
		UnitPriceMicros: &higher,
	})
	require.NoError(t, err)
	require.Equal(t, higher, updated.UnitPriceMicros, "the catalogue did change, so the assertion below is not vacuous")

	after := lineFor(t, fixture.read(t, order.ID), coffee.ID)
	require.Equal(t, before.UnitPriceMicros, after.UnitPriceMicros, "the line keeps the price it was quoted at")
	require.Equal(t, before.LineTotalMicros, after.LineTotalMicros)
	require.Equal(t, coffee.ID, after.ProductID, "and still says which catalogue entry it came from")
}

// TestAnUnknownProductRefusesTheWholeOrder is the refusal that keeps an invoice from being for part of
// what a customer asked for: the error names the product and carries what the catalogue does have, and
// the transaction leaves nothing behind — not the line that did resolve, not the header, not the
// customer the header would have been written with.
func TestAnUnknownProductRefusesTheWholeOrder(t *testing.T) {
	fixture := newOrderFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Kopi Arabika", UnitPriceMicros: 15_000_000, Unit: "kg",
	})

	before := fixture.orders(t, orderdomain.OrderFilter{})
	itemsBefore := fixture.countInWorkspace(t, `SELECT count(*) FROM order_items`)
	customersBefore := fixture.countInWorkspace(t, `SELECT count(*) FROM customers`)
	require.Empty(t, before, "the workspace starts with no orders, so the counts below mean something")

	_, err := fixture.service.Record(ctx, fixture.scope, orderdomain.NewOrder{
		CustomerPhone: "0812-3456-7890",
		CustomerName:  "Bu Sari",
		Items: []orderdomain.NewItem{
			{Product: "Kopi Arabika", Quantity: 2},
			// A name the catalogue does not have exactly, though it is close to one it does — the case
			// where guessing would produce an invoice for the wrong product.
			{Product: "kopi", Quantity: 1},
		},
	})
	require.ErrorIs(t, err, orderdomain.ErrProductUnknown)

	var unknown *orderdomain.UnknownProductError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, "kopi", unknown.Name)
	require.NotEmpty(t, unknown.Candidates, "the refusal carries what the catalogue does have")
	require.Equal(t, "Kopi Arabika", unknown.Candidates[0].Name)

	require.Len(t, fixture.orders(t, orderdomain.OrderFilter{}), len(before))
	require.Equal(t, itemsBefore, fixture.countInWorkspace(t, `SELECT count(*) FROM order_items`))
	require.Equal(t, customersBefore, fixture.countInWorkspace(t, `SELECT count(*) FROM customers`))
}

// TestOrdersFromTheSamePhoneShareOneCustomer is the identity the flow actually has: a WhatsApp
// conversation is a number, so a second order from the same person resolves to the same customer row
// however the number was written — and the name follows the order that was recorded last.
func TestOrdersFromTheSamePhoneShareOneCustomer(t *testing.T) {
	fixture := newOrderFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Kopi Arabika", UnitPriceMicros: 15_000_000, Unit: "kg",
	})

	first := fixture.record(t, orderdomain.NewOrder{
		CustomerPhone: "0812-3456-7890",
		CustomerName:  "Bu Sari",
		Items:         []orderdomain.NewItem{{Product: "Kopi Arabika", Quantity: 1}},
	})
	second := fixture.record(t, orderdomain.NewOrder{
		// The same person as the WhatsApp adapter would hand the number over.
		CustomerPhone: "6281234567890@s.whatsapp.net",
		CustomerName:  "Bu Sari Wijaya",
		Items:         []orderdomain.NewItem{{Product: "Kopi Arabika", Quantity: 2}},
	})

	require.NotEqual(t, uuid.Nil, first.Customer.ID, "the order points at a customer row")
	require.Equal(t, first.Customer.ID, second.Customer.ID, "one phone is one customer, whatever shape it arrived in")
	require.Equal(t, "6281234567890", second.Customer.Phone)
	require.NotEqual(t, first.ID, second.ID, "the second order is its own order")

	require.Equal(t, 1, fixture.countInWorkspace(t, `SELECT count(*) FROM customers`))

	var name string
	require.NoError(t, fixture.pool.InScopeRead(ctx, database.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT name FROM customers`).Scan(&name)
	}))
	require.Equal(t, "Bu Sari Wijaya", name, "the name follows the latest order")

	// The read the flow's second question is answered from, with the number typed the way a person
	// types it.
	found := fixture.orders(t, orderdomain.OrderFilter{CustomerPhone: "0812-3456-7890"})
	require.Len(t, found, 2)
	for _, order := range found {
		require.Equal(t, "6281234567890", order.Customer.Phone)
	}

	require.Empty(t, fixture.orders(t, orderdomain.OrderFilter{CustomerPhone: "0812-0000-0000"}),
		"another number is another customer's orders")
}

// TestOrderLookupIsCaseInsensitive is what keeps a customer's spelling from deciding whether the flow
// finds the product: the comparison is case-folded, and a term that names nothing exactly is still
// answered with the candidates it could mean — which is what a model asks about instead of inventing a
// price.
func TestOrderLookupIsCaseInsensitive(t *testing.T) {
	fixture := newOrderFixture(t, appDatabase(t))
	ctx := t.Context()

	coffee := fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Kopi Arabika", UnitPriceMicros: 15_000_000, Unit: "kg",
	})
	fixture.sell(t, orderdomain.CreateProductRequest{
		Name: "Gula Aren", UnitPriceMicros: 3_333_333, Unit: "kg",
	})

	lookup, err := fixture.service.Lookup(ctx, fixture.scope, "kopi arabika")
	require.NoError(t, err)
	require.NotNil(t, lookup.Found, "a name in another case is the same product")
	require.Equal(t, coffee.ID, lookup.Found.ID)

	absent, err := fixture.service.Lookup(ctx, fixture.scope, "kopi")
	require.NoError(t, err)
	require.Nil(t, absent.Found, "the term names no product exactly")
	require.NotEmpty(t, absent.Candidates, "but the catalogue has something similar, and that is the answer")

	names := make([]string, 0, len(absent.Candidates))
	for _, candidate := range absent.Candidates {
		names = append(names, candidate.Name)
	}
	require.Contains(t, names, "Kopi Arabika")
	require.NotContains(t, names, "Gula Aren")
}

// TestTheOrderToolsMatchTheDatabaseCatalogue reads the promise the adapter's comments make: the tools
// it declares are the rows the tool filter offers, with the same labels and with `needs_connection`
// false — because a catalogue and an order are the workspace's own data and nothing outside this
// deployment has to be connected for a Bolu to use them. A disagreement would either offer a tool the
// hub has nothing to run or hide one that needs nothing.
func TestTheOrderToolsMatchTheDatabaseCatalogue(t *testing.T) {
	fixture := newOrderFixture(t, appDatabase(t))
	ctx := t.Context()

	adapter := orderadapter.New(fixture.service)

	// Both directions are read: the rows that name this app, and any row whose name carries this app's
	// prefix — a row that says `order.` but belongs to another app would be a tool nothing can run.
	rows, err := fixture.pool.PgxPool().Query(ctx, `
		SELECT name, integration_app, label, needs_connection
		FROM tool_catalog
		WHERE integration_app = $1 OR name LIKE $2`, adapter.App(), orderadapter.App+".%")
	require.NoError(t, err)
	defer rows.Close()

	labels := map[string]string{}
	connections := map[string]bool{}
	for rows.Next() {
		var name, app, label string
		var needsConnection bool
		require.NoError(t, rows.Scan(&name, &app, &label, &needsConnection))
		require.Equal(t, adapter.App(), app, "a tool named after this app belongs to this app")
		labels[name] = label
		connections[name] = needsConnection
	}
	require.NoError(t, rows.Err())

	require.Equal(t, map[string]string{
		"order.find_product": "read",
		"order.record_order": "write_internal",
		"order.read_orders":  "read",
	}, labels)

	for _, tool := range adapter.Tools() {
		label, declared := labels[tool.Name]
		require.True(t, declared, "the database must have a catalogue row for %s", tool.Name)
		require.Equal(t, tool.Label, label, "the label the adapter declares is the one the tool filter reads")
		require.False(t, connections[tool.Name], "%s is the workspace's own data and needs no connected account", tool.Name)
	}

	// And the other way: a row with no declaration would be offered to a Bolu with nothing to run it.
	require.Len(t, labels, len(adapter.Tools()))
	require.False(t, adapter.NeedsConnection(), "the adapter and the rows agree that no connection is needed")
}
