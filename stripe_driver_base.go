// Copyright 2026 xgfone
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stripe

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/stripe/stripe-go/v86"
	"github.com/xgfone/go-payment-driver/builder"
	"github.com/xgfone/go-payment-driver/driver"
)

func registerBuilder(scene string, newf builder.DriverNewer[Config]) {
	metadata := driver.NewMetadata(Type, scene).WithLinkType(driver.LinkTypeCodeUrl)
	builder.Register(builder.New(newf, metadata))
}

func newDriver(b builder.Builder, c Config) (d _Driver, err error) {
	if err = c.init(); err != nil {
		return
	}

	metadata := b.Metadata()
	metadata.Currencies = c.Currencies
	metadata.Channels = c.PaymentMethodTypes
	d = _Driver{
		metadata: metadata,
		client:   stripe.NewClient(c.SecretKey),
		config:   c,
	}
	return
}

type _Driver struct {
	metadata driver.Metadata
	client   *stripe.Client
	config   Config
}

func (d *_Driver) Metadata() driver.Metadata {
	md := d.metadata
	md.Currencies = slices.Clone(md.Currencies)
	md.Channels = slices.Clone(md.Channels)
	return md
}

// Keep keys bounded and separate payment and refund namespaces. The same
// business ID must never be reused for a different operation or payment.
func idempotencyKey(operation, id string) string {
	return fmt.Sprintf("go-payment-driver-stripe:%s:%x", operation, sha256.Sum256([]byte(id)))
}

func isStripeNotFound(err error) bool {
	var e *stripe.Error
	return errors.As(err, &e) && (e.Code == stripe.ErrorCodeResourceMissing ||
		e.HTTPStatusCode == http.StatusNotFound)
}

func wrapError(err error) error {
	var e *stripe.Error
	if !errors.As(err, &e) {
		return err
	}

	// Codes take precedence: amount_too_small is also an invalid_request_error.
	switch e.Code {
	case stripe.ErrorCodeAmountTooSmall:
		return driver.ErrTooSmallPaymentAmount

	case stripe.ErrorCodeBalanceInsufficient:
		return driver.ErrBalanceInsufficient

	case stripe.ErrorCodeChargeAlreadyRefunded:
		return driver.ErrPaymentRefundedFully
	}

	switch e.Type {
	case stripe.ErrorTypeCard, stripe.ErrorTypeIdempotency, stripe.ErrorTypeInvalidRequest:
		return driver.ErrBadRequest.WithReason(e.Msg)

	default:
		return fmt.Errorf("stripe: %w", err)
	}
}

// The driver interface uses ISO minor units. Stripe has different units for
// ISK/UGX (two decimal API values) and MGA (zero decimal API values).
func stripeAmount(amount int64, currency string) (int64, error) {
	if amount <= 0 {
		return 0, driver.ErrBadRequest.WithReason("amount must be positive")
	}

	switch strings.ToUpper(currency) {
	case "ISK", "UGX":
		if amount > math.MaxInt64/100 {
			return 0, driver.ErrBadRequest.WithReason("amount overflows Stripe units")
		}
		amount *= 100

	case "MGA":
		if amount%100 != 0 {
			return 0, driver.ErrBadRequest.WithReason("Stripe requires whole MGA amounts")
		}
		amount /= 100
	}

	return amount, nil
}

func paidAmount(amount int64, currency string) int64 {
	switch strings.ToUpper(currency) {
	case "ISK", "UGX":
		return amount / 100

	case "MGA":
		return amount * 100

	default:
		return amount
	}
}
