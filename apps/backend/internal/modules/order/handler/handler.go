// Package handler exposes the workspace's own data over HTTP: the product catalogue a workspace
// maintains, and the orders it took.
//
// The catalogue is a screen's business — a person types their prices, and every price a Bolu later
// quotes comes from here — so it is the only write surface. Orders are read-only over HTTP: an order is
// recorded by the Bolu from what a customer said, and a client that could insert one directly could
// claim an order that never happened. Cancelling is the one exception, because a person deciding not to
// fulfil an order is exactly the kind of decision the product puts in a human's hands.
package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the catalogue and the orders.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// Routes registers the endpoints every member of the workspace may read.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/orders/products", h.Products)
	group.Get("/orders", h.Orders)
	group.Get("/orders/:orderID", h.Order)
}

// ManagerRoutes registers the writes. The container applies the tenant middleware and a managing-role
// check.
func ManagerRoutes(group fiber.Router, h *Handler) {
	group.Post("/orders/products", h.CreateProduct)
	group.Put("/orders/products/:productID", h.UpdateProduct)
	group.Delete("/orders/products/:productID", h.DeleteProduct)
	group.Post("/orders/:orderID/cancel", h.Cancel)
}

// Products lists the catalogue.
func (h *Handler) Products(c *fiber.Ctx) error {
	products, err := h.service.Products(c.UserContext(), scope(c), domain.ProductFilter{
		Search:          c.Query("q"),
		IncludeInactive: c.QueryBool("include_inactive"),
		Limit:           c.QueryInt("limit", 0),
	})
	if err != nil {
		return h.fail(c, "list products", err)
	}

	payload := make([]productPayload, 0, len(products))
	for _, product := range products {
		payload = append(payload, toProductPayload(product))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// CreateProduct adds a catalogue entry.
func (h *Handler) CreateProduct(c *fiber.Ctx) error {
	var req productRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	product, err := h.service.CreateProduct(c.UserContext(), scope(c), domain.CreateProductRequest{
		SKU:  valueOf(req.SKU),
		Name: valueOf(req.Name),
		// The wire carries rupiah because a screen has rupiah and the API does not ask a client to know
		// the storage unit; the conversion happens here, once, with the same factor the usage report uses.
		UnitPriceMicros: rupiahToMicros(valueOf(req.PriceIDR)),
		Unit:            valueOf(req.Unit),
	})
	if err != nil {
		return h.fail(c, "create product", err)
	}

	return response.Created(c, "produk ditambahkan", toProductPayload(product))
}

// UpdateProduct edits a catalogue entry.
func (h *Handler) UpdateProduct(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("productID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "product id is not a uuid", "invalid_input", nil)
	}

	var req productRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	var price *int64
	if req.PriceIDR != nil {
		micros := rupiahToMicros(*req.PriceIDR)
		price = &micros
	}

	product, err := h.service.UpdateProduct(c.UserContext(), scope(c), id, domain.UpdateProductRequest{
		SKU:             req.SKU,
		Name:            req.Name,
		UnitPriceMicros: price,
		Unit:            req.Unit,
		Active:          req.Active,
	})
	if err != nil {
		return h.fail(c, "update product", err)
	}

	return response.OK(c, "produk diperbarui", toProductPayload(product))
}

// DeleteProduct removes a catalogue entry.
func (h *Handler) DeleteProduct(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("productID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "product id is not a uuid", "invalid_input", nil)
	}

	if err := h.service.DeleteProduct(c.UserContext(), scope(c), id); err != nil {
		return h.fail(c, "delete product", err)
	}

	return response.OK(c, "produk dihapus", nil)
}

// Orders lists the workspace's orders.
func (h *Handler) Orders(c *fiber.Ctx) error {
	orders, err := h.service.Orders(c.UserContext(), scope(c), domain.OrderFilter{
		CustomerPhone: c.Query("customer_phone"),
		Limit:         c.QueryInt("limit", 0),
	})
	if err != nil {
		return h.fail(c, "list orders", err)
	}

	payload := make([]orderPayload, 0, len(orders))
	for _, order := range orders {
		payload = append(payload, toOrderPayload(order))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Order returns one order with its lines.
func (h *Handler) Order(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("orderID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "order id is not a uuid", "invalid_input", nil)
	}

	order, err := h.service.Order(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get order", err)
	}

	return response.OK(c, "ok", toOrderPayload(order))
}

// Cancel marks an order a person decided not to fulfil.
func (h *Handler) Cancel(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("orderID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "order id is not a uuid", "invalid_input", nil)
	}

	order, err := h.service.Cancel(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "cancel order", err)
	}

	return response.OK(c, "pesanan dibatalkan", toOrderPayload(order))
}

// fail maps a service error onto a response.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return response.Error(c, fiber.StatusNotFound, "tidak ditemukan", "not_found", nil)
	case errors.Is(err, domain.ErrDuplicateProduct):
		return response.Error(c, fiber.StatusConflict,
			"produk dengan nama itu sudah ada di katalog", "duplicate_product", nil)
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrProductUnknown):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	default:
		h.log.Error("order request failed", zap.String("operation", operation), zap.Error(err))

		return response.Error(c, fiber.StatusInternalServerError, "permintaan gagal", "internal_error", nil)
	}
}

// scope is the tenant the request runs as.
//
// The workspace comes from `WorkspaceUUID`, which prefers the scope the tenant middleware resolved and
// falls back to the header — the same call every other module's handler makes. Reading the header
// alone would lose the tenant for a request that named it from the query string, which the tenant
// middleware accepts, and the service would refuse the whole call as unscoped.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// ---------------------------------------------------------------- the wire

// productRequest is a catalogue entry from a screen.
//
// Every field is a pointer on an edit and a value on a create, which is what lets one struct serve
// both: an absent field on an edit means "leave it alone", and on a create it means "empty".
type productRequest struct {
	SKU      *string  `json:"sku"`
	Name     *string  `json:"name"`
	PriceIDR *float64 `json:"price_idr"`
	Unit     *string  `json:"unit"`
	Active   *bool    `json:"active"`
}

// productPayload is a catalogue entry as a screen reads it.
type productPayload struct {
	ProductID string  `json:"product_id"`
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	PriceIDR  float64 `json:"price_idr"`
	Unit      string  `json:"unit"`
	Active    bool    `json:"active"`
	UpdatedAt string  `json:"updated_at"`
	CreatedAt string  `json:"created_at"`
}

// orderPayload is an order as a screen reads it.
type orderPayload struct {
	OrderID       string        `json:"order_id"`
	Code          string        `json:"code"`
	Status        string        `json:"status"`
	Note          string        `json:"note,omitempty"`
	CustomerName  string        `json:"customer_name"`
	CustomerPhone string        `json:"customer_phone"`
	AgentID       string        `json:"agent_id,omitempty"`
	TaskID        string        `json:"task_id,omitempty"`
	TotalIDR      float64       `json:"total_idr"`
	Lines         []linePayload `json:"lines"`
	CreatedAt     string        `json:"created_at"`
}

// linePayload is one order line.
type linePayload struct {
	ProductID   string  `json:"product_id,omitempty"`
	Description string  `json:"description"`
	Unit        string  `json:"unit"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price_idr"`
	LineTotal   float64 `json:"line_total_idr"`
}

// toProductPayload renders a product.
func toProductPayload(product domain.Product) productPayload {
	return productPayload{
		ProductID: product.ID.String(),
		SKU:       product.SKU,
		Name:      product.Name,
		PriceIDR:  product.PriceIDR(),
		Unit:      product.Unit,
		Active:    product.Active,
		CreatedAt: product.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: product.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// toOrderPayload renders an order with its lines.
func toOrderPayload(order domain.Order) orderPayload {
	payload := orderPayload{
		OrderID:       order.ID.String(),
		Code:          order.Code,
		Status:        order.Status,
		Note:          order.Note,
		CustomerName:  order.Customer.Name,
		CustomerPhone: order.Customer.Phone,
		TotalIDR:      order.TotalIDR(),
		CreatedAt:     order.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Lines:         make([]linePayload, 0, len(order.Lines)),
	}
	if order.AgentID != uuid.Nil {
		payload.AgentID = order.AgentID.String()
	}
	if order.TaskID != uuid.Nil {
		payload.TaskID = order.TaskID.String()
	}

	for _, line := range order.Lines {
		item := linePayload{
			Description: line.Description,
			Unit:        line.Unit,
			Quantity:    line.Quantity,
			UnitPrice:   float64(line.UnitPriceMicros) / 1_000_000,
			LineTotal:   line.TotalIDR(),
		}
		if line.ProductID != uuid.Nil {
			item.ProductID = line.ProductID.String()
		}

		payload.Lines = append(payload.Lines, item)
	}

	return payload
}

// rupiahToMicros converts the rupiah a screen sends into the micro-rupiah everything else stores.
func rupiahToMicros(rupiah float64) int64 { return int64(rupiah * 1_000_000) }

// valueOf reads an optional field, answering the zero value when it is absent. It is what lets one
// request struct serve a create (every field present) and an edit (only the changed ones).
func valueOf[T any](value *T) T {
	if value == nil {
		var zero T

		return zero
	}

	return *value
}
