# The Stripe Payment Channel Driver Library

[![GoDoc](https://pkg.go.dev/badge/github.com/xgfone/go-payment-driver-stripe)](https://pkg.go.dev/github.com/xgfone/go-payment-driver-stripe)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg?style=flat-square)](https://raw.githubusercontent.com/xgfone/go-payment-driver-stripe/main/LICENSE)
![Minimum Go Version](https://img.shields.io/github/go-mod/go-version/xgfone/go-payment-driver-stripe?label=Go%2B)
![Latest SemVer](https://img.shields.io/github/v/tag/xgfone/go-payment-driver-stripe?sort=semver)

The `stripe_checkout` driver uses Stripe's hosted Checkout page for one-time payments.
It supports creating, querying, and canceling payments, creating and querying refunds,
and handling both payment and refund events through a unified webhook interface.
The code is organized into configuration, base driver, and implementation files,
using the same Builder pattern as the PayPal driver.

## Creating a Driver and a Payment

```go
package example

import (
    "context"
    "encoding/json"
    "os"

    stripe "github.com/xgfone/go-payment-driver-stripe"
    "github.com/xgfone/go-payment-driver/builder"
    "github.com/xgfone/go-payment-driver/driver"
)

func CreatePayment(ctx context.Context) (driver.PayLinkInfo, error) {
    config, err := json.Marshal(stripe.Config{
        SecretKey:          os.Getenv("STRIPE_SECRET_KEY"),
        WebhookSecret:      os.Getenv("STRIPE_WEBHOOK_SECRET"),
        SuccessURL:         "https://example.com/paid?session_id={CHECKOUT_SESSION_ID}",
        CancelURL:          "https://example.com/cancel",
        Currencies:         []string{"USD", "EUR"},
        PaymentMethodTypes: []string{"card"},
    })
    if err != nil {
        return driver.PayLinkInfo{}, err
    }

    d, err := builder.BuildDriver("stripe_checkout", string(config))
    if err != nil {
        return driver.PayLinkInfo{}, err
    }

    return d.CreatePayment(ctx, driver.CreatePaymentRequest{
        PaymentId:       "payment-20260919-001",
        PaymentDesc:     "Example order",
        PaymentCurrency: "USD",
        PaymentAmount:   1250, // USD 12.50
    })
}
```

`SecretKey`, `WebhookSecret`, `SuccessURL`, `CancelURL`, and `Currencies` are
required. `Currencies` lists the currencies allowed by the merchant. The payment
currency comes directly from the request's `PaymentCurrency` and must be in that
list; there is no default currency. Configured currency codes are normalized to
uppercase and sent to Stripe in lowercase. `PaymentMethodTypes` defaults to `card`.
Other methods must be enabled on the Stripe account and meet their regional,
currency, and other requirements. Use `sk_test_...` for testing and `sk_live_...`
for production; no separate Sandbox flag is needed.

`PayLink` is a Checkout URL that can be opened through a redirect or encoded
as a QR code. Its `LinkType` is `code_url`. Persist `ChannelPaymentId` (`cs_...`)
and `ChannelData`, and pass them to subsequent query, cancellation, and refund
requests. `ChannelPaymentId` always identifies the Checkout Session; it does not
change to a PaymentIntent ID after payment. `ChannelData` stores the Session,
PaymentIntent, and latest Charge IDs. Update the stored data when queries or
callbacks return new values.

Amounts use ISO 4217 minor units, as specified by the driver interface, and
remain integers without floating-point conversion. For example, pass 1250
for USD 12.50 or JPY 1250. The driver converts Stripe's special amount units
for ISK, UGX, and MGA. MGA amounts must represent whole units, so the requested
minor-unit amount must be a multiple of 100. Stripe still validates account
currency support, minimum payment amounts, and other restrictions.
See [Stripe's currency documentation](https://docs.stripe.com/currencies).

## Hosted Checkout Return Pages

Generic, mobile-friendly payment return page templates are available in
[`examples/checkout-pages`](examples/checkout-pages). They support English,
Simplified Chinese, and Traditional Chinese with shared wording for Hong Kong
and Taiwan. They follow the phone's language and allow manual selection.
The success template displays “Payment successful”; the cancel template displays
“Payment not completed”, since leaving Checkout does not establish a failed
charge. Customize the branding, copy, and merchant navigation for your application,
then set `SuccessURL` and `CancelURL` to the deployed HTTPS URLs. These static
pages do not verify payment status. The example README explains deployment,
backend confirmation, asynchronous payments, and cancellation behavior.

## Expiration and Idempotency

Stripe Checkout sessions can expire between 30 minutes and 24 hours after creation.
When `ExpiresIn == 0`, the driver uses Stripe's default of 24 hours instead of the generic
interface's 5-minute default. An explicit `ExpiresIn` outside this range returns `ErrBadRequest`.
See [Stripe's Checkout parameters](https://docs.stripe.com/api/checkout/sessions/create).

Payment creation uses `PaymentId` to generate an idempotency key, while refund
creation uses `RefundId` in a separate namespace. Keep business IDs, configuration,
and request parameters unchanged when retrying. Refund IDs should be globally
unique within a Stripe account. Stripe retains idempotency keys for at least 24
hours. After the retention period, do not rely on the same ID for deduplication;
query the persisted channel ID first.
See [Stripe's idempotent request documentation](https://docs.stripe.com/api/idempotent_requests).

For a custom expiration that supports retries across driver calls, generate and
persist an absolute deadline before the first creation attempt. Pass it through
`ExtInfo` as `stripe.CheckoutOptions`, a pointer to it, or a `json.RawMessage`
containing its JSON representation:

```go
// Persist expiresAt with the order and reuse it on retries; do not recalculate it.
expiresAt := time.Now().Add(time.Hour)
req := driver.CreatePaymentRequest{
    PaymentId:       "payment-20260919-002",
    PaymentAmount:   1250,
    PaymentCurrency: "USD",
    ExtInfo:         stripe.CheckoutOptions{ExpiresAt: expiresAt},
}
```

When using Payment's JSON API, pass the options through `Extra`, for example
`"Extra": {"ExpiresAt": "2026-09-19T18:00:00+08:00"}`. Choose a deadline within
Stripe's allowed expiration range. The driver decodes `json.RawMessage` into
`CheckoutOptions` before applying the same expiration rules. `ExpiresAt` uses
RFC 3339 format. Empty, `{}`, or `null` JSON options provide no `ExpiresAt`
override, so the existing `ExpiresIn` behavior applies. Invalid JSON or an
invalid timestamp returns `ErrBadRequest`.

`CheckoutOptions.ExpiresAt` takes precedence over `ExpiresIn`. Stripe validates it
against the original creation time. Using a nonzero `ExpiresIn` directly recalculates
the absolute deadline on each call, so retries across calls may trigger a Stripe
idempotency conflict because the parameters have changed. The driver does not
automatically create another payment by switching idempotency keys. SDK network
retries within a single call reuse the same parameters.

## Payment and Refund Statuses

| Stripe status                                            | Driver status                                                         |
| -------------------------------------------------------- | --------------------------------------------------------------------- |
| Checkout open / unpaid                                   | Processing, including failed attempts that can be retried in Checkout |
| Checkout complete / unpaid, asynchronous payment pending | Processing                                                            |
| Checkout paid, or complete / no_payment_required         | Success                                                               |
| Checkout expired                                         | Closed                                                                |
| checkout.session.async_payment_failed                    | Failure                                                               |
| Refund pending / requires_action                         | Processing                                                            |
| Refund succeeded                                         | Success                                                               |
| Refund failed / canceled                                 | Failure                                                               |

`CancelPayment` expires a Checkout Session that is still open. It returns `driver.ErrPaid`
for a paid session, nil for an expired or nonexistent session, and `driver.ErrUnallowed`
for a completed session with an asynchronous payment still pending. Stripe does not allow
canceling a Checkout payment by canceling its PaymentIntent.
See [Stripe's session expiration API](https://docs.stripe.com/api/checkout/sessions/expire).

Refund requests require `PaymentId`, `RefundId`, `RefundAmount`, and the persisted
`ChannelPaymentId` / `ChannelData`. The driver resolves the PaymentIntent and validates
the successful payment status, currency, and amount paid. The original `RefundReason`
is stored in metadata; the reason sent to Stripe is `requested_by_customer`. Acceptance
of a refund request does not necessarily mean the refund is complete. Continue querying
or wait for a callback. A fully refunded payment, insufficient balance, and a disallowed
refund map to `ErrPaymentRefundedFully`, `ErrBalanceInsufficient`, and `ErrUnallowed`,
respectively.

Persist the returned `ChannelRefundId` (`re_...`) to query a refund directly.
Without that ID, `QueryRefund` paginates through refunds for the payment's
PaymentIntent and matches `RefundId`. This can recover a refund whose creation
response was lost. Queries for nonexistent payments or refunds return
`ok == false, err == nil`. `IsRefunded` is true only when the Charge
has been fully refunded; partial refunds do not set it.

## Webhooks

Configure a webhook for **Your account** using **snapshot events**. Use an API
version that matches the SDK's `stripe.APIVersion` (`2026-08-26.dahlia` for
v86.4.2). Subscribe to **all seven events below** for this driver's payment and
refund lifecycle, including the asynchronous events even if the channel
currently only accepts cards:

| Required event                             | How the driver uses it                                                                                  |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------- |
| `checkout.session.completed`               | Reads the completed Checkout Session's payment status; an asynchronous payment can still be Processing. |
| `checkout.session.async_payment_succeeded` | Updates the payment after an asynchronous payment succeeds.                                             |
| `checkout.session.async_payment_failed`    | Marks an unsuccessful asynchronous payment as Failure.                                                  |
| `checkout.session.expired`                 | Marks an expired Checkout Session as Closed.                                                            |
| `refund.created`                           | Records the initial refund status, which may already be Success.                                        |
| `refund.updated`                           | Updates the refund status as it changes.                                                                |
| `refund.failed`                            | Marks the refund as Failure.                                                                            |

`payment_intent.*` and `charge.*` subscriptions are not required by this driver;
those events are not handled as separate payment or refund callbacks.

When integrating with Payment, use
`https://<payment-domain>/payment/v1/notify/<channel-code>` as the webhook URL.
`<channel-code>` is the configured channel's Code, not the `stripe_checkout`
driver type. Do not use `SuccessURL` or `CancelURL` as the webhook URL.

Set `WebhookSecret` to the endpoint's `whsec_...` signing secret. The Stripe CLI
forwarding secret differs from the endpoint secret shown in the Dashboard.
For separate live environments, register one endpoint for each environment,
subscribe each endpoint to the same seven events, and configure each environment
with its own endpoint's signing secret. Endpoints on the same Stripe account
receive matching account events regardless of which environment created the
order; the application must identify and acknowledge events for unrelated orders.
`CreatePaymentRequest.CallbackUrl` and `CreateRefundRequest.CallbackUrl` are not used
in Stripe requests. See [Stripe's webhook documentation](https://docs.stripe.com/webhooks).

```go
func webhookHandler(d driver.Driver, persist func(context.Context, driver.CallbackInfo) error) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        info, err := d.ParseCallbackRequest(r.Context(), driver.CallbackRequest{Request: r})
        if err == nil && info.Type != "" {
            err = persist(r.Context(), info)
        }
        d.SendCallbackResponse(r.Context(), w, err)
    }
}
```

Pass the unmodified request body to the driver. Do not let JSON middleware consume or
re-encode it first. The driver limits the body to 1 MiB and uses the SDK to validate
the signature, signature timestamp, and API version compatibility. Valid but unrelated
events return an empty `CallbackInfo` and should be acknowledged normally. For example,
`payment_intent.*` events are not processed as separate payments. Checkout events for
flows other than one-time payments are also ignored. Successful processing returns
HTTP 200. Signature verification or business persistence failures return HTTP 500
so Stripe can retry delivery.

Webhooks may arrive more than once or out of order. The application must process
payment and refund IDs idempotently and prevent stale Processing events from
overwriting confirmed success states. Query the current status when necessary.
A browser visiting `SuccessURL` is not proof of payment.

Successful payment events provide `PayerPaidAt`. Refund events provide `RefundedAt`
when a refund is created in the succeeded state or an update changes its status to
succeeded. Query responses do not provide a reliable completion time, so these fields
remain zero-valued. The driver does not treat Session, PaymentIntent, Charge,
or Refund creation times as completion times. Do not overwrite a persisted
completion time with a zero value when updating local records.

The current implementation supports one-time Checkout payments for the account itself.
It does not include subscriptions, Stripe Connect profit sharing, or standalone
PaymentIntent/Elements flows. `Share == true` returns `ErrUnsupported`.

## Sandbox Testing

For Checkout card payments, no separate test buyer account is required. Enter
Stripe's test card details directly on the hosted payment page. Sandbox payments
do not move real funds; use test cards rather than real card details. See
[Stripe's official test card documentation](https://docs.stripe.com/testing).

1. Set `SecretKey` to the current Sandbox's `sk_test_...` API key.
2. Register the webhook in the same Sandbox using the [seven required events](#webhooks).
   Set `WebhookSecret` to that endpoint's `whsec_...` signing secret. For Payment,
   use `https://<payment-domain>/payment/v1/notify/<channel-code>` as the endpoint.
3. Create a payment through your registered channel, then open the returned
   `PayLink` directly or scan a QR code containing it. Choose card payment.
4. Enter one of the test cards below and submit the payment. For 3D Secure,
   follow the simulated authentication prompts.

| Scenario                 | Test card number      | Expected behavior                                      |
| ------------------------ | --------------------- | ------------------------------------------------------ |
| Successful Visa payment  | `4242 4242 4242 4242` | Payment succeeds.                                      |
| Insufficient funds       | `4000 0000 0000 9995` | Payment attempt is declined with `insufficient_funds`. |
| 3D Secure authentication | `4000 0027 6000 3184` | Every payment requires authentication.                 |

Use these values for the remaining fields when requested:

| Field           | Example value                              |
| --------------- | ------------------------------------------ |
| Expiration date | `12/34`, or another valid future date      |
| CVC             | `123`, or any three digits for these cards |
| Cardholder name | `Test User`                                |
| Email           | `test@example.com`                         |

For more scenarios, see Stripe's documentation on
[declined payments](https://docs.stripe.com/testing#declined-payments) and
[3D Secure testing](https://docs.stripe.com/testing#regulatory-cards).

After a successful card payment, check the return page, the backend's payment
status, and the `checkout.session.completed` webhook delivery. Confirm that the
webhook receives HTTP 200 and that the application records Success. A visit to
`SuccessURL` alone is not payment verification. Test and live webhook signing
secrets are different; use the secret belonging to the Sandbox endpoint.
See [Stripe's webhook documentation](https://docs.stripe.com/webhooks).

A declined card attempt does not necessarily finish the order: Checkout allows
another attempt, so the driver keeps an open, unpaid Session in Processing.
Likewise, following `CancelURL` does not call `CancelPayment` or prove that a
charge failed. These are expected behaviors when testing the result pages.

## Verification

```sh
go test -race ./...
go vet ./...
```

Tests simulate Stripe responses through a local HTTP transport and use the real
SDK's serialization and signature utilities to verify requests and callbacks.
No Stripe credentials or network access are required. Before going live, verify
your enabled payment methods and webhook configuration in your own Stripe test
account.
