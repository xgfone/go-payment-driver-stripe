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

// Package stripe implements Stripe hosted Checkout for go-payment-driver.
package stripe

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const Type = "stripe"

type Config struct {
	WebhookSecret string

	SecretKey  string
	SuccessURL string
	CancelURL  string

	// Currencies is the required merchant currency allowlist, not Stripe's global list.
	Currencies []string

	// Defaults to card. Methods must be enabled on the Stripe account.
	PaymentMethodTypes []string `json:",omitempty"`
}

func (c *Config) Init() error { return c.init() }

func (c *Config) init() error {
	if strings.TrimSpace(c.SecretKey) == "" {
		return errors.New("missing SecretKey")
	}
	if strings.TrimSpace(c.WebhookSecret) == "" {
		return errors.New("missing WebhookSecret")
	}

	for _, field := range []struct{ name, value string }{
		{"SuccessURL", c.SuccessURL},
		{"CancelURL", c.CancelURL},
	} {
		u, err := url.Parse(field.value)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("%s must be an absolute HTTP(S) URL", field.name)
		}
	}

	if len(c.Currencies) == 0 {
		return errors.New("missing Currencies")
	}

	c.Currencies = slices.Clone(c.Currencies)
	for i, code := range c.Currencies {
		code = strings.ToUpper(strings.TrimSpace(code))
		if !validCurrency(code) {
			return fmt.Errorf("invalid currency %q", code)
		}
		c.Currencies[i] = code
	}

	c.PaymentMethodTypes = slices.Clone(c.PaymentMethodTypes)
	if len(c.PaymentMethodTypes) == 0 {
		c.PaymentMethodTypes = []string{"card"}
	}

	for i, method := range c.PaymentMethodTypes {
		method = strings.ToLower(strings.TrimSpace(method))
		if method == "" {
			return errors.New("empty payment method type")
		}
		c.PaymentMethodTypes[i] = method
	}

	return nil
}

func validCurrency(code string) bool {
	return len(code) == 3 && code[0] >= 'A' && code[0] <= 'Z' &&
		code[1] >= 'A' && code[1] <= 'Z' && code[2] >= 'A' && code[2] <= 'Z'
}

// Desensitize hides credentials in configuration returned for display.
func (c *Config) Desensitize() {
	c.WebhookSecret = "***"
	c.SecretKey = "***"
}

// CheckoutOptions may be supplied as CreatePaymentRequest.ExtInfo as a value,
// a pointer, or JSON encoded in json.RawMessage.
type CheckoutOptions struct {
	// ExpiresAt overrides ExpiresIn. Persist and reuse this absolute deadline
	// when retrying creation with a custom expiry, so idempotent parameters match.
	// Stripe requires 30 minutes to 24 hours after session creation.
	ExpiresAt time.Time
}

// ChannelData must be persisted along with ChannelPaymentId. The latter always
// identifies a Checkout Session; PaymentIntentId is used for refunds.
type ChannelData struct {
	CheckoutSessionId string `json:",omitempty"`
	PaymentIntentId   string `json:",omitempty"`
	LatestChargeId    string `json:",omitempty"`
}
