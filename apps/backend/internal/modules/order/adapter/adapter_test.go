package adapter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/adapter"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain"
)

// The adapter is deliberately thin — it decides nothing about what an order says — so what this file
// tests is its contract with the runtime: which tools it declares, what it does with a call's JSON,
// and the exact shape the model reads back. The port is a hand-written fake rather than the mock so
// that the fake can be a plain function of its inputs; the point is the JSON, not the wiring.

// fakeOrders is the adapter's port, scripted per test and recording what it was asked.
type fakeOrders struct {
	lookupResult domain.Lookup
	lookupErr    error
	lookupScope  domain.Scope
	lookupTerm   string

	recordResult domain.Order
	recordErr    error
	recordScope  domain.Scope
	recorded     domain.NewOrder

	ordersResult []domain.Order
	ordersErr    error
	ordersScope  domain.Scope
	ordersFilter domain.OrderFilter
}

func (f *fakeOrders) Lookup(_ context.Context, scope domain.Scope, term string) (domain.Lookup, error) {
	f.lookupScope, f.lookupTerm = scope, term

	return f.lookupResult, f.lookupErr
}

func (f *fakeOrders) Record(_ context.Context, scope domain.Scope, order domain.NewOrder) (domain.Order, error) {
	f.recordScope, f.recorded = scope, order

	return f.recordResult, f.recordErr
}

func (f *fakeOrders) Orders(_ context.Context, scope domain.Scope, filter domain.OrderFilter) ([]domain.Order, error) {
	f.ordersScope, f.ordersFilter = scope, filter

	return f.ordersResult, f.ordersErr
}

// callScope is a scope with the attribution an order has to carry.
func callScope() integrationdomain.Scope {
	return integrationdomain.Scope{
		UserID:      uuid.New(),
		WorkspaceID: uuid.New(),
		AgentID:     uuid.New(),
		TaskID:      uuid.New(),
	}
}

// callArguments renders the tool arguments a model would send.
func callArguments(t *testing.T, arguments any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(arguments)
	require.NoError(t, err)

	return encoded
}

// execute runs one call against a fake.
func execute(t *testing.T, orders *fakeOrders, scope integrationdomain.Scope, tool string, arguments json.RawMessage) (integrationdomain.Result, error) {
	t.Helper()

	return adapter.New(orders).Execute(t.Context(), integrationdomain.Call{
		Scope:     scope,
		Tool:      tool,
		Arguments: arguments,
	})
}

// TestTheAdapterIsTheOrderApplication: the adapter is not a connected account, so it declares that a
// call needs neither a token nor a connection — which is what lets the tool filter offer these tools
// without a grant.
func TestTheAdapterIsTheOrderApplication(t *testing.T) {
	a := adapter.New(&fakeOrders{})

	require.Equal(t, adapter.App, a.App())
	require.False(t, a.NeedsConnection())
	require.False(t, a.NeedsToken())
	require.Nil(t, a.Triggers())

	var connectionless integrationdomain.Connectionless = a
	require.False(t, connectionless.NeedsConnection())

	var credentialled integrationdomain.Credentialled = a
	require.False(t, credentialled.NeedsToken())
}

// TestTheToolsAreDeclaredWithTheirLabels: the label is the only thing that decides whether a person is
// asked before a call goes out, so recording an order has to be `write_internal` (it changes the
// workspace and nobody is asked) while reading a price or an order stays `read`.
func TestTheToolsAreDeclaredWithTheirLabels(t *testing.T) {
	specs := adapter.New(&fakeOrders{}).Tools()
	require.Len(t, specs, 3)

	byName := map[string]integrationdomain.ToolSpec{}
	for _, spec := range specs {
		require.NoError(t, spec.Valid())
		require.Equal(t, adapter.App, spec.App())

		var schema map[string]any
		require.NoError(t, json.Unmarshal(spec.Schema, &schema))
		require.Equal(t, "object", schema["type"])

		byName[spec.Name] = spec
	}

	require.Equal(t, integrationdomain.LabelRead, byName[adapter.ToolFindProduct].Label)
	require.Equal(t, integrationdomain.LabelWriteInternal, byName[adapter.ToolRecordOrder].Label)
	require.Equal(t, integrationdomain.LabelRead, byName[adapter.ToolReadOrders].Label)
}

// TestAnUnknownToolIsRefused: a name the adapter does not answer to is a runtime mistake, not a
// workspace question, so it is reported as such rather than answered as an empty result.
func TestAnUnknownToolIsRefused(t *testing.T) {
	_, err := execute(t, &fakeOrders{}, callScope(), "order.does_not_exist", json.RawMessage(`{}`))
	require.ErrorIs(t, err, integrationdomain.ErrUnknownTool)
}

// TestArgumentsThatAreNotJSONAreASchemaViolation: every tool parses before it does anything, so
// malformed arguments cost nothing and are reported before the port is reached.
func TestArgumentsThatAreNotJSONAreASchemaViolation(t *testing.T) {
	for _, tool := range []string{adapter.ToolFindProduct, adapter.ToolRecordOrder, adapter.ToolReadOrders} {
		t.Run(tool, func(t *testing.T) {
			_, err := execute(t, &fakeOrders{}, callScope(), tool, json.RawMessage(`not json`))
			require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
		})
	}
}

// TestFindingAProductAnswersWithItsPrice: the price the model is told is the catalogue's, in rupiah,
// which is the number it quotes — so the payload carries the name, the unit, and the rupiah price.
func TestFindingAProductAnswersWithItsPrice(t *testing.T) {
	scope := callScope()
	fake := &fakeOrders{lookupResult: domain.Lookup{
		Found: &domain.Product{Name: "Kopi Arabika", SKU: "KOPI-ARB", Unit: "kg", UnitPriceMicros: 15_000_000},
	}}

	result, err := execute(t, fake, scope, adapter.ToolFindProduct, callArguments(t, map[string]any{"product": "Kopi Arabika"}))
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(result.Content, &payload))
	require.Equal(t, true, payload["ditemukan"])

	product, ok := payload["produk"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Kopi Arabika", product["nama"])
	require.Equal(t, "kg", product["satuan"])
	require.Equal(t, 15.0, product["harga_rupiah"])

	require.Equal(t, scope.UserID, fake.lookupScope.UserID)
	require.Equal(t, scope.WorkspaceID, fake.lookupScope.WorkspaceID)
	require.Equal(t, "Kopi Arabika", fake.lookupTerm)
}

// TestAMissingProductIsAnAnswerNotAnError: the tool found out something true — the workspace does not
// sell that — so it comes back as data. The `keterangan` is the part that keeps a model from inventing
// a price when it has no candidate either.
func TestAMissingProductIsAnAnswerNotAnError(t *testing.T) {
	withCandidates := &fakeOrders{lookupResult: domain.Lookup{
		Candidates: []domain.Product{{Name: "Kopi Arabika", UnitPriceMicros: 15_000_000}},
	}}

	result, err := execute(t, withCandidates, callScope(), adapter.ToolFindProduct, callArguments(t, map[string]any{"product": "Es Jeruk"}))
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(result.Content, &payload))
	require.Equal(t, false, payload["ditemukan"])
	require.Len(t, payload["kandidat"].([]any), 1)
	require.Contains(t, payload["keterangan"].(string), "jangan")
}

// TestAMissingProductWithNoCandidatesSaysSo is the third answer: the catalogue has nothing close, so
// the only true thing to tell the model is that there is nothing to offer and it must not guess.
func TestAMissingProductWithNoCandidatesSaysSo(t *testing.T) {
	result, err := execute(t, &fakeOrders{}, callScope(), adapter.ToolFindProduct, callArguments(t, map[string]any{"product": "Es Jeruk"}))
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(result.Content, &payload))
	require.Equal(t, false, payload["ditemukan"])
	require.Empty(t, payload["kandidat"].([]any))
	require.Contains(t, payload["keterangan"].(string), "jangan")
}

// TestRecordingAnOrderCarriesTheAttributionAndTheLines: the order belongs to the Bolu and the task
// that took it, so both travel from the call's scope onto the record rather than being dropped by the
// adapter.
func TestRecordingAnOrderCarriesTheAttributionAndTheLines(t *testing.T) {
	scope := callScope()
	fake := &fakeOrders{recordResult: domain.Order{
		ID:          uuid.New(),
		Code:        "ORD-7",
		Status:      domain.StatusRecorded,
		TotalMicros: 45_000_000,
		Customer:    domain.Customer{Name: "Budi", Phone: "0812"},
		Lines: []domain.OrderLine{
			{Description: "Kopi Arabika", Unit: "kg", Quantity: 3, UnitPriceMicros: 15_000_000, LineTotalMicros: 45_000_000},
		},
	}}

	result, err := execute(t, fake, scope, adapter.ToolRecordOrder, callArguments(t, map[string]any{
		"customer_name":  "Budi",
		"customer_phone": "0812",
		"note":           "antar pagi",
		"items": []map[string]any{
			{"product": "Kopi Arabika", "quantity": 3, "note": "panas"},
		},
	}))
	require.NoError(t, err)

	require.Equal(t, scope.UserID, fake.recordScope.UserID)
	require.Equal(t, scope.WorkspaceID, fake.recordScope.WorkspaceID)
	require.Equal(t, domain.NewOrder{
		CustomerName:  "Budi",
		CustomerPhone: "0812",
		Note:          "antar pagi",
		AgentID:       scope.AgentID,
		TaskID:        scope.TaskID,
		Items:         []domain.NewItem{{Product: "Kopi Arabika", Quantity: 3, Note: "panas"}},
	}, fake.recorded)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(result.Content, &payload))
	require.Equal(t, "ORD-7", payload["kode"])
	require.Equal(t, 45.0, payload["total_rupiah"])
	require.Len(t, payload["baris"].([]any), 1)
}

// TestARefusedRecordingIsReturnedAsAnError: a refusal is what the model reads and reacts to, so it is
// the error rather than a rendered result — and nothing is rendered at all, because nothing was
// written.
func TestARefusedRecordingIsReturnedAsAnError(t *testing.T) {
	refusal := fmt.Errorf("%w: jumlah untuk %q harus lebih dari nol", domain.ErrInvalidInput, "Kopi Arabika")
	fake := &fakeOrders{recordErr: refusal}

	result, err := execute(t, fake, callScope(), adapter.ToolRecordOrder, callArguments(t, map[string]any{
		"items": []map[string]any{{"product": "Kopi Arabika", "quantity": 0}},
	}))

	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Empty(t, result.Content)
	require.Empty(t, result.Summary)
}

// TestReadingOrdersPassesTheFilterAndRendersTheList: the phone narrows the read to one customer and the
// limit bounds the page, and the answer says how many it found so a model can tell "none" from "all of
// them".
func TestReadingOrdersPassesTheFilterAndRendersTheList(t *testing.T) {
	scope := callScope()
	fake := &fakeOrders{ordersResult: []domain.Order{
		{
			Code:        "ORD-1",
			Status:      domain.StatusRecorded,
			TotalMicros: 15_000_000,
			Customer:    domain.Customer{Name: "Budi"},
			CreatedAt:   time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC),
			Lines:       []domain.OrderLine{{Description: "Kopi Arabika", Quantity: 1, UnitPriceMicros: 15_000_000, LineTotalMicros: 15_000_000}},
		},
	}}

	result, err := execute(t, fake, scope, adapter.ToolReadOrders, callArguments(t, map[string]any{
		"customer_phone": "0812",
		"limit":          5,
	}))
	require.NoError(t, err)

	require.Equal(t, domain.OrderFilter{CustomerPhone: "0812", Limit: 5}, fake.ordersFilter)
	require.Equal(t, scope.UserID, fake.ordersScope.UserID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(result.Content, &payload))
	require.Equal(t, 1.0, payload["jumlah"])
	require.Len(t, payload["pesanan"].([]any), 1)
}
