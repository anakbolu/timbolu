package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The arithmetic a price is read through, which is the part a careless refactor changes silently: the
// stored unit is micro-rupiah, a person and a model read rupiah, and the conversion between them is
// what decides whether a quote is a thousand times off.

func TestMoneyIsReadInRupiahAndStoredInMicros(t *testing.T) {
	product := Product{UnitPriceMicros: 15_000_000_000}
	require.Equal(t, 15000.0, product.PriceIDR(), "Rp 15.000 is 15_000_000_000 micros")

	line := OrderLine{UnitPriceMicros: 15_000_000_000, LineTotalMicros: 30_000_000_000}
	require.Equal(t, 15000.0, line.PriceIDR())
	require.Equal(t, 30000.0, line.TotalIDR())

	order := Order{TotalMicros: 30_000_000_000}
	require.Equal(t, 30000.0, order.TotalIDR())

	// A fractional price keeps its sen rather than losing them to an integer division, which is what a
	// per-kilogram price below the rupiah usually looks like.
	require.Equal(t, 1234.56, OrderLine{UnitPriceMicros: 1_234_560_000}.PriceIDR())
}

func TestRupiahIsWrittenTheWayAPersonWritesIt(t *testing.T) {
	cases := []struct {
		amount float64
		want   string
		why    string
	}{
		{15000, "Rp 15000", "a whole amount has no decimals, and no trailing separator"},
		{0, "Rp 0", "a free product is a price, not an absence"},
		{30000, "Rp 30000", "the common case: an order total"},
		{1234.5, "Rp 1234,50", "sen are written when they are there"},
		{15000.004, "Rp 15000", "a rounding artifact below a sen is not written"},
		{999.999, "Rp 1000", "a fraction that rounds up carries into the whole amount"},
	}

	for _, testCase := range cases {
		require.Equal(t, testCase.want, Rupiah(testCase.amount), testCase.why)
	}
}

// TestTheUnknownProductErrorNamesWhatTheCatalogueHas is the message a model reads when it asks for
// something that is not sold. It is the whole of what the model gets, so it has to say what was asked
// for, what the catalogue does have, and what to do — the candidates are the part that turns a dead end
// into a question for the customer.
func TestTheUnknownProductErrorNamesWhatTheCatalogueHas(t *testing.T) {
	err := &UnknownProductError{
		Name: "Kopi Arabika Gayo 1 kg",
		Candidates: []Product{
			{Name: "Kopi Arabika", Unit: "kg", UnitPriceMicros: 15_000_000_000},
			{Name: "Kopi Susu", Unit: "pcs", UnitPriceMicros: 20_000_000_000},
		},
	}

	require.ErrorIs(t, err, ErrProductUnknown)

	message := err.Error()
	require.Contains(t, message, "Kopi Arabika Gayo 1 kg")
	require.Contains(t, message, "Kopi Arabika (Rp 15000)")
	require.Contains(t, message, "Kopi Susu (Rp 20000)")
	require.Contains(t, message, "jangan mengarang harga")

	// With nothing close in the catalogue the error says so, and still refuses rather than guessing.
	empty := &UnknownProductError{Name: "Kopi Luwak"}
	require.ErrorIs(t, empty, ErrProductUnknown)
	require.Contains(t, empty.Error(), "Kopi Luwak")
	require.NotContains(t, empty.Error(), "Kandidat")
}

func TestTheLimitsAreBounded(t *testing.T) {
	require.Equal(t, DefaultLimit, ProductFilter{}.Normalised().Limit)
	require.Equal(t, DefaultLimit, OrderFilter{}.Normalised().Limit)
	require.Equal(t, MaxLimit, ProductFilter{Limit: MaxLimit + 50}.Normalised().Limit)
	require.Equal(t, MaxLimit, OrderFilter{Limit: MaxLimit + 50}.Normalised().Limit)
	require.Equal(t, 7, ProductFilter{Limit: 7}.Normalised().Limit)
}
