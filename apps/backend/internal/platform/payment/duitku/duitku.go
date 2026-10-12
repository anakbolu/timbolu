// Package duitku adapts Duitku's payment gateway to the billing module's payment port.
//
// It lives in the platform layer rather than in the module because the module must stay
// runnable without it: without credentials an invoice is still written, and the screen
// says to transfer and confirm. This package knows Duitku's shapes; the module knows only
// what an invoice is.
//
// Two things about Duitku decide how this is written:
//
//   - it is a hosted payment page, not a charge API. The inquiry answers a `paymentUrl`
//     a person opens and a `vaNumber` for the methods that have one, so what crosses the
//     port is the instructions a person acts on rather than a link or a number.
//   - both directions are authenticated with the same primitive: HMAC-SHA256 over a
//     documented string, with the merchant's API key as the secret. The signature is
//     therefore the whole security of the callback, and it is compared in constant time.
package duitku

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
)

// Name is the provider key invoices record and callbacks arrive under.
const Name = "duitku"

// Hosts. Which one is used is configuration rather than a code change, because a sandbox
// key against the production host is a charge on somebody's card.
const (
	ProductionBaseURL = "https://passport.duitku.com"
	SandboxBaseURL    = "https://sandbox.duitku.com"
)

// InquiryPath is where a transaction is asked for.
const InquiryPath = "/webapi/api/merchant/v2/inquiry"

// Payment methods this adapter offers by name. They are Duitku's own two-letter codes,
// and the list is short on purpose: the codes change as banks are added, and the
// deployment names the one it sells.
const (
	// MethodBCAVA is BCA's virtual account, which is the one most Indonesian customers
	// recognise.
	MethodBCAVA = "BC"
	// MethodMaybankVA is Maybank's virtual account.
	MethodMaybankVA = "VA"
	// MethodPermataVA is Permata's virtual account.
	MethodPermataVA = "BT"
	// MethodQRIS makes a QR code a customer scans from any Indonesian wallet.
	MethodQRIS = "SP"
)

// DefaultMethod is what a deployment gets when it names none. A virtual account is what
// the product's customers already use, and a number they know how to pay is worth more
// than an optimal fee.
const DefaultMethod = MethodBCAVA

// DefaultExpiryMinutes is how long a payment stays payable when nothing says otherwise.
//
// A day is long enough for a transfer made after work and short enough that a workspace
// still inside its grace window can be asked again — and it is well under the grace
// window, so an expired link is never the reason a workspace stopped.
const DefaultExpiryMinutes = 1440

// Config is what a deployment sets to take online payment.
type Config struct {
	// MerchantCode identifies the project at Duitku. Empty means this deployment takes no
	// online payment.
	MerchantCode string
	// APIKey signs the inquiry and verifies the callback. Empty is refused at startup.
	APIKey string
	// Sandbox selects Duitku's sandbox host. It is explicit because a sandbox key against
	// the production host is a charge on somebody's card.
	Sandbox bool
	// PaymentMethod is which Duitku payment channel a person is offered.
	PaymentMethod string
	// ExpiryMinutes is how long the payment stays payable.
	ExpiryMinutes int
	// CallbackURL is where Duitku posts the payment notification. It must be an absolute
	// address this deployment serves, because Duitku reaches it from outside.
	CallbackURL string
	// ReturnURL is where a person lands after the payment page. Empty omits it, which is
	// a worse experience but not a refusal from Duitku.
	ReturnURL string
	// BaseURL overrides Duitku's host. It is what points a test, or a deployment
	// verifying its own wiring, at a stand-in.
	BaseURL string
	// HTTPClient is the client used. Nil takes one with a sane timeout, so a provider that
	// never answers cannot hold a request open forever.
	HTTPClient *http.Client
}

// Gateway is the Duitku-backed payment gateway.
type Gateway struct {
	cfg    Config
	client *http.Client
}

// New builds the gateway.
//
// A missing merchant code or key is refused here rather than at the first charge, so a
// deployment that meant to take payment but mistyped a value finds out at startup. A
// missing callback URL is refused for the same reason: Duitku would accept the
// transaction and then have nowhere to tell us it was paid, which is the one failure this
// whole path exists to avoid.
func New(cfg Config) (*Gateway, error) {
	if strings.TrimSpace(cfg.MerchantCode) == "" {
		return nil, fmt.Errorf("duitku: DUITKU_MERCHANT_CODE is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("duitku: DUITKU_API_KEY is required")
	}
	if strings.TrimSpace(cfg.CallbackURL) == "" {
		return nil, fmt.Errorf("duitku: DUITKU_CALLBACK_URL is required, or no payment can be confirmed")
	}
	if strings.TrimSpace(cfg.PaymentMethod) == "" {
		cfg.PaymentMethod = DefaultMethod
	}
	if cfg.ExpiryMinutes <= 0 {
		cfg.ExpiryMinutes = DefaultExpiryMinutes
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	return &Gateway{cfg: cfg, client: client}, nil
}

// Name implements the port.
func (g *Gateway) Name() string { return Name }

// inquiryRequest is Duitku's own shape for a transaction request.
//
// Only the fields this adapter fills are declared: a struct with every documented field
// would be a second place to keep in step with their API, and half of them are for payment
// methods this deployment does not sell.
type inquiryRequest struct {
	MerchantCode    string       `json:"merchantCode"`
	PaymentAmount   int64        `json:"paymentAmount"`
	PaymentMethod   string       `json:"paymentMethod"`
	MerchantOrderID string       `json:"merchantOrderId"`
	ProductDetails  string       `json:"productDetails"`
	AdditionalParam string       `json:"additionalParam,omitempty"`
	CustomerVaName  string       `json:"customerVaName"`
	Email           string       `json:"email"`
	ItemDetails     []itemDetail `json:"itemDetails,omitempty"`
	CallbackURL     string       `json:"callbackUrl"`
	ReturnURL       string       `json:"returnUrl,omitempty"`
	Signature       string       `json:"signature"`
	ExpiryPeriod    int          `json:"expiryPeriod"`
}

type itemDetail struct {
	Name     string `json:"name"`
	Price    int64  `json:"price"`
	Quantity int    `json:"quantity"`
}

// inquiryResponse is the part of Duitku's answer this adapter reads.
type inquiryResponse struct {
	MerchantCode  string `json:"merchantCode"`
	Reference     string `json:"reference"`
	PaymentURL    string `json:"paymentUrl"`
	VANumber      string `json:"vaNumber"`
	Amount        string `json:"amount"`
	StatusCode    string `json:"statusCode"`
	StatusMessage string `json:"statusMessage"`
}

// Charge asks Duitku for a payment for one invoice.
//
// The order id is derived from the invoice rather than random, so a charge whose response
// was lost is the same transaction when it is tried again — and Duitku refuses a repeated
// order id, which turns a lost response into a readable refusal instead of a second
// payment nobody can reconcile.
func (g *Gateway) Charge(ctx context.Context, invoice domain.Invoice, customer domain.Customer) (domain.Charge, error) {
	orderID := OrderID(invoice.ID.String(), invoice.Kind)

	product := productDetails(invoice)
	request := inquiryRequest{
		MerchantCode:    g.cfg.MerchantCode,
		PaymentAmount:   invoice.AmountIDR,
		PaymentMethod:   g.cfg.PaymentMethod,
		MerchantOrderID: orderID,
		ProductDetails:  product,
		CustomerVaName:  customerName(customer),
		Email:           customer.Email,
		ItemDetails:     []itemDetail{{Name: product, Price: invoice.AmountIDR, Quantity: 1}},
		CallbackURL:     g.cfg.CallbackURL,
		ReturnURL:       g.cfg.ReturnURL,
		Signature:       g.sign(orderID, invoice.AmountIDR),
		ExpiryPeriod:    g.cfg.ExpiryMinutes,
	}

	body, err := json.Marshal(request)
	if err != nil {
		return domain.Charge{}, fmt.Errorf("duitku: encode inquiry: %w", err)
	}

	response, err := g.post(ctx, InquiryPath, body)
	if err != nil {
		return domain.Charge{}, err
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return domain.Charge{}, fmt.Errorf("duitku: read inquiry response: %w", err)
	}

	var answer inquiryResponse
	if err := json.Unmarshal(payload, &answer); err != nil {
		return domain.Charge{}, fmt.Errorf("duitku: decode inquiry response: %w", err)
	}

	// Duitku answers 200 for an inquiry it refused, with the reason in the body, so the
	// status code alone is not the verdict — and a charge read as successful would leave a
	// payment page that does not exist.
	if response.StatusCode >= http.StatusMultipleChoices || !accepted(answer.StatusCode) {
		message := strings.TrimSpace(answer.StatusMessage)
		if message == "" {
			message = strings.TrimSpace(string(payload))
		}
		return domain.Charge{}, fmt.Errorf("duitku: inquiry refused (%s): %s", answer.StatusCode, message)
	}
	if strings.TrimSpace(answer.PaymentURL) == "" && strings.TrimSpace(answer.VANumber) == "" {
		return domain.Charge{}, fmt.Errorf("duitku: the inquiry carries neither a payment page nor a virtual account")
	}

	// The reference recorded is the *order id*, not Duitku's own reference, and that is the
	// one thing here that has to be right: the callback names `merchantOrderId`, so a
	// settlement is looked up by it. Recording Duitku's internal reference instead would
	// leave every payment unmatched and every period unextended, with a callback that
	// arrived and was accepted for an invoice nothing could find.
	return domain.Charge{
		Reference:    orderID,
		Instructions: instructions(answer),
		ExpiresAt:    time.Now().Add(time.Duration(g.cfg.ExpiryMinutes) * time.Minute),
	}, nil
}

// instructions is what a person acts on.
//
// A Duitku transaction usually has both: a payment page that lists every channel and a
// virtual account number for the channel that was asked for. Both are given, because the
// page is what reaches somebody who wants to choose and the number is what reaches
// somebody who already knows.
func instructions(answer inquiryResponse) string {
	switch {
	case strings.TrimSpace(answer.VANumber) != "" && strings.TrimSpace(answer.PaymentURL) != "":
		return fmt.Sprintf("Buka %s atau transfer ke virtual account %s", answer.PaymentURL, answer.VANumber)
	case strings.TrimSpace(answer.PaymentURL) != "":
		return "Buka " + answer.PaymentURL
	default:
		return "Transfer ke virtual account " + answer.VANumber
	}
}

// productDetails is what the payment page shows as the line item.
func productDetails(invoice domain.Invoice) string {
	switch invoice.Kind {
	case domain.KindTopup:
		return "Paket tambahan token Bolu"
	default:
		return "Langganan Bolu"
	}
}

// customerName is the name the bank side shows, which Duitku caps at twenty characters.
func customerName(customer domain.Customer) string {
	name := strings.TrimSpace(customer.Name)
	if name == "" {
		name = "Pemilik usaha"
	}
	if len([]rune(name)) > 20 {
		return string([]rune(name)[:20])
	}
	return name
}

// callback is the body Duitku posts when a payment happens. It arrives as a form, not as
// JSON, which is why the handler hands over the raw bytes and the headers rather than a
// parsed object.
type callback struct {
	MerchantCode    string
	Amount          string
	MerchantOrderID string
	ResultCode      string
	Reference       string
	PaymentCode     string
	SettlementDate  string
	Signature       string
}

// Verify checks a callback's signature and says what happened.
//
// The signature is Duitku's own: HMAC-SHA256 over `merchantCode + amount + merchantOrderId`
// with the API key. Recomputing it is the only thing standing between a stranger and a paid
// package, so it is compared in constant time and a delivery that fails it is refused
// rather than parsed hopefully.
func (g *Gateway) Verify(_ context.Context, delivery domain.Delivery) (domain.Payment, error) {
	payload, err := parseCallback(delivery)
	if err != nil {
		return domain.Payment{}, err
	}

	if payload.MerchantCode != g.cfg.MerchantCode {
		// A callback for another project is not ours to act on, whatever its signature
		// says: the key is per project, so this is the one case a correct signature on a
		// foreign payload would otherwise be accepted.
		return domain.Payment{}, fmt.Errorf("duitku: the callback names another merchant")
	}

	expected := g.verify(payload.MerchantOrderID, payload.Amount)
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(payload.Signature))) {
		return domain.Payment{}, fmt.Errorf("duitku: the callback signature does not match")
	}

	amount, err := strconv.ParseInt(strings.TrimSpace(payload.Amount), 10, 64)
	if err != nil {
		return domain.Payment{}, fmt.Errorf("duitku: the callback amount %q is not a number", payload.Amount)
	}

	payment := domain.Payment{
		// The order id is what the callback names and what the invoice recorded, so it is
		// the reference a settlement is looked up by.
		Reference: payload.MerchantOrderID,
		AmountIDR: amount,
	}

	switch strings.TrimSpace(payload.ResultCode) {
	case "00":
		payment.Paid = true
		payment.PaidAt = settledAt(payload.SettlementDate)
	default:
		// `01` is a failed payment and anything else is a status this build does not know.
		// Both leave the invoice outstanding or mark it failed, which is the caller's
		// decision: this returns what the provider said and nothing more. A payment link
		// that ran out arrives here as `01` too, and the sweep closes it as expired.
	}

	return payment, nil
}

// parseCallback reads the form Duitku posts.
func parseCallback(delivery domain.Delivery) (callback, error) {
	// The body is a urlencoded form, so it is parsed as one. The header map is not
	// consulted: Duitku sends everything in the body and nothing that has to be read from
	// a header.
	values, err := url.ParseQuery(string(delivery.Body))
	if err != nil {
		return callback{}, fmt.Errorf("duitku: decode callback: %w", err)
	}

	payload := callback{
		MerchantCode:    values.Get("merchantCode"),
		Amount:          values.Get("amount"),
		MerchantOrderID: values.Get("merchantOrderId"),
		ResultCode:      values.Get("resultCode"),
		Reference:       values.Get("reference"),
		PaymentCode:     values.Get("paymentCode"),
		SettlementDate:  values.Get("settlementDate"),
		Signature:       values.Get("signature"),
	}

	if payload.MerchantOrderID == "" || payload.Signature == "" {
		return callback{}, fmt.Errorf("duitku: the callback names no order")
	}
	if payload.Amount == "" {
		return callback{}, fmt.Errorf("duitku: the callback states no amount")
	}

	return payload, nil
}

// post sends one JSON request.
func (g *Gateway) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL()+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("duitku: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("duitku: %w", err)
	}
	return response, nil
}

// baseURL is the host this gateway talks to.
func (g *Gateway) baseURL() string {
	if strings.TrimSpace(g.cfg.BaseURL) != "" {
		return strings.TrimRight(g.cfg.BaseURL, "/")
	}
	if g.cfg.Sandbox {
		return SandboxBaseURL
	}
	return ProductionBaseURL
}

// sign is the signature of an inquiry: `merchantCode + merchantOrderId + paymentAmount`.
func (g *Gateway) sign(orderID string, amount int64) string {
	return hmacHex(g.cfg.MerchantCode+orderID+strconv.FormatInt(amount, 10), g.cfg.APIKey)
}

// verify is the signature of a callback: `merchantCode + amount + merchantOrderId`.
//
// The two are different strings, and that is Duitku's own asymmetry rather than a mistake
// here: the inquiry signs the amount from our side and the callback signs the amount from
// theirs, and the order of the parts is what their documentation states.
func (g *Gateway) verify(orderID, amount string) string {
	return hmacHex(g.cfg.MerchantCode+amount+orderID, g.cfg.APIKey)
}

// hmacHex is one HMAC-SHA256 rendered the way Duitku renders it: lowercase hex.
func hmacHex(message, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// OrderID is the reference Duitku echoes back, and therefore the one the callback names.
//
// It is derived from the invoice rather than random, so a transaction whose response was
// lost is the same transaction when it is tried again: Duitku refuses a repeated order id,
// which is how a lost response becomes a readable refusal instead of a second payment.
func OrderID(invoiceID, kind string) string {
	suffix := "m"
	if kind == domain.KindTopup {
		suffix = "t"
	}
	// Duitku caps the field at fifty characters, and a uuid plus a prefix and a suffix is
	// forty. A longer one would be truncated by their side and the callback would then name
	// something this build cannot find.
	return "bolu-" + invoiceID + "-" + suffix
}

// accepted reports whether Duitku's own status code is a success. Its codes are strings,
// and `00` is the one that means the transaction was created.
func accepted(statusCode string) bool {
	return strings.TrimSpace(statusCode) == "00"
}

// settledAt reads the settlement date, which Duitku sends as a day.
func settledAt(raw string) time.Time {
	text := strings.TrimSpace(raw)
	if text == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ErrNotConfigured is what a caller gets when a deployment has no gateway.
var ErrNotConfigured = errors.New("duitku: not configured")
