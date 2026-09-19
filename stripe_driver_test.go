package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/stripe/stripe-go/v86"

	"github.com/stripe/stripe-go/v86/webhook"
	"github.com/xgfone/go-payment-driver/builder"
	"github.com/xgfone/go-payment-driver/driver"
)

func testConfig() Config {
	return Config{
		WebhookSecret: "whsec_local",

		SecretKey:  "sk_test_local",
		SuccessURL: "https://example.com/success?session_id={CHECKOUT_SESSION_ID}",
		CancelURL:  "https://example.com/cancel",
		Currencies: []string{"USD"}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real SDK's serialization, decoding, pagination and error types
// without sockets, credentials or requests to Stripe.
func testDriver(t *testing.T, handle http.HandlerFunc) *Driver {
	t.Helper()
	base, err := newDriver(builder.Get("stripe_checkout"), testConfig())
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if handle == nil {
			t.Fatalf("unexpected Stripe request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer sk_test_local" {
			t.Error("missing API key")
		}
		if r.Header.Get("Stripe-Version") != sdk.APIVersion {
			t.Error("incorrect API version")
		}

		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		handle(w, r)
		return w.Result(), nil
	})}

	base.client = sdk.NewClient(base.config.SecretKey, sdk.WithBackends(sdk.NewBackendsWithConfig(&sdk.BackendConfig{
		HTTPClient:        client,
		MaxNetworkRetries: sdk.Int64(0),
		LeveledLogger:     &sdk.LeveledLogger{Level: sdk.LevelNull},
	})))

	return &Driver{_Driver: base}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func stripeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":%q,"message":%q}}`, code, code)
}

func TestBuilder(t *testing.T) {
	b := builder.Get("stripe_checkout")
	if b == nil {
		t.Fatal("builder not registered")
	}

	config := testConfig()
	config.Currencies = []string{"usd", "eur"}
	config.PaymentMethodTypes = []string{"card", "alipay"}
	raw, _ := json.Marshal(config)
	d, err := builder.BuildDriver("stripe_checkout", string(raw))
	if err != nil {
		t.Fatal(err)
	}

	want := driver.Metadata{
		Type:       "stripe_checkout",
		Provider:   "stripe",
		PayScene:   "checkout",
		LinkType:   driver.LinkTypeCodeUrl,
		Currencies: []string{"USD", "EUR"},
		Channels:   []string{"card", "alipay"},
	}
	if got := d.Metadata(); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata = %#v", got)
	}

	changed := d.Metadata()
	changed.Currencies[0], changed.Channels[0] = "JPY", "other"
	if got := d.Metadata(); !reflect.DeepEqual(got, want) {
		t.Fatal("metadata exposes mutable configuration")
	}

	display, err := builder.ParseConfig("stripe_checkout", string(raw))
	if err != nil {
		t.Fatal(err)
	}

	if conf := display.(Config); strings.Contains(conf.SecretKey, "local") ||
		strings.Contains(conf.WebhookSecret, "local") {
		t.Fatal("configuration display leaks secrets")
	}
	if _, err := b.BuildDriver(Config{}); err == nil {
		t.Fatal("direct build must validate configuration")
	}
	if config.Currencies[0] != "usd" {
		t.Fatal("configuration slices were mutated")
	}
}

func TestConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"key", func(c *Config) { c.SecretKey = "" }},
		{"webhook", func(c *Config) { c.WebhookSecret = "" }},
		{"success", func(c *Config) { c.SuccessURL = "/success" }},
		{"cancel", func(c *Config) { c.CancelURL = "javascript:alert(1)" }},
		{"missing currencies", func(c *Config) { c.Currencies = nil }},
		{"invalid currency", func(c *Config) { c.Currencies = []string{"US"} }},
		{"currencies", func(c *Config) { c.Currencies = []string{""} }},
		{"method", func(c *Config) { c.PaymentMethodTypes = []string{""} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.change(&c)
			if c.Init() == nil {
				t.Fatal("expected configuration error")
			}
		})
	}

	c := testConfig()
	if err := c.Init(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Currencies, []string{"USD"}) ||
		!reflect.DeepEqual(c.PaymentMethodTypes, []string{"card"}) {
		t.Fatalf("defaults = %+v", c)
	}

	c = testConfig()
	c.Currencies = []string{"jpy"}
	if err := c.Init(); err != nil || !reflect.DeepEqual(c.Currencies, []string{"JPY"}) {
		t.Fatalf("currencies = %v, %v", c.Currencies, err)
	}
}

func TestCreatePayment(t *testing.T) {
	var bodies []string
	var keys []string
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/checkout/sessions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}

		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		form, _ := url.ParseQuery(string(body))
		for k, want := range map[string]string{
			"mode": "payment",

			"client_reference_id":  "pay_1",
			"metadata[payment_id]": "pay_1",

			"line_items[0][quantity]":                       "1",
			"line_items[0][price_data][currency]":           "usd",
			"line_items[0][price_data][unit_amount]":        "1250",
			"line_items[0][price_data][product_data][name]": "pay_1",

			"payment_intent_data[metadata][payment_id]": "pay_1",

			"payment_method_types[0]":   "card",
			"adaptive_pricing[enabled]": "false",

			"success_url": dSuccessURL(),
		} {
			if form.Get(k) != want {
				t.Errorf("%s = %q, want %q", k, form.Get(k), want)
			}
		}

		if form.Get("expires_at") != "" {
			t.Error("default expiry must be stable across retries")
		}
		writeJSON(t, w, map[string]any{
			"id":  "cs_1",
			"url": "https://checkout.stripe.com/pay/cs_1",

			"payment_intent": "pi_1",
		})
	})

	req := driver.CreatePaymentRequest{
		PaymentId:       "pay_1",
		PaymentAmount:   1250,
		PaymentCurrency: "USD",
	}
	for range 2 {
		info, err := d.CreatePayment(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}

		cd := driver.DecodeChannelData[ChannelData](info.ChannelData)
		if info.ChannelPaymentId != "cs_1" || info.PayLink == "" ||
			cd.PaymentIntentId != "pi_1" || cd.CheckoutSessionId != "cs_1" {
			t.Fatalf("pay link = %+v / %+v", info, cd)
		}
	}

	if bodies[0] != bodies[1] || keys[0] == "" || keys[0] != keys[1] {
		t.Fatal("creation retries are not idempotent")
	}
}

func dSuccessURL() string { return testConfig().SuccessURL }

func TestCreateValidationAndExpiry(t *testing.T) {
	d := testDriver(t, nil)
	for _, req := range []driver.CreatePaymentRequest{
		{PaymentAmount: 100, PaymentCurrency: "USD"},
		{PaymentId: "p", PaymentAmount: 0, PaymentCurrency: "USD"},
		{PaymentId: "p", PaymentAmount: 100, PaymentCurrency: "EUR"},
		{PaymentId: "p", PaymentAmount: 100, PaymentCurrency: "USD", Share: true},
		{PaymentId: "p", PaymentAmount: 100, PaymentCurrency: "USD", ExpiresIn: 5 * time.Minute},
		{PaymentId: "p", PaymentAmount: 100, PaymentCurrency: "USD", ExpiresIn: -time.Minute},
		{PaymentId: "p", PaymentAmount: 100, PaymentCurrency: "USD", ExpiresIn: 25 * time.Hour},
		{PaymentId: "p", PaymentAmount: 100, PaymentCurrency: "USD", ExtInfo: "invalid"},
	} {
		if _, err := d.CreatePayment(context.Background(), req); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}

	fixed := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, ext := range []any{
		CheckoutOptions{ExpiresAt: fixed},
		&CheckoutOptions{ExpiresAt: fixed},
	} {
		req := driver.CreatePaymentRequest{
			ExtInfo:   ext,
			ExpiresIn: time.Minute,
		}
		first, err := checkoutExpiry(req)
		if err != nil || *first != fixed.Unix() {
			t.Fatalf("expiry: %v %v", first, err)
		}

		second, _ := checkoutExpiry(req)
		if *first != *second {
			t.Fatal("absolute expiry changed")
		}
	}

	before := time.Now().Add(time.Hour).Unix()
	expiry, err := checkoutExpiry(driver.CreatePaymentRequest{ExpiresIn: time.Hour})
	if err != nil || *expiry < before || *expiry > before+1 {
		t.Fatalf("relative expiry: %v %v", expiry, err)
	}

	d = testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]string{"id": "cs_1"})
	})
	_, err = d.CreatePayment(context.Background(), driver.CreatePaymentRequest{
		PaymentId:       "p",
		PaymentAmount:   100,
		PaymentCurrency: "USD",
	})
	if err == nil {
		t.Fatal("accepted missing payment link")
	}
}

func TestCreatePaymentJSONOptions(t *testing.T) {
	fixed := time.Now().Add(time.Hour).Truncate(time.Second)
	raw, err := json.Marshal(CheckoutOptions{ExpiresAt: fixed})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		raw       json.RawMessage
		deadline  bool
		wantError bool
	}{
		{name: "deadline", raw: raw, deadline: true},
		{name: "nil"},
		{name: "empty", raw: json.RawMessage{}},
		{name: "empty object", raw: json.RawMessage(`{}`)},
		{name: "null", raw: json.RawMessage(`null`)},
		{name: "invalid JSON", raw: json.RawMessage(`{"ExpiresAt":`), wantError: true},
		{name: "array", raw: json.RawMessage(`[]`), wantError: true},
		{name: "invalid timestamp", raw: json.RawMessage(`{"ExpiresAt":"invalid"}`), wantError: true},
		{name: "numeric timestamp", raw: json.RawMessage(`{"ExpiresAt":1700000000}`), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.wantError {
					t.Fatal("invalid JSON options reached Stripe")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}

				var want string
				if tc.deadline {
					want = fmt.Sprint(fixed.Unix())
				}
				if got := r.Form.Get("expires_at"); got != want {
					t.Errorf("expires_at = %q, want %q", got, want)
				}
				writeJSON(t, w, map[string]string{
					"id":  "cs_1",
					"url": "https://checkout.stripe.com/pay/cs_1",
				})
			})
			req := driver.CreatePaymentRequest{
				PaymentId:       "pay_1",
				PaymentAmount:   100,
				PaymentCurrency: "USD",
				ExtInfo:         tc.raw,
			}
			if tc.deadline {
				// A decoded absolute deadline must take precedence over ExpiresIn.
				req.ExpiresIn = time.Minute
			}

			_, err := d.CreatePayment(context.Background(), req)
			if tc.wantError {
				if !errors.Is(err, driver.ErrBadRequest) {
					t.Fatalf("error = %v, want ErrBadRequest", err)
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("error = %v, Stripe requests = %d", err, calls)
			}
		})
	}

	// Empty JSON options must still honor a relative expiration.
	expiry, err := checkoutExpiry(driver.CreatePaymentRequest{
		ExtInfo: json.RawMessage(`{}`), ExpiresIn: 5 * time.Minute,
	})
	if expiry != nil || !errors.Is(err, driver.ErrBadRequest) {
		t.Fatalf("relative expiration validation: %v, %v", expiry, err)
	}
}

func TestQueryPayment(t *testing.T) {
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/checkout/sessions/cs_1" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		if !strings.Contains(r.URL.RawQuery, "payment_intent.latest_charge") {
			t.Error("charge not expanded")
		}
		_, _ = io.WriteString(w, `{"id":"cs_1","client_reference_id":"pay_1","status":"complete","payment_status":"paid","amount_total":1250,"currency":"usd","created":100,"customer":"cus_1","payment_intent":{"id":"pi_1","status":"succeeded","latest_charge":{"id":"ch_1","refunded":true}}}`)
	})

	info, ok, err := d.QueryPayment(context.Background(), driver.QueryPaymentRequest{
		ChannelData: `{"CheckoutSessionId":"cs_1","PaymentIntentId":"pi_1"}`,
	})
	if err != nil || !ok {
		t.Fatalf("query: %v, %v", ok, err)
	}

	if info.ChannelPaymentId != "cs_1" || info.PaymentId != "pay_1" ||
		info.TaskStatus != driver.TaskStatusSuccess || !info.IsRefunded ||
		info.PayerPaidAmount != 1250 || info.PayerPaidCurrency != "USD" ||
		info.PayerId != "cus_1" {
		t.Fatalf("payment = %+v", info)
	}
	if !info.PayerPaidAt.IsZero() {
		t.Fatal("creation time used as payment time")
	}

	cd := driver.DecodeChannelData[ChannelData](info.ChannelData)
	if cd.LatestChargeId != "ch_1" {
		t.Fatalf("channel data = %+v", cd)
	}
}

func TestPaymentStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status sdk.CheckoutSessionStatus
		paid   sdk.CheckoutSessionPaymentStatus
		intent sdk.PaymentIntentStatus
		failed bool
		want   driver.TaskStatus
	}{
		{"open", "open", "unpaid", "", false, driver.TaskStatusProcessing},
		{"retryable decline", "open", "unpaid", "requires_payment_method", true, driver.TaskStatusProcessing},
		{"async pending", "complete", "unpaid", "processing", false, driver.TaskStatusProcessing},
		{"async failed", "complete", "unpaid", "requires_payment_method", true, driver.TaskStatusFailure},
		{"expired", "expired", "unpaid", "requires_payment_method", true, driver.TaskStatusClosed},
		{"paid", "complete", "paid", "succeeded", false, driver.TaskStatusSuccess},
		{"free", "complete", "no_payment_required", "", false, driver.TaskStatusSuccess},
		{"unknown", "future", "future", "", false, driver.TaskStatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &sdk.CheckoutSession{
				Status:        tc.status,
				PaymentStatus: tc.paid,
				AmountTotal:   100,
				Currency:      "usd",
				PaymentIntent: &sdk.PaymentIntent{Status: tc.intent},
			}
			if tc.failed {
				session.PaymentIntent.LastPaymentError = &sdk.Error{Msg: "declined"}
			}

			got := sessionToPaymentInfo(session)
			if got.TaskStatus != tc.want {
				t.Fatalf("status = %s, want %s", got.TaskStatus, tc.want)
			}
			if tc.want != driver.TaskStatusSuccess &&
				(got.PayerPaidAmount != 0 || got.PayerPaidCurrency != "") {
				t.Fatal("unpaid session reported as paid")
			}
		})
	}
}

func TestCancelPayment(t *testing.T) {
	for _, tc := range []struct {
		name, status, paid string
		expire             bool
		race               string
		want               error
	}{
		{name: "open", status: "open", paid: "unpaid", expire: true},
		{name: "expired", status: "expired", paid: "unpaid"},
		{name: "paid", status: "complete", paid: "paid", want: driver.ErrPaid},
		{name: "async processing", status: "complete", paid: "unpaid", want: driver.ErrUnallowed},
		{name: "paid race", status: "open", paid: "unpaid", expire: true, race: "paid", want: driver.ErrPaid},
		{name: "expired race", status: "open", paid: "unpaid", expire: true, race: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gets, posts := 0, 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets++
					status, paid := tc.status, tc.paid
					if gets > 1 && tc.race == "paid" {
						status, paid = "complete", "paid"
					}
					if gets > 1 && tc.race == "expired" {
						status = "expired"
					}
					writeJSON(t, w, map[string]string{
						"id":             "cs_1",
						"status":         status,
						"payment_status": paid,
					})
				} else {
					posts++
					if r.URL.Path != "/v1/checkout/sessions/cs_1/expire" ||
						r.Header.Get("Idempotency-Key") == "" {
						t.Fatalf("invalid cancel request %s", r.URL)
					}
					if tc.race != "" {
						stripeError(w, 400, "checkout_session_not_open")
					} else {
						_, _ = io.WriteString(w, `{"id":"cs_1","status":"expired"}`)
					}
				}
			})

			err := d.CancelPayment(context.Background(), driver.CancelPaymentRequest{
				ChannelPaymentId: "cs_1",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("cancel = %v, want %v", err, tc.want)
			}
			if (posts > 0) != tc.expire {
				t.Fatalf("expire calls = %d", posts)
			}
		})
	}
}

func TestNotFoundAndErrors(t *testing.T) {
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		stripeError(w, 404, "resource_missing")
	})

	_, ok, err := d.QueryPayment(context.Background(), driver.QueryPaymentRequest{
		ChannelPaymentId: "cs_missing",
	})
	if ok || err != nil {
		t.Fatalf("query = %v %v", ok, err)
	}

	_, ok, err = d.QueryRefund(context.Background(), driver.QueryRefundRequest{
		ChannelRefundId: "re_missing",
	})
	if ok || err != nil {
		t.Fatalf("refund = %v %v", ok, err)
	}

	err = d.CancelPayment(context.Background(), driver.CancelPaymentRequest{
		ChannelPaymentId: "cs_missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		code sdk.ErrorCode
		want error
	}{
		{sdk.ErrorCodeAmountTooSmall, driver.ErrTooSmallPaymentAmount},
		{sdk.ErrorCodeBalanceInsufficient, driver.ErrBalanceInsufficient},
		{sdk.ErrorCodeChargeAlreadyRefunded, driver.ErrPaymentRefundedFully},
	} {
		err := &sdk.Error{Type: sdk.ErrorTypeInvalidRequest, Code: tc.code}
		got := wrapError(fmt.Errorf("wrapped: %w", err))
		if got != tc.want {
			t.Fatalf("code %s = %v, want %v", tc.code, got, tc.want)
		}
	}

	original := &sdk.Error{Type: sdk.ErrorTypeAPI, Msg: "unavailable"}
	if !errors.Is(wrapError(original), original) {
		t.Fatal("lost original API error")
	}
}

func TestRefundPaymentAndIdempotency(t *testing.T) {
	bodies, keys := []string{}, []string{}
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/checkout/sessions/cs_1":
			_, _ = io.WriteString(w, `{"id":"cs_1","payment_intent":"pi_1"}`)

		case "/v1/payment_intents/pi_1":
			_, _ = io.WriteString(w, `{"id":"pi_1","status":"succeeded","currency":"usd","amount_received":1250,"metadata":{"payment_id":"pay_1"}}`)

		case "/v1/refunds":
			if r.Method != "POST" {
				t.Fatal("expected POST")
			}
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(body))
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			form, _ := url.ParseQuery(string(body))
			for k, want := range map[string]string{
				"payment_intent":                "pi_1",
				"amount":                        "500",
				"metadata[payment_id]":          "pay_1",
				"metadata[refund_id]":           "refund_1",
				"metadata[refund_reason]":       "returned item",
				"metadata[checkout_session_id]": "cs_1",
				"reason":                        "requested_by_customer",
			} {
				if form.Get(k) != want {
					t.Errorf("%s = %q", k, form.Get(k))
				}
			}
			_, _ = io.WriteString(w, `{"id":"re_1","status":"pending","created":100,"payment_intent":"pi_1","charge":"ch_1","metadata":{"payment_id":"pay_1","refund_id":"refund_1","refund_reason":"returned item","checkout_session_id":"cs_1"}}`)

		default:
			t.Fatalf("unexpected request %s", r.URL)
		}
	})

	req := driver.CreateRefundRequest{
		PaymentId:        "pay_1",
		RefundId:         "refund_1",
		ChannelPaymentId: "cs_1",
		PaymentAmount:    1250,
		PaymentCurrency:  "USD",
		RefundAmount:     500,
		RefundReason:     "returned item",
	}
	for range 2 {
		info, err := d.RefundPayment(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if info.ChannelRefundId != "re_1" || info.RefundId != "refund_1" ||
			info.PaymentId != "pay_1" || info.TaskStatus != driver.TaskStatusProcessing ||
			info.RefundReason != "returned item" || !info.RefundedAt.IsZero() {
			t.Fatalf("refund = %+v", info)
		}
	}
	if bodies[0] != bodies[1] || keys[0] == "" || keys[0] != keys[1] ||
		keys[0] == idempotencyKey("payment", "refund_1") {
		t.Fatal("refund retry keys or parameters differ")
	}
}

func TestQueryRefundPagination(t *testing.T) {
	pages := 0
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/refunds" || r.URL.Query().Get("payment_intent") != "pi_1" {
			t.Fatalf("bad list request %s", r.URL)
		}

		pages++
		if pages == 1 {
			_, _ = io.WriteString(w, `{"object":"list","url":"/v1/refunds","has_more":true,"data":[{"id":"re_other","metadata":{"refund_id":"other"}}]}`)
		} else {
			if r.URL.Query().Get("starting_after") != "re_other" {
				t.Error("missing pagination cursor")
			}
			_, _ = io.WriteString(w, `{"object":"list","url":"/v1/refunds","has_more":false,"data":[{"id":"re_1","status":"succeeded","metadata":{"payment_id":"pay_1","refund_id":"refund_1"}}]}`)
		}
	})

	info, ok, err := d.QueryRefund(context.Background(), driver.QueryRefundRequest{
		RefundId:    "refund_1",
		ChannelData: `{"PaymentIntentId":"pi_1"}`,
	})
	if err != nil || !ok || info.ChannelRefundId != "re_1" ||
		info.TaskStatus != driver.TaskStatusSuccess || pages != 2 {
		t.Fatalf("refund = %+v, %v %v, pages %d", info, ok, err, pages)
	}
	if !info.RefundedAt.IsZero() {
		t.Fatal("query invented refund completion time")
	}
}

func TestQueryRefundByChannelID(t *testing.T) {
	d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/refunds/re_1" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		_, _ = io.WriteString(w, `{"id":"re_1","status":"failed","failure_reason":"declined"}`)
	})

	info, ok, err := d.QueryRefund(context.Background(), driver.QueryRefundRequest{
		PaymentId:       "pay_1",
		RefundId:        "refund_1",
		ChannelRefundId: "re_1",
	})
	if err != nil || !ok || info.PaymentId != "pay_1" || info.RefundId != "refund_1" ||
		info.TaskStatus != driver.TaskStatusFailure || info.FailReason != "declined" {
		t.Fatalf("refund = %+v, %v %v", info, ok, err)
	}
}

func signedRequest(t *testing.T, eventType string, object any, timestamp time.Time, version string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":          "evt_1",
		"object":      "event",
		"type":        eventType,
		"created":     1700000000,
		"api_version": version,
		"data":        map[string]any{"object": object},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   body,
		Secret:    "whsec_local",
		Timestamp: timestamp,
	})
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(payload.Payload)))
	req.Header.Set("Stripe-Signature", payload.Header)
	return req
}

func TestCallbacks(t *testing.T) {
	d := testDriver(t, nil)
	for _, tc := range []struct {
		event, status, paid string
		want                driver.TaskStatus
	}{
		{"checkout.session.completed", "complete", "paid", driver.TaskStatusSuccess},
		{"checkout.session.completed", "complete", "unpaid", driver.TaskStatusProcessing},
		{"checkout.session.async_payment_succeeded", "complete", "paid", driver.TaskStatusSuccess},
		{"checkout.session.async_payment_failed", "complete", "unpaid", driver.TaskStatusFailure},
		{"checkout.session.expired", "expired", "unpaid", driver.TaskStatusClosed},
	} {
		t.Run(tc.event+"/"+tc.paid, func(t *testing.T) {
			object := map[string]any{
				"id":                  "cs_1",
				"mode":                "payment",
				"client_reference_id": "pay_1",
				"status":              tc.status,
				"payment_status":      tc.paid,
				"payment_intent":      "pi_1",
				"amount_total":        1250,
				"currency":            "usd",
			}
			cb, err := d.ParseCallbackRequest(context.Background(), driver.CallbackRequest{
				Request: signedRequest(t, tc.event, object, time.Now(), sdk.APIVersion),
			})
			if err != nil {
				t.Fatal(err)
			}

			if cb.Type != driver.CallbackTypePayment || cb.PaymentInfo == nil ||
				cb.PaymentInfo.TaskStatus != tc.want || cb.PaymentInfo.ChannelPaymentId != "cs_1" ||
				cb.PaymentInfo.PaymentId != "pay_1" {
				t.Fatalf("callback = %+v", cb)
			}
			if tc.want == driver.TaskStatusSuccess && cb.PaymentInfo.PayerPaidAt.Unix() != 1700000000 {
				t.Fatal("missing success event time")
			}
			if tc.want != driver.TaskStatusSuccess && (cb.PaymentInfo.PayerPaidAmount != 0 ||
				!cb.PaymentInfo.PayerPaidAt.IsZero()) {
				t.Fatal("unpaid callback contains paid values")
			}
		})
	}

	for _, tc := range []struct {
		event, status string
		want          driver.TaskStatus
	}{
		{"refund.created", "pending", driver.TaskStatusProcessing},
		{"refund.updated", "succeeded", driver.TaskStatusSuccess},
		{"refund.failed", "failed", driver.TaskStatusFailure},
		{"refund.updated", "canceled", driver.TaskStatusFailure},
		{"refund.updated", "requires_action", driver.TaskStatusProcessing},
	} {
		t.Run(tc.event+"/"+tc.status, func(t *testing.T) {
			object := map[string]any{
				"id":             "re_1",
				"status":         tc.status,
				"payment_intent": "pi_1",
				"metadata": map[string]string{
					"payment_id": "pay_1",
					"refund_id":  "refund_1",
				},
			}
			cb, err := d.ParseCallbackRequest(context.Background(), driver.CallbackRequest{
				Request: signedRequest(t, tc.event, object, time.Now(), sdk.APIVersion),
			})
			if err != nil || cb.Type != driver.CallbackTypeRefund || cb.RefundInfo == nil ||
				cb.RefundInfo.TaskStatus != tc.want || cb.RefundInfo.RefundId != "refund_1" {
				t.Fatalf("callback = %+v, %v", cb, err)
			}
			if tc.want != driver.TaskStatusSuccess && !cb.RefundInfo.RefundedAt.IsZero() {
				t.Fatal("incomplete refund contains success time")
			}
		})
	}

	cb, err := d.ParseCallbackRequest(context.Background(), driver.CallbackRequest{
		Request: signedRequest(t, "payment_intent.payment_failed", map[string]string{"id": "pi_1"}, time.Now(), sdk.APIVersion),
	})
	if err != nil || cb.Type != "" || cb.PaymentInfo != nil || cb.RefundInfo != nil {
		t.Fatal("unhandled event must be ignored")
	}
}

func TestWebhookValidation(t *testing.T) {
	d := testDriver(t, nil)
	object := map[string]string{"id": "cs_1", "mode": "payment"}
	valid := func() *http.Request {
		return signedRequest(t, "checkout.session.completed", object, time.Now(), sdk.APIVersion)
	}

	missing := valid()
	missing.Header.Del("Stripe-Signature")
	tampered := valid()
	tampered.Body = io.NopCloser(strings.NewReader(`{"tampered":true}`))
	oversized := httptest.NewRequest("POST", "/webhook", strings.NewReader(strings.Repeat("x", maxWebhookBody+1)))
	for name, req := range map[string]*http.Request{
		"nil":               nil,
		"missing signature": missing,
		"tampered":          tampered,
		"oversized":         oversized,
		"expired signature": signedRequest(t, "checkout.session.completed", object, time.Now().Add(-10*time.Minute), sdk.APIVersion),
		"API mismatch":      signedRequest(t, "checkout.session.completed", object, time.Now(), "2020-08-27"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.ParseCallbackRequest(context.Background(), driver.CallbackRequest{Request: req})
			if err == nil {
				t.Fatal("accepted invalid webhook")
			}
		})
	}
}

func TestCallbackResponse(t *testing.T) {
	d := testDriver(t, nil)
	for _, err := range []error{nil, errors.New("private database error")} {
		w := httptest.NewRecorder()
		d.SendCallbackResponse(context.Background(), w, err)
		if err == nil {
			if w.Code != 200 || w.Body.String() != `{"received":true}` ||
				w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response = %v", w)
			}
		} else if w.Code < 500 || strings.Contains(w.Body.String(), "private") {
			t.Fatal("failed webhook not retried or internal error disclosed")
		}
	}
}

func TestCurrencyAmounts(t *testing.T) {
	for _, tc := range []struct {
		code        string
		minor, wire int64
	}{
		{"USD", 1250, 1250},
		{"JPY", 1250, 1250},
		{"ISK", 1250, 125000},
		{"UGX", 1250, 125000},
		{"MGA", 125000, 1250},
		{"KWD", 1250, 1250},
	} {
		got, err := stripeAmount(tc.minor, tc.code)
		if err != nil || got != tc.wire || paidAmount(got, tc.code) != tc.minor {
			t.Fatalf("%s amount = %d %v", tc.code, got, err)
		}
	}

	for _, tc := range []struct {
		amount int64
		code   string
	}{
		{0, "USD"},
		{-1, "USD"},
		{math.MaxInt64, "ISK"},
		{101, "MGA"},
	} {
		if _, err := stripeAmount(tc.amount, tc.code); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestRefundValidation(t *testing.T) {
	base := driver.CreateRefundRequest{
		PaymentId:        "pay_1",
		RefundId:         "refund_1",
		RefundAmount:     100,
		PaymentCurrency:  "USD",
		ChannelPaymentId: "cs_1",
	}
	for _, tc := range []struct {
		name   string
		change func(*driver.CreateRefundRequest)
	}{
		{"missing payment ID", func(r *driver.CreateRefundRequest) { r.PaymentId = "" }},
		{"missing refund ID", func(r *driver.CreateRefundRequest) { r.RefundId = "" }},
		{"negative amount", func(r *driver.CreateRefundRequest) { r.RefundAmount = -1 }},
		{"exceeds requested payment", func(r *driver.CreateRefundRequest) { r.PaymentAmount = 50 }},
		{"long reason", func(r *driver.CreateRefundRequest) { r.RefundReason = strings.Repeat("x", 501) }},
		{"mismatched session", func(r *driver.CreateRefundRequest) {
			r.ChannelData = `{"CheckoutSessionId":"cs_other","PaymentIntentId":"pi_other"}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.change(&req)
			_, err := testDriver(t, nil).RefundPayment(context.Background(), req)
			if !errors.Is(err, driver.ErrBadRequest) {
				t.Fatalf("validation = %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name, status, currency, paymentID string
		received                          int64
		want                              error
	}{
		{"unpaid", "processing", "usd", "pay_1", 100, driver.ErrUnallowed},
		{"wrong currency", "succeeded", "eur", "pay_1", 100, driver.ErrBadRequest},
		{"wrong payment", "succeeded", "usd", "pay_other", 100, driver.ErrBadRequest},
		{"exceeds captured amount", "succeeded", "usd", "pay_1", 50, driver.ErrUnallowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/payment_intents/pi_1" {
					t.Fatalf("refund must not be created: %s %s", r.Method, r.URL)
				}

				writeJSON(t, w, map[string]any{
					"id":              "pi_1",
					"status":          tc.status,
					"currency":        tc.currency,
					"amount_received": tc.received,
					"metadata":        map[string]string{"payment_id": tc.paymentID},
				})
			})

			req := base
			req.ChannelData = `{"PaymentIntentId":"pi_1"}`
			_, err := d.RefundPayment(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("refund = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRefundAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{
		{"charge_already_refunded", driver.ErrPaymentRefundedFully},
		{"balance_insufficient", driver.ErrBalanceInsufficient},
		{"amount_too_small", driver.ErrTooSmallPaymentAmount},
		{"charge_disputed", driver.ErrUnallowed},
	} {
		t.Run(tc.code, func(t *testing.T) {
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_, _ = io.WriteString(w, `{"id":"pi_1","status":"succeeded","currency":"usd","amount_received":1000}`)
				} else {
					stripeError(w, 400, tc.code)
				}
			})

			_, err := d.RefundPayment(context.Background(), driver.CreateRefundRequest{
				PaymentId:    "pay_1",
				RefundId:     "refund_1",
				RefundAmount: 100,
				ChannelData:  `{"PaymentIntentId":"pi_1"}`,
			})
			// Exact comparison also checks the specific reason for ErrUnallowed variants.
			if tc.code == "charge_disputed" {
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v", err)
				}
			} else if err != tc.want {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRefundQueryFailureAndEmpty(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			pages := 0
			d := testDriver(t, func(w http.ResponseWriter, r *http.Request) {
				pages++
				if !fail {
					_, _ = io.WriteString(w, `{"object":"list","data":[],"has_more":false}`)
					return
				}
				if pages == 1 {
					_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"re_other"}],"has_more":true}`)
					return
				}
				stripeError(w, 403, "permission_denied")
			})

			_, ok, err := d.QueryRefund(context.Background(), driver.QueryRefundRequest{
				RefundId:    "missing",
				ChannelData: `{"PaymentIntentId":"pi_1"}`,
			})
			if ok || (err != nil) != fail {
				t.Fatalf("query = %v %v", ok, err)
			}
		})
	}
}

func TestRefundCompletionTimestamp(t *testing.T) {
	d := testDriver(t, nil)
	for _, tc := range []struct {
		event, previous string
		complete        bool
	}{
		{"refund.created", "", true},
		{"refund.updated", "pending", true},
		{"refund.updated", "succeeded", false},
		{"refund.updated", "", false},
	} {
		body, _ := json.Marshal(map[string]any{
			"id":          "evt_refund",
			"object":      "event",
			"type":        tc.event,
			"created":     1700000000,
			"api_version": sdk.APIVersion,
			"data": map[string]any{
				"object":              map[string]string{"id": "re_1", "status": "succeeded"},
				"previous_attributes": map[string]string{"status": tc.previous},
			},
		})

		payload := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
			Payload: body,
			Secret:  "whsec_local",
		})

		req := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(body)))
		req.Header.Set("Stripe-Signature", payload.Header)
		cb, err := d.ParseCallbackRequest(context.Background(), driver.CallbackRequest{Request: req})
		if err != nil {
			t.Fatal(err)
		}
		if cb.RefundInfo.RefundedAt.IsZero() == tc.complete {
			t.Fatalf("completion time for %s after %q: %v",
				tc.event, tc.previous, cb.RefundInfo.RefundedAt)
		}
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := testDriver(t, nil).CreatePayment(ctx, driver.CreatePaymentRequest{
		PaymentId:       "p",
		PaymentAmount:   100,
		PaymentCurrency: "USD",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v", err)
	}
}
