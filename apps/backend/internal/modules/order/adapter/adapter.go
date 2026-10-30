// Package adapter exposes the workspace's own data — its product catalogue and the orders it takes —
// as tools the hub can run.
//
// It is an adapter in the hub's sense, and it is the first one that is *not* a connected account: a
// catalogue is the workspace's, there is no provider, no token, and nothing outside this deployment to
// reach. `NeedsConnection` is what says so, and it is what the tool filter reads to offer these tools
// to a Bolu whose profile asks for them without demanding a grant.
//
// The adapter is deliberately thin. Everything that decides what an order says lives in the service:
// this file turns a tool call's JSON into a service call and the answer back into JSON, and it does no
// arithmetic, no resolution, and no validation of its own — a second place that decided what a price
// is would be a second answer to the same question.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/order/domain"
)

// App is the application key this adapter answers for.
const App = "order"

// Tool names.
const (
	// ToolFindProduct answers what the catalogue charges for something. It is `read`: it looks, and
	// nothing outside the workspace changes.
	ToolFindProduct = "order.find_product"
	// ToolRecordOrder writes the order into the workspace's own data. It is `write_internal`, which is
	// the label that means "this changes the workspace and nobody is asked": an order a customer
	// placed is a record of something that already happened, not an action that leaves the building.
	ToolRecordOrder = "order.record_order"
	// ToolReadOrders answers what has been ordered, so a follow-up question about a previous order is
	// answerable without asking the customer to repeat themselves.
	ToolReadOrders = "order.read_orders"
)

// Service is the part of the module this adapter uses.
//
// It is declared here rather than taken as the whole service, so the adapter can say exactly what it
// needs: look something up, record an order, read orders back. Anything wider would let a tool reach
// parts of the module that are not its business.
type Service interface {
	Lookup(ctx context.Context, scope domain.Scope, term string) (domain.Lookup, error)
	Record(ctx context.Context, scope domain.Scope, order domain.NewOrder) (domain.Order, error)
	Orders(ctx context.Context, scope domain.Scope, filter domain.OrderFilter) ([]domain.Order, error)
}

// Adapter is the workspace-data integration.
type Adapter struct {
	orders Service
}

// New builds the adapter.
func New(orders Service) *Adapter { return &Adapter{orders: orders} }

// App implements integrationdomain.Adapter.
func (a *Adapter) App() string { return App }

// NeedsToken implements integrationdomain.Credentialled: there is no token, because there is no
// account. The workspace is the account.
func (a *Adapter) NeedsToken() bool { return false }

// NeedsConnection implements integrationdomain.Connectionless.
//
// False is the whole point of this adapter: a catalogue and an order are the workspace's own data, so
// a call needs no connected account and no grant. The tool filter reads the same fact from
// `tool_catalog.needs_connection`, and a test asserts the two agree — a disagreement would either
// offer a tool nothing can run or hide one that needs nothing.
func (a *Adapter) NeedsConnection() bool { return false }

// Triggers implements integrationdomain.Adapter. A catalogue announces nothing.
func (a *Adapter) Triggers() []integrationdomain.TriggerSpec { return nil }

// Tools implements integrationdomain.Adapter.
func (a *Adapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{
		{
			Name:  ToolFindProduct,
			Label: integrationdomain.LabelRead,
			Description: "Cari harga satu produk di katalog workspace. Pakai ini sebelum menyebut " +
				"harga ke pelanggan; kalau produknya tidak ada, tanyakan ke pelanggan jangan mengarang harga.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"product": {"type": "string", "description": "Nama produk seperti yang disebut pelanggan"}
				},
				"required": ["product"]
			}`),
		},
		{
			Name:  ToolRecordOrder,
			Label: integrationdomain.LabelWriteInternal,
			Description: "Catat pesanan pelanggan ke data workspace: produk, jumlah, dan kontak. " +
				"Harganya diambil dari katalog, jadi cukup sebut nama produknya. " +
				"Panggil ini setelah semua produk dan jumlahnya jelas.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"customer_name": {"type": "string", "description": "Nama pelanggan, kalau disebutkan"},
					"customer_phone": {"type": "string", "description": "Nomor WhatsApp pelanggan, apa adanya"},
					"note": {"type": "string", "description": "Catatan pesanan, misalnya alamat kirim"},
					"items": {
						"type": "array",
						"minItems": 1,
						"description": "Satu entri per produk yang dipesan",
						"items": {
							"type": "object",
							"properties": {
								"product": {"type": "string", "description": "Nama produk di katalog"},
								"quantity": {"type": "number", "description": "Jumlah, boleh pecahan untuk barang timbang"},
								"note": {"type": "string", "description": "Catatan untuk baris ini"}
							},
							"required": ["product", "quantity"]
						}
					}
				},
				"required": ["items"]
			}`),
		},
		{
			Name:  ToolReadOrders,
			Label: integrationdomain.LabelRead,
			Description: "Baca pesanan terakhir workspace ini, atau pesanan satu pelanggan. " +
				"Pakai untuk menjawab pertanyaan tentang pesanan sebelumnya.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"customer_phone": {"type": "string", "description": "Nomor pelanggan; kosongkan untuk pesanan terakhir siapa pun"},
					"limit": {"type": "integer", "minimum": 1, "maximum": 50}
				}
			}`),
		},
	}
}

// Execute implements integrationdomain.Adapter.
func (a *Adapter) Execute(ctx context.Context, call integrationdomain.Call) (integrationdomain.Result, error) {
	if a.orders == nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: the order module is not configured", domain.ErrInvalidInput)
	}

	scope := domain.Scope{UserID: call.Scope.UserID, WorkspaceID: call.Scope.WorkspaceID}

	switch call.Tool {
	case ToolFindProduct:
		return a.findProduct(ctx, scope, call)
	case ToolRecordOrder:
		return a.recordOrder(ctx, scope, call)
	case ToolReadOrders:
		return a.readOrders(ctx, scope, call)
	default:
		return integrationdomain.Result{}, fmt.Errorf("%w: %s", integrationdomain.ErrUnknownTool, call.Tool)
	}
}

// findArguments is what the lookup tool accepts.
type findArguments struct {
	Product string `json:"product"`
}

// findProduct answers what the catalogue charges, or what it does have that is close.
//
// **A product that is not in the catalogue is a successful answer, not an error.** The tool found out
// something true — the workspace does not sell that — and a model that receives it as data can ask the
// customer what they meant; a model that receives it as a failure tends to retry with a name it
// invents. The JSON says which of the three cases it is.
func (a *Adapter) findProduct(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	var args findArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	lookup, err := a.orders.Lookup(ctx, scope, args.Product)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	payload := map[string]any{
		"ditemukan":   lookup.Found != nil,
		"kandidat":    productList(lookup.Candidates),
		"keterangan":  explain(lookup),
		"satuan_uang": "rupiah",
	}

	summary := fmt.Sprintf("Katalog: %q tidak ada", strings.TrimSpace(args.Product))
	if lookup.Found != nil {
		payload["produk"] = productPayload(*lookup.Found)
		summary = fmt.Sprintf("Harga %s: %s", lookup.Found.Name, domain.Rupiah(lookup.Found.PriceIDR()))
	}

	content, err := json.Marshal(payload)
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("order: render result: %w", err)
	}

	return integrationdomain.Result{Content: content, Summary: summary}, nil
}

// explain says which of the three answers this is, in a sentence a model can act on.
func explain(lookup domain.Lookup) string {
	switch {
	case lookup.Found != nil:
		return "Produk ini ada di katalog. Harga di atas per satuan; jangan mengubahnya."
	case len(lookup.Candidates) > 0:
		return "Produk dengan nama itu tidak ada. Ada kandidat di daftar kandidat: tanyakan ke pelanggan maksudnya yang mana, jangan menebak harga."
	default:
		return "Produk itu tidak ada di katalog workspace, dan tidak ada kandidat yang mirip. Tanyakan ke pelanggan atau ke pemilik usaha; jangan mengarang harga."
	}
}

// recordArguments is what the recording tool accepts.
type recordArguments struct {
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	Note          string `json:"note"`
	Items         []struct {
		Product  string  `json:"product"`
		Quantity float64 `json:"quantity"`
		Note     string  `json:"note"`
	} `json:"items"`
}

// recordOrder writes the order and answers with what was recorded.
//
// The answer carries the order's code and its total, which is what the Bolu composes the invoice from.
// It does not carry the price per line as something the model may change: the model reads them, and the
// numbers it quotes come from here rather than from its own arithmetic.
func (a *Adapter) recordOrder(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	var args recordArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	items := make([]domain.NewItem, 0, len(args.Items))
	for _, item := range args.Items {
		items = append(items, domain.NewItem{
			Product:  item.Product,
			Quantity: item.Quantity,
			Note:     item.Note,
		})
	}

	order, err := a.orders.Record(ctx, scope, domain.NewOrder{
		CustomerPhone: args.CustomerPhone,
		CustomerName:  args.CustomerName,
		Note:          args.Note,
		AgentID:       call.Scope.AgentID,
		TaskID:        call.Scope.TaskID,
		Items:         items,
	})
	if err != nil {
		// A refusal is returned as an error rather than as a result, and that is the runtime's own
		// convention: the workflow hands a failed tool call back to the model as something it can read
		// and react to. The text is written for that reader — it names what was asked for, what the
		// catalogue has instead, and what to do next — because the message is the whole of what the
		// model gets.
		//
		// Nothing was written: the service refuses before the first insert, so a partial order cannot
		// exist and a second attempt is a fresh one.
		return integrationdomain.Result{}, err
	}

	payload := map[string]any{
		"order_id":     order.ID.String(),
		"kode":         order.Code,
		"status":       order.Status,
		"total_rupiah": order.TotalIDR(),
		"pelanggan": map[string]any{
			"nama":  order.Customer.Name,
			"nomor": order.Customer.Phone,
		},
		"baris": lineList(order),
	}

	content, err := json.Marshal(payload)
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("order: render result: %w", err)
	}

	return integrationdomain.Result{
		Content: content,
		Summary: fmt.Sprintf("Pesanan %s dicatat: %d baris, %s", order.Code, len(order.Lines), domain.Rupiah(order.TotalIDR())),
	}, nil
}

// readArguments is what the read tool accepts.
type readArguments struct {
	CustomerPhone string `json:"customer_phone"`
	Limit         int    `json:"limit"`
}

// readOrders answers what has been ordered.
func (a *Adapter) readOrders(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	var args readArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	orders, err := a.orders.Orders(ctx, scope, domain.OrderFilter{
		CustomerPhone: args.CustomerPhone,
		Limit:         args.Limit,
	})
	if err != nil {
		return integrationdomain.Result{}, err
	}

	listed := make([]map[string]any, 0, len(orders))
	for _, order := range orders {
		listed = append(listed, map[string]any{
			"kode":         order.Code,
			"status":       order.Status,
			"total_rupiah": order.TotalIDR(),
			"dibuat":       order.CreatedAt.Format("2006-01-02"),
			"pelanggan":    order.Customer.Name,
			"baris":        lineList(order),
		})
	}

	content, err := json.Marshal(map[string]any{"pesanan": listed, "jumlah": len(listed)})
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("order: render result: %w", err)
	}

	return integrationdomain.Result{
		Content: content,
		Summary: fmt.Sprintf("%d pesanan terbaca", len(listed)),
	}, nil
}

// ---------------------------------------------------------------- rendering

// productPayload renders one product for the model, in rupiah and in the unit it is sold by.
func productPayload(product domain.Product) map[string]any {
	return map[string]any{
		"nama":         product.Name,
		"sku":          product.SKU,
		"satuan":       product.Unit,
		"harga_rupiah": product.PriceIDR(),
	}
}

// productList renders candidates.
func productList(products []domain.Product) []map[string]any {
	out := make([]map[string]any, 0, len(products))
	for _, product := range products {
		out = append(out, productPayload(product))
	}

	return out
}

// lineList renders an order's lines.
func lineList(order domain.Order) []map[string]any {
	out := make([]map[string]any, 0, len(order.Lines))
	for _, line := range order.Lines {
		out = append(out, map[string]any{
			"produk":              line.Description,
			"jumlah":              line.Quantity,
			"satuan":              line.Unit,
			"harga_satuan_rupiah": line.PriceIDR(),
			"total_rupiah":        line.TotalIDR(),
		})
	}

	return out
}
