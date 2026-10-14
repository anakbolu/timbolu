package handler

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// This file is the wire and nothing else: the fields a screen sends and reads, and the status and
// code it branches on. What the catalogue and the orders mean is the service's business and is tested
// there; a test that reached past the request into the rule would pass while the JSON was wrong.

// newTestApp mounts both route sets behind the identity the container's middlewares would have put on
// the request: the authenticated user, the verified tenant scope, and the workspace id the request
// carries.
func newTestApp(t *testing.T, service domain.Service, workspaceID uuid.UUID) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fiberErr *fiber.Error
			code := fiber.StatusInternalServerError
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	group := app.Group("/api", func(c *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		// The request context caches the workspace the request named, and the handler reads that
		// local rather than the scope. The key is unexported, so the shape here is the one the
		// accessor reads.
		c.Locals("workspace_id", workspaceID.String())
		return c.Next()
	})

	h := New(service, zap.NewNop())
	Routes(group, h)
	ManagerRoutes(group, h)

	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	for name, values := range resp.Header {
		for _, value := range values {
			recorder.Header().Add(name, value)
		}
	}
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

// asWorkspace matches a call that ran as the tenant the request named and as a real user. A handler
// that passed a zero workspace would read another workspace's catalogue or nothing at all, and the
// only place that is visible is here.
func asWorkspace(workspaceID uuid.UUID) interface{} {
	return mock.MatchedBy(func(scope domain.Scope) bool {
		return scope.WorkspaceID == workspaceID && scope.UserID != uuid.Nil
	})
}

// microsPerRupiah is the factor between the rupiah a screen sends and the micro-rupiah everything
// stores: the same one the usage report and the browser divide by.
const microsPerRupiah = 1_000_000

// productFixture is one catalogue entry in the unit the module stores, so the wire has to convert it
// back to rupiah to be right. The price is Rp 15.000 a kilo.
func productFixture() domain.Product {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

	return domain.Product{
		ID:              uuid.New(),
		SKU:             "KOPI-1",
		Name:            "Kopi Arabika",
		UnitPriceMicros: 15_000 * microsPerRupiah,
		Unit:            "kg",
		Active:          true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// orderFixture is one recorded order: two kilos at the catalogue's price, with the line carrying its
// own copy of what was agreed.
func orderFixture(agentID, taskID uuid.UUID) domain.Order {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	orderID := uuid.New()

	return domain.Order{
		ID:       orderID,
		Code:     "ORD-2026-0001",
		Status:   domain.StatusRecorded,
		Note:     "kirim siang",
		AgentID:  agentID,
		TaskID:   taskID,
		Customer: domain.Customer{ID: uuid.New(), Name: "Siti", Phone: "0812345678"},
		// The total is the sum the database wrote, in micro-rupiah like every price: two kilos of the
		// Rp 15.000 coffee.
		TotalMicros: 30_000 * microsPerRupiah,
		Lines: []domain.OrderLine{{
			ID:              uuid.New(),
			OrderID:         orderID,
			ProductID:       uuid.New(),
			Description:     "Kopi Arabika",
			Unit:            "kg",
			Quantity:        2,
			UnitPriceMicros: 15_000 * microsPerRupiah,
			LineTotalMicros: 30_000 * microsPerRupiah,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// TestTheCatalogueIsInRupiahAndCarriesItsCount: a screen renders prices in rupiah, so micro-rupiah on
// the wire would show fifteen million for a kilo of coffee. The filter is passed through whole
// because it is what the search field and the "show archived" toggle ask for.
func TestTheCatalogueIsInRupiahAndCarriesItsCount(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	product := productFixture()

	svc.EXPECT().Products(mock.Anything, mock.Anything, domain.ProductFilter{
		Search:          "kopi",
		IncludeInactive: true,
		Limit:           5,
	}).Return([]domain.Product{product}, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/orders/products?q=kopi&include_inactive=true&limit=5", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	body := resp.Body.String()
	require.Contains(t, body, `"product_id":"`+product.ID.String()+`"`)
	require.Contains(t, body, `"sku":"KOPI-1"`)
	require.Contains(t, body, `"name":"Kopi Arabika"`)
	require.Contains(t, body, `"price_idr":15000`)
	require.NotContains(t, body, "15000000", "the storage unit does not cross the wire")
	require.Contains(t, body, `"unit":"kg"`)
	require.Contains(t, body, `"active":true`)
	require.Contains(t, body, `"count":1`)
}

// TestANewProductArrivesInRupiahAndIsStoredInMicros: the screen types fifteen thousand and the store
// keeps fifteen million, and the conversion happens once — on the way in — so an order's price and
// the catalogue's price are the same number.
func TestANewProductArrivesInRupiahAndIsStoredInMicros(t *testing.T) {
	svc := mocks.NewService(t)
	workspaceID := uuid.New()
	app := newTestApp(t, svc, workspaceID)
	product := productFixture()

	var received domain.CreateProductRequest
	svc.EXPECT().CreateProduct(mock.Anything, asWorkspace(workspaceID), mock.MatchedBy(func(request domain.CreateProductRequest) bool {
		received = request
		return true
	})).Return(product, nil).Once()

	resp := do(t, app, fiber.MethodPost, "/api/orders/products",
		`{"sku":"KOPI-1","name":"Kopi Arabika","price_idr":15000,"unit":"kg"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	// The screen sent Rp 15.000 and the store keeps micro-rupiah, which is the same value the
	// catalogue renders back as 15000 — the conversion is a scale, not a different price.
	require.Equal(t, int64(15_000)*microsPerRupiah, received.UnitPriceMicros)
	require.Equal(t, "Kopi Arabika", received.Name)
	require.Contains(t, resp.Body.String(), `"product_id":"`+product.ID.String()+`"`)
	require.Contains(t, resp.Body.String(), `"price_idr":15000`)
}

// TestADuplicateNameIsAConflict: a name is how a Bolu finds a product, so two of them is a decision a
// person has to make rather than a bad request to retype.
func TestADuplicateNameIsAConflict(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	svc.EXPECT().CreateProduct(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Product{}, domain.ErrDuplicateProduct).Once()

	resp := do(t, app, fiber.MethodPost, "/api/orders/products", `{"name":"Kopi Arabika","price_idr":15000}`)

	require.Equal(t, fiber.StatusConflict, resp.Code)
	require.Contains(t, resp.Body.String(), "duplicate_product")
}

// TestAnEditTouchesOnlyTheFieldsItNames: the request struct is all pointers for exactly this reason —
// an edit that sends a price must not blank the sku it did not mention.
func TestAnEditTouchesOnlyTheFieldsItNames(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	product := productFixture()

	var received domain.UpdateProductRequest
	svc.EXPECT().UpdateProduct(mock.Anything, mock.Anything, product.ID, mock.MatchedBy(func(request domain.UpdateProductRequest) bool {
		received = request
		return true
	})).Return(product, nil).Once()

	resp := do(t, app, fiber.MethodPut, "/api/orders/products/"+product.ID.String(), `{"price_idr":20000}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.NotNil(t, received.UnitPriceMicros)
	require.Equal(t, int64(20_000)*microsPerRupiah, *received.UnitPriceMicros)
	require.Nil(t, received.SKU)
	require.Nil(t, received.Name)
	require.Nil(t, received.Unit)
	require.Nil(t, received.Active)

	// A body naming only the unit leaves the price alone rather than sending a nil price as zero,
	// which would make the coffee free.
	var namedOnlyUnit domain.UpdateProductRequest
	svc.EXPECT().UpdateProduct(mock.Anything, mock.Anything, product.ID, mock.MatchedBy(func(request domain.UpdateProductRequest) bool {
		namedOnlyUnit = request
		return true
	})).Return(product, nil).Once()

	resp = do(t, app, fiber.MethodPut, "/api/orders/products/"+product.ID.String(), `{"unit":"karung"}`)
	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Nil(t, namedOnlyUnit.UnitPriceMicros)
	require.NotNil(t, namedOnlyUnit.Unit)
	require.Equal(t, "karung", *namedOnlyUnit.Unit)

	// An edit to something another person deleted is a 404, not a silent success.
	svc.EXPECT().UpdateProduct(mock.Anything, mock.Anything, product.ID, mock.Anything).
		Return(domain.Product{}, domain.ErrNotFound).Once()

	resp = do(t, app, fiber.MethodPut, "/api/orders/products/"+product.ID.String(), `{"price_idr":20000}`)
	require.Equal(t, fiber.StatusNotFound, resp.Code)
	require.Contains(t, resp.Body.String(), "not_found")

	// An id that is not a uuid is the caller's typo, and the service is never asked about it.
	resp = do(t, app, fiber.MethodPut, "/api/orders/products/bukan-uuid", `{"price_idr":20000}`)
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_input")
}

// TestDeletingAProductAnswersWhetherItWasThere: deleting what somebody else already deleted is not an
// error to a person who wanted it gone, but it is worth saying which of the two happened.
func TestDeletingAProductAnswersWhetherItWasThere(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	product := productFixture()

	svc.EXPECT().DeleteProduct(mock.Anything, mock.Anything, product.ID).Return(nil).Once()

	resp := do(t, app, fiber.MethodDelete, "/api/orders/products/"+product.ID.String(), "")
	require.Equal(t, fiber.StatusOK, resp.Code)

	svc.EXPECT().DeleteProduct(mock.Anything, mock.Anything, product.ID).
		Return(domain.ErrNotFound).Once()

	resp = do(t, app, fiber.MethodDelete, "/api/orders/products/"+product.ID.String(), "")
	require.Equal(t, fiber.StatusNotFound, resp.Code)
	require.Contains(t, resp.Body.String(), "not_found")

	resp = do(t, app, fiber.MethodDelete, "/api/orders/products/bukan-uuid", "")
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_input")
}

// TestTheOrderListIsFilteredByCustomer: the customer's number is how a screen answers "what did this
// person order", and a limit is what keeps the answer one page.
func TestTheOrderListIsFilteredByCustomer(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	// No agent and no task: an order a person typed has neither, and a screen that read a zero uuid
	// would render a link that opens nothing.
	order := orderFixture(uuid.Nil, uuid.Nil)

	svc.EXPECT().Orders(mock.Anything, mock.Anything, domain.OrderFilter{CustomerPhone: "0812345678", Limit: 3}).
		Return([]domain.Order{order}, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/orders?customer_phone=0812345678&limit=3", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	body := resp.Body.String()
	require.Contains(t, body, `"order_id":"`+order.ID.String()+`"`)
	require.Contains(t, body, `"code":"ORD-2026-0001"`)
	require.Contains(t, body, `"status":"recorded"`)
	require.Contains(t, body, `"customer_name":"Siti"`)
	require.Contains(t, body, `"customer_phone":"0812345678"`)
	require.Contains(t, body, `"total_idr":30`)
	require.Contains(t, body, `"count":1`)
	require.NotContains(t, body, `"agent_id"`)
	require.NotContains(t, body, `"task_id"`)
}

// TestAnOrderIsRenderedWithItsLines: the invoice a person approves is built from this answer, so the
// line's own description, unit and price have to survive the trip — and the ids are strings, because
// a client that read a typed uuid would read sixteen numbers as one.
func TestAnOrderIsRenderedWithItsLines(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	agentID, taskID := uuid.New(), uuid.New()
	order := orderFixture(agentID, taskID)

	svc.EXPECT().Order(mock.Anything, mock.Anything, order.ID).Return(order, nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/orders/"+order.ID.String(), "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	body := resp.Body.String()
	require.Contains(t, body, `"order_id":"`+order.ID.String()+`"`)
	require.Contains(t, body, `"customer_phone":"0812345678"`)
	require.Contains(t, body, `"total_idr":30`)
	require.Contains(t, body, `"agent_id":"`+agentID.String()+`"`)
	require.Contains(t, body, `"task_id":"`+taskID.String()+`"`)
	require.Contains(t, body, `"lines":[{"product_id":"`+order.Lines[0].ProductID.String()+`"`)
	require.Contains(t, body, `"description":"Kopi Arabika"`)
	require.Contains(t, body, `"quantity":2`)
	require.Contains(t, body, `"unit_price_idr":15000`)
	require.Contains(t, body, `"line_total_idr":30000`)

	// An id that is not a uuid never reaches the service.
	resp = do(t, app, fiber.MethodGet, "/api/orders/bukan-uuid", "")
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_input")

	// An order that is not in this workspace is a 404 rather than an empty order.
	missing := uuid.New()
	svc.EXPECT().Order(mock.Anything, mock.Anything, missing).Return(domain.Order{}, domain.ErrNotFound).Once()

	resp = do(t, app, fiber.MethodGet, "/api/orders/"+missing.String(), "")
	require.Equal(t, fiber.StatusNotFound, resp.Code)
	require.Contains(t, resp.Body.String(), "not_found")
}

// TestCancellingAnOrderAnswersTheCanceledOrder: a person who decided not to fulfil an order should
// see the order in the state their decision left it, not have to ask again.
func TestCancellingAnOrderAnswersTheCanceledOrder(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	order := orderFixture(uuid.Nil, uuid.Nil)
	order.Status = domain.StatusCanceled

	svc.EXPECT().Cancel(mock.Anything, mock.Anything, order.ID).Return(order, nil).Once()

	resp := do(t, app, fiber.MethodPost, "/api/orders/"+order.ID.String()+"/cancel", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"status":"canceled"`)

	// A cancellation that named something that is not an order is refused before the service is asked.
	resp = do(t, app, fiber.MethodPost, "/api/orders/bukan-uuid/cancel", "")
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_input")

	// One that is not there answers 404 rather than pretending an order was cancelled.
	missing := uuid.New()
	svc.EXPECT().Cancel(mock.Anything, mock.Anything, missing).Return(domain.Order{}, domain.ErrNotFound).Once()

	resp = do(t, app, fiber.MethodPost, "/api/orders/"+missing.String()+"/cancel", "")
	require.Equal(t, fiber.StatusNotFound, resp.Code)
	require.Contains(t, resp.Body.String(), "not_found")
}

// TestAnUnexpectedFailureIsLoggedAndNotRepeated: the store's own words describe the deployment, and a
// client that read them would be reading somebody else's problem.
func TestAnUnexpectedFailureIsLoggedAndNotRepeated(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	svc.EXPECT().Products(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("relation products does not exist")).Once()

	resp := do(t, app, fiber.MethodGet, "/api/orders/products", "")

	require.Equal(t, fiber.StatusInternalServerError, resp.Code)
	require.Contains(t, resp.Body.String(), "internal_error")
	require.NotContains(t, resp.Body.String(), "relation products", "what the store said does not cross the wire")
}

// TestABodyThatIsNotJSONIsTheCallersProblem: a truncated payload is a typo or a broken client, and
// saying which of the two — rather than 500 — is what lets somebody fix it.
func TestABodyThatIsNotJSONIsTheCallersProblem(t *testing.T) {
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())

	resp := do(t, app, fiber.MethodPost, "/api/orders/products", "{")
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_request")

	resp = do(t, app, fiber.MethodPut, "/api/orders/products/"+uuid.NewString(), "{")
	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_request")
}

// TestARefusalIsAStatusAndACode: a screen branches on the status and shows the code. A 404 and a 400
// are both "no", but they send a person to different places.
func TestARefusalIsAStatusAndACode(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"a filter that cannot be honoured", domain.ErrInvalidInput, fiber.StatusBadRequest, "invalid_input"},
		{
			"a name the catalogue does not have",
			&domain.UnknownProductError{Name: "Kopi Organik", Candidates: []domain.Product{productFixture()}},
			fiber.StatusBadRequest,
			"invalid_input",
		},
		{"nothing there", domain.ErrNotFound, fiber.StatusNotFound, "not_found"},
		{"a name already in use", domain.ErrDuplicateProduct, fiber.StatusConflict, "duplicate_product"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := mocks.NewService(t)
			app := newTestApp(t, svc, uuid.New())

			svc.EXPECT().Orders(mock.Anything, mock.Anything, mock.Anything).Return(nil, tc.err).Once()

			resp := do(t, app, fiber.MethodGet, "/api/orders", "")

			require.Equal(t, tc.status, resp.Code)
			require.Contains(t, resp.Body.String(), tc.code)
		})
	}

	// A refusal a person has to act on has to say what it is about: an unknown name answers with the
	// candidates, because that is what the person asks the customer about.
	svc := mocks.NewService(t)
	app := newTestApp(t, svc, uuid.New())
	svc.EXPECT().Orders(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, &domain.UnknownProductError{Name: "Kopi Organik", Candidates: []domain.Product{productFixture()}}).Once()

	resp := do(t, app, fiber.MethodGet, "/api/orders", "")
	require.Contains(t, resp.Body.String(), "Kopi Arabika")
}

// TestTheServiceRunsAsTheRequestsWorkspace: the tenant comes from the request, never from the body or
// the query — one read and one write, because a write that ran as the wrong workspace would put a
// product in somebody else's catalogue.
func TestTheServiceRunsAsTheRequestsWorkspace(t *testing.T) {
	svc := mocks.NewService(t)
	workspaceID := uuid.New()
	app := newTestApp(t, svc, workspaceID)

	var read, write domain.Scope
	svc.EXPECT().Products(mock.Anything, mock.MatchedBy(func(scope domain.Scope) bool {
		read = scope
		return true
	}), mock.Anything).Return(nil, nil).Once()
	svc.EXPECT().CreateProduct(mock.Anything, mock.MatchedBy(func(scope domain.Scope) bool {
		write = scope
		return true
	}), mock.Anything).Return(productFixture(), nil).Once()

	resp := do(t, app, fiber.MethodGet, "/api/orders/products?workspace_id="+uuid.NewString(), "")
	require.Equal(t, fiber.StatusOK, resp.Code)

	resp = do(t, app, fiber.MethodPost, "/api/orders/products",
		`{"name":"Es Jeruk","price_idr":8000,"workspace_id":"`+uuid.NewString()+`"}`)
	require.Equal(t, fiber.StatusCreated, resp.Code)

	require.Equal(t, workspaceID, read.WorkspaceID)
	require.Equal(t, workspaceID, write.WorkspaceID)
}
