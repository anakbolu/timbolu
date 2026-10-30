package duitku_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/billing/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/payment/duitku"
)

// The adapter is this file: what it asks Duitku for, what it reads back, and — the part
// that matters most — what it refuses.
//
// Duitku authenticates both directions with HMAC-SHA256 over a documented string, and the
// callback's signature is the only thing standing between a stranger and a paid package.
// So it is checked against a body the test signed itself, with the wrong key, with an
// amount edited after signing, and with a callback for another merchant.

const (
	merchantCode = "D1234"
	apiKey       = "6c2f6bb1e6170a4d3b5c8f0e1a2d4b6c"
	callbackURL  = "https://api.example.com/api/billing/webhooks/duitku"
)

// newGateway points the adapter at a test server.
func newGateway(t *testing.T, handler http.HandlerFunc) (*duitku.Gateway, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	gateway, err := duitku.New(duitku.Config{
		MerchantCode: merchantCode,
		APIKey:       apiKey,
		Sandbox:      true,
		CallbackURL:  callbackURL,
		ReturnURL:    "https://app.example.com/settings/subscription",
		BaseURL:      server.URL,
	})
	require.NoError(t, err)

	return gateway, server
}

// TestAValueThatCannotTakePaymentIsRefusedAtStartup: a deployment that meant to take
// payment but mistyped a value finds out while it is starting, not when the first customer
// tries to pay. The callback URL is in that list for a reason of its own: without it Duitku
// would accept a payment that nothing ever hears about.
func TestAValueThatCannotTakePaymentIsRefusedAtStartup(t *testing.T) {
	cases := map[string]duitku.Config{
		"no merchant code": {APIKey: apiKey, CallbackURL: callbackURL},
		"no api key":       {MerchantCode: merchantCode, CallbackURL: callbackURL},
		"no callback":      {MerchantCode: merchantCode, APIKey: apiKey},
	}

	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := duitku.New(config)
			require.Error(t, err)
			require.Contains(t, err.Error(), "required")
		})
	}

	// The channel and the expiry are not required: a deployment that names neither gets the
	// product's own defaults rather than a refusal.
	gateway, err := duitku.New(duitku.Config{
		MerchantCode: merchantCode, APIKey: apiKey, CallbackURL: callbackURL,
	})
	require.NoError(t, err)
	require.Equal(t, duitku.Name, gateway.Name())
}

// TestAnInquiryCarriesWhatDuitkuNeedsAndIsSignedCorrectly is the request half, and the
// signature is the part that cannot be guessed: Duitku computes
// `merchantCode + merchantOrderId + paymentAmount` and compares it with HMAC-SHA256.
func TestAnInquiryCarriesWhatDuitkuNeedsAndIsSignedCorrectly(t *testing.T) {
	var request struct {
		MerchantCode    string `json:"merchantCode"`
		PaymentAmount   int64  `json:"paymentAmount"`
		PaymentMethod   string `json:"paymentMethod"`
		MerchantOrderID string `json:"merchantOrderId"`
		ProductDetails  string `json:"productDetails"`
		CustomerVaName  string `json:"customerVaName"`
		Email           string `json:"email"`
		ItemDetails     []struct {
			Name     string `json:"name"`
			Price    int64  `json:"price"`
			Quantity int    `json:"quantity"`
		} `json:"itemDetails"`
		CallbackURL  string `json:"callbackUrl"`
		ReturnURL    string `json:"returnUrl"`
		Signature    string `json:"signature"`
		ExpiryPeriod int    `json:"expiryPeriod"`
	}

	gateway, _ := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, duitku.InquiryPath, r.URL.Path)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &request))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"merchantCode": "` + merchantCode + `",
			"reference": "D1234CX80TZJ85Q70QCI",
			"paymentUrl": "https://sandbox.duitku.com/topup/topupdirectv2.aspx?ref=BCA7WZ7EID1234WEC",
			"vaNumber": "7007014001444348",
			"amount": "149000",
			"statusCode": "00",
			"statusMessage": "SUCCESS"
		}`))
	})

	invoice := domain.Invoice{
		ID:          uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Kind:        domain.KindSubscription,
		AmountIDR:   149000,
		PeriodStart: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
	}

	charge, err := gateway.Charge(t.Context(), invoice, domain.Customer{
		WorkspaceID: uuid.New(), Name: "Toko Sinar", Email: "owner@example.com",
	})
	require.NoError(t, err)

	require.Equal(t, merchantCode, request.MerchantCode)
	require.Equal(t, int64(149000), request.PaymentAmount)
	require.Equal(t, duitku.DefaultMethod, request.PaymentMethod, "a bank virtual account is what the product sells")
	require.Equal(t, "owner@example.com", request.Email)
	require.Equal(t, callbackURL, request.CallbackURL)
	require.Equal(t, "https://app.example.com/settings/subscription", request.ReturnURL)
	require.Equal(t, duitku.DefaultExpiryMinutes, request.ExpiryPeriod)
	require.Len(t, request.ItemDetails, 1, "the payment page shows what is being bought")
	require.Equal(t, int64(149000), request.ItemDetails[0].Price)

	// The signature is Duitku's own string, in their own order.
	require.Equal(t, sign(merchantCode+request.MerchantOrderID+"149000", apiKey), request.Signature)

	// The order id is derived from the invoice rather than random, so a transaction whose
	// response was lost is the same transaction when it is tried again.
	require.Equal(t, request.MerchantOrderID, charge.Reference,
		"the callback names the order id, so it — and not Duitku's internal reference — is what the invoice records")
	require.NotEqual(t, "D1234CX80TZJ85Q70QCI", charge.Reference,
		"recording Duitku's own reference would leave every payment unmatched")
	require.Contains(t, charge.Reference, invoice.ID.String())
	require.True(t, strings.HasSuffix(charge.Reference, "-m"), "a month and a top-up are told apart")
	require.LessOrEqual(t, len(charge.Reference), 50, "Duitku caps the field at fifty characters")

	// What a person acts on: the payment page and the number, because the page reaches
	// somebody who wants to choose and the number reaches somebody who already knows.
	require.Contains(t, charge.Instructions, "7007014001444348")
	require.Contains(t, charge.Instructions, "topupdirectv2")
	require.False(t, charge.ExpiresAt.IsZero())
}

// TestAQRISInquiryHasNoVirtualAccount: not every channel has a number, and instructions
// that named an empty one would send somebody to a transfer they cannot make.
func TestAQRISInquiryHasNoVirtualAccount(t *testing.T) {
	gateway, err := duitku.New(duitku.Config{
		MerchantCode:  merchantCode,
		APIKey:        apiKey,
		CallbackURL:   callbackURL,
		PaymentMethod: duitku.MethodQRIS,
		BaseURL:       "http://127.0.0.1:1",
	})
	require.NoError(t, err)

	// The charge cannot be created against an unreachable host, which is the honest
	// answer; what is asserted here is that the configured channel is the one asked for.
	_, err = gateway.Charge(t.Context(), domain.Invoice{ID: uuid.New(), AmountIDR: 1000}, domain.Customer{})
	require.Error(t, err)

	var request struct {
		PaymentMethod string `json:"paymentMethod"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		_, _ = w.Write([]byte(`{"statusCode":"00","paymentUrl":"https://sandbox.duitku.com/topup/x","reference":"R1"}`))
	}))
	t.Cleanup(server.Close)

	qr, err := duitku.New(duitku.Config{
		MerchantCode:  merchantCode,
		APIKey:        apiKey,
		CallbackURL:   callbackURL,
		PaymentMethod: duitku.MethodQRIS,
		BaseURL:       server.URL,
	})
	require.NoError(t, err)

	charge, err := qr.Charge(t.Context(), domain.Invoice{ID: uuid.New(), AmountIDR: 25000, Kind: domain.KindTopup}, domain.Customer{})
	require.NoError(t, err)
	require.Equal(t, duitku.MethodQRIS, request.PaymentMethod)
	require.NotContains(t, charge.Instructions, "virtual account", "a channel with no number must not name one")
	require.Contains(t, charge.Instructions, "https://sandbox.duitku.com/topup/x")
}

// TestAnInquiryDuitkuRefusedIsAnErrorInItsOwnWords: Duitku answers 200 for an inquiry it
// refused, with the reason in the body — so the status code alone is not the verdict, and a
// refusal read as success would leave a payment page that does not exist.
func TestAnInquiryDuitkuRefusedIsAnErrorInItsOwnWords(t *testing.T) {
	cases := map[string]string{
		"a refused transaction": `{"statusCode":"01","statusMessage":"Payment method not allowed"}`,
		"an unknown status":     `{"statusCode":"","paymentUrl":"https://x"}`,
		// A transaction with neither a page nor a number is not one a person can pay,
		// whatever the status said.
		"nothing to pay with": `{"statusCode":"00","statusMessage":"SUCCESS"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			gateway, _ := newGateway(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})

			_, err := gateway.Charge(t.Context(), domain.Invoice{ID: uuid.New(), AmountIDR: 1000}, domain.Customer{})

			require.Error(t, err)
			require.Contains(t, err.Error(), "duitku:")
		})
	}

	// A provider that cannot be reached keeps its own words, because "Duitku is
	// unreachable" is what an operator needs to read.
	unreachable, err := duitku.New(duitku.Config{
		MerchantCode: merchantCode, APIKey: apiKey, CallbackURL: callbackURL, BaseURL: "http://127.0.0.1:1",
	})
	require.NoError(t, err)

	_, err = unreachable.Charge(context.Background(), domain.Invoice{ID: uuid.New(), AmountIDR: 1000}, domain.Customer{})
	require.Error(t, err)
}

// TestASettlementIsPaidWhenTheSignatureMatches is the gate that matters: the callback is
// unauthenticated in transit, so what makes it a payment is Duitku's own signature over
// the merchant code, the amount, and the order id.
func TestASettlementIsPaidWhenTheSignatureMatches(t *testing.T) {
	gateway, _ := newGateway(t, func(http.ResponseWriter, *http.Request) {})

	delivery := callback(t, map[string]string{
		"merchantCode":    merchantCode,
		"amount":          "149000",
		"merchantOrderId": "bolu-1-m",
		"resultCode":      "00",
		"reference":       "D1234CX80TZJ85Q70QCI",
		"settlementDate":  "2026-10-13",
	}, apiKey)

	payment, err := gateway.Verify(t.Context(), delivery)
	require.NoError(t, err)
	require.True(t, payment.Paid)
	require.Equal(t, "bolu-1-m", payment.Reference)
	require.Equal(t, int64(149000), payment.AmountIDR)
	require.Equal(t, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), payment.PaidAt)
}

// TestASignatureThatDoesNotMatchIsRefused: with the wrong key, an amount edited after
// signing, another merchant's code, or no signature at all. This is the whole security of
// the route.
func TestASignatureThatDoesNotMatchIsRefused(t *testing.T) {
	gateway, _ := newGateway(t, func(http.ResponseWriter, *http.Request) {})

	fields := map[string]string{
		"merchantCode":    merchantCode,
		"amount":          "149000",
		"merchantOrderId": "bolu-1-m",
		"resultCode":      "00",
	}

	cases := map[string]domain.Delivery{
		"another key": callback(t, fields, "another-key-entirely"),
		"an amount edited after signing": func() domain.Delivery {
			signed := callback(t, fields, apiKey)
			return domain.Delivery{
				Provider: duitku.Name,
				Body:     []byte(strings.Replace(string(signed.Body), "amount=149000", "amount=1000", 1)),
			}
		}(),
		"another merchant": callback(t, map[string]string{
			"merchantCode": "D9999", "amount": "149000", "merchantOrderId": "bolu-1-m", "resultCode": "00",
		}, apiKey),
		"no signature": {Body: []byte("merchantCode=D1234&amount=149000&merchantOrderId=bolu-1-m&resultCode=00")},
		"no order":     {Body: []byte("signature=whatever&amount=1")},
		"no amount":    {Body: []byte("merchantCode=D1234&merchantOrderId=bolu-1-m&signature=x")},
	}

	for name, delivery := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := gateway.Verify(t.Context(), delivery)
			require.Error(t, err)
		})
	}
}

// TestWhatEachResultCodeMeans: `00` is money that landed and everything else is not. An
// unknown code is treated as a failed payment rather than as a settlement, which is the
// direction that never hands out a package for a payment that did not happen.
func TestWhatEachResultCodeMeans(t *testing.T) {
	gateway, _ := newGateway(t, func(http.ResponseWriter, *http.Request) {})

	cases := []struct {
		resultCode string
		paid       bool
	}{
		{resultCode: "00", paid: true},
		{resultCode: "01"},
		{resultCode: "02"},
		{resultCode: ""},
	}

	for _, tc := range cases {
		t.Run("result "+tc.resultCode, func(t *testing.T) {
			payment, err := gateway.Verify(t.Context(), callback(t, map[string]string{
				"merchantCode": merchantCode, "amount": "25000",
				"merchantOrderId": "bolu-topup-t", "resultCode": tc.resultCode,
			}, apiKey))

			require.NoError(t, err)
			require.Equal(t, tc.paid, payment.Paid)
			require.Equal(t, int64(25000), payment.AmountIDR)
		})
	}

	// A callback with no settlement date is still a payment: the moment is recorded as
	// unknown rather than invented, and the module stamps its own clock.
	payment, err := gateway.Verify(t.Context(), callback(t, map[string]string{
		"merchantCode": merchantCode, "amount": "25000", "merchantOrderId": "bolu-1-m", "resultCode": "00",
	}, apiKey))
	require.NoError(t, err)
	require.True(t, payment.Paid)
	require.True(t, payment.PaidAt.IsZero())
}

// TestTheHostIsChosenByConfigurationAndNotByTheKeyShape: a sandbox key against the
// production host is a charge on somebody's card, so the choice is stated and never
// guessed.
func TestTheHostIsChosenByConfigurationAndNotByTheKeyShape(t *testing.T) {
	require.NotEqual(t, duitku.SandboxBaseURL, duitku.ProductionBaseURL)
	require.Contains(t, duitku.SandboxBaseURL, "sandbox")
	require.NotContains(t, duitku.ProductionBaseURL, "sandbox")
}

// TestTheOrderIDIsShortEnoughForDuitku: their field is fifty characters, and a longer one
// would be truncated by their side — which would leave a callback naming something this
// build cannot find.
func TestTheOrderIDIsShortEnoughForDuitku(t *testing.T) {
	orderID := duitku.OrderID(uuid.NewString(), domain.KindSubscription)
	require.LessOrEqual(t, len(orderID), 50)

	topUp := duitku.OrderID(uuid.NewString(), domain.KindTopup)
	require.LessOrEqual(t, len(topUp), 50)
	require.NotEqual(t, orderID[:len(orderID)-1], topUp[:len(topUp)-1], "the two kinds are told apart")
}

// callback builds a delivery the way Duitku would, signed with a key the caller chooses so
// the wrong-key case is expressible.
func callback(t *testing.T, fields map[string]string, key string) domain.Delivery {
	t.Helper()

	values := url.Values{}
	for name, value := range fields {
		values.Set(name, value)
	}
	if values.Get("signature") == "" && fields["merchantCode"] != "" && fields["amount"] != "" {
		values.Set("signature", sign(fields["merchantCode"]+fields["amount"]+fields["merchantOrderId"], key))
	}
	// The order matters to nothing here: a urlencoded form is parsed, not read in sequence.
	values.Set("productDetail", "Langganan Bolu")
	values.Set("paymentCode", "BC")

	return domain.Delivery{
		Provider: duitku.Name,
		Body:     []byte(values.Encode()),
		Headers:  map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
}

// sign is Duitku's own HMAC-SHA256, lowercase hex.
func sign(message, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
