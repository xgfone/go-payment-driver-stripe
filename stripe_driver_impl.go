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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
	"github.com/xgfone/go-payment-driver/builder"
	"github.com/xgfone/go-payment-driver/driver"
)

func init() {
	registerBuilder("checkout", func(b builder.Builder, c Config) (driver.Driver, error) {
		d, err := newDriver(b, c)
		if err != nil {
			return nil, err
		}
		return &Driver{_Driver: d}, nil
	})
}

type Driver struct{ _Driver }

// CheckoutDriver is the hosted Checkout driver.
type CheckoutDriver = Driver

var _ driver.Driver = (*Driver)(nil)

func (d *Driver) CreatePayment(ctx context.Context, req driver.CreatePaymentRequest) (info driver.PayLinkInfo, err error) {
	if req.PaymentId == "" || utf8.RuneCountInString(req.PaymentId) > 200 {
		return info, driver.ErrBadRequest.WithReason("PaymentId must contain 1 to 200 characters")
	}

	if req.Share {
		return info, driver.ErrUnsupported.WithReason("Stripe Checkout driver does not support profit sharing")
	}

	currency := strings.ToUpper(req.PaymentCurrency)
	if !d.metadata.CurrencyIsSupported(currency) {
		return info, driver.ErrUnsupported.WithReasonf("unsupported currency %q", currency)
	}

	amount, err := stripeAmount(req.PaymentAmount, currency)
	if err != nil {
		return info, err
	}

	expiresAt, err := checkoutExpiry(req)
	if err != nil {
		return info, err
	}

	metadata := map[string]string{"payment_id": req.PaymentId}
	params := &stripe.CheckoutSessionCreateParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),

		Metadata:  metadata,
		ExpiresAt: expiresAt,

		CancelURL:  stripe.String(d.config.CancelURL),
		SuccessURL: stripe.String(d.config.SuccessURL),

		ClientReferenceID: stripe.String(req.PaymentId),

		// Keep the requested currency and amount independent of account defaults.
		AdaptivePricing: &stripe.CheckoutSessionCreateAdaptivePricingParams{
			Enabled: stripe.Bool(false),
		},

		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Metadata: metadata,
		},

		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:   stripe.String(strings.ToLower(currency)),
				UnitAmount: stripe.Int64(amount),
				ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name: stripe.String(cmp.Or(req.PaymentDesc, req.PaymentId)),
				},
			},
		}},
	}

	for _, method := range d.config.PaymentMethodTypes {
		params.PaymentMethodTypes = append(params.PaymentMethodTypes, stripe.String(method))
	}

	params.SetIdempotencyKey(idempotencyKey("payment", req.PaymentId))
	session, err := d.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return info, wrapError(err)
	}
	if session.ID == "" || session.URL == "" {
		return info, errors.New("stripe: Checkout Session has no ID or payment URL")
	}

	return driver.PayLinkInfo{
		PayLink:          session.URL,
		ChannelPaymentId: session.ID,
		ChannelData:      driver.EncodeChannelData(sessionChannelData(session)),
	}, nil
}

func checkoutExpiry(req driver.CreatePaymentRequest) (*int64, error) {
	var options CheckoutOptions
	switch ext := req.ExtInfo.(type) {
	case nil:
	case CheckoutOptions:
		options = ext

	case *CheckoutOptions:
		if ext != nil {
			options = *ext
		}

	case json.RawMessage:
		if len(ext) > 0 {
			if err := json.Unmarshal(ext, &options); err != nil {
				return nil, driver.ErrBadRequest.WithReasonf("invalid CheckoutOptions JSON: %v", err)
			}
		}

	default:
		return nil, driver.ErrBadRequest.WithReason("ExtInfo must be CheckoutOptions, *CheckoutOptions, or json.RawMessage")
	}

	if !options.ExpiresAt.IsZero() {
		// Validate against the original creation time on Stripe. Checking against
		// now would reject valid idempotent retries of an existing session.
		return stripe.Int64(options.ExpiresAt.Unix()), nil
	}

	if req.ExpiresIn == 0 {
		return nil, nil // Stripe's default of 24 hours keeps retry parameters stable.
	} else if req.ExpiresIn < 30*time.Minute || req.ExpiresIn > 24*time.Hour {
		return nil, driver.ErrBadRequest.WithReason("Stripe Checkout ExpiresIn must be between 30 minutes and 24 hours")
	}

	return stripe.Int64(time.Now().Add(req.ExpiresIn).Unix()), nil
}

func (d *Driver) retrieveSession(ctx context.Context, channelID, data string) (*stripe.CheckoutSession, error) {
	cd := driver.DecodeChannelData[ChannelData](data)
	id := cmp.Or(channelID, cd.CheckoutSessionId)
	if id == "" {
		return nil, driver.ErrBadRequest.WithReason("missing Checkout Session ID")
	}

	params := &stripe.CheckoutSessionRetrieveParams{}
	params.AddExpand("payment_intent.latest_charge")
	return d.client.V1CheckoutSessions.Retrieve(ctx, id, params)
}

func (d *Driver) QueryPayment(ctx context.Context, req driver.QueryPaymentRequest) (info driver.PaymentInfo, ok bool, err error) {
	session, err := d.retrieveSession(ctx, req.ChannelPaymentId, req.ChannelData)
	if err != nil {
		if isStripeNotFound(err) {
			return info, false, nil
		}
		return info, false, wrapError(err)
	}

	info = sessionToPaymentInfo(session)
	info.PaymentId = cmp.Or(info.PaymentId, req.PaymentId)
	return info, true, nil
}

func (d *Driver) CancelPayment(ctx context.Context, req driver.CancelPaymentRequest) error {
	session, err := d.retrieveSession(ctx, req.ChannelPaymentId, req.ChannelData)
	if err != nil {
		if isStripeNotFound(err) {
			return nil
		}
		return wrapError(err)
	}

	if err, done := cancellationResult(session); done {
		return err
	}

	params := &stripe.CheckoutSessionExpireParams{}
	params.SetIdempotencyKey(idempotencyKey("expire", session.ID))
	_, err = d.client.V1CheckoutSessions.Expire(ctx, session.ID, params)
	if err == nil {
		return nil
	}

	// A payment or expiry may have completed between retrieve and expire.
	if current, queryErr := d.retrieveSession(ctx, session.ID, ""); queryErr == nil {
		if result, done := cancellationResult(current); done {
			return result
		}
	}

	return wrapError(err)
}

func cancellationResult(session *stripe.CheckoutSession) (error, bool) {
	if sessionToPaymentInfo(session).TaskStatus == driver.TaskStatusSuccess {
		return driver.ErrPaid, true
	}

	switch session.Status {
	case stripe.CheckoutSessionStatusExpired:
		return nil, true

	case stripe.CheckoutSessionStatusOpen:
		return nil, false

	default:
		return driver.ErrUnallowed.WithReason("only an open Checkout Session can be canceled"), true
	}
}

func (d *Driver) RefundPayment(ctx context.Context, req driver.CreateRefundRequest) (info driver.RefundInfo, err error) {
	if req.PaymentId == "" || req.RefundId == "" ||
		utf8.RuneCountInString(req.PaymentId) > 500 ||
		utf8.RuneCountInString(req.RefundId) > 500 {
		return info, driver.ErrBadRequest.WithReason("PaymentId and RefundId must contain 1 to 500 characters")
	}

	if req.RefundAmount <= 0 || (req.PaymentAmount > 0 && req.RefundAmount > req.PaymentAmount) {
		return info, driver.ErrBadRequest.WithReason("invalid refund amount")
	}

	if utf8.RuneCountInString(req.RefundReason) > 500 {
		return info, driver.ErrBadRequest.WithReason("RefundReason exceeds 500 characters")
	}

	cd, err := d.resolvePaymentIntent(ctx, req.ChannelPaymentId, req.ChannelData)
	if err != nil {
		if isStripeNotFound(err) {
			return info, driver.ErrUnallowed.WithReason("payment not found")
		}
		return info, wrapError(err)
	}

	if cd.PaymentIntentId == "" {
		return info, driver.ErrUnallowed.WithReason("Checkout Session has no payment intent")
	}

	pi, err := d.client.V1PaymentIntents.Retrieve(ctx, cd.PaymentIntentId, nil)
	if err != nil {
		return info, wrapError(err)
	}

	if pi.Status != stripe.PaymentIntentStatusSucceeded {
		return info, driver.ErrUnallowed.WithReason("payment has not succeeded")
	}

	if req.PaymentCurrency != "" && !strings.EqualFold(req.PaymentCurrency, string(pi.Currency)) {
		return info, driver.ErrBadRequest.WithReason("refund currency does not match the payment")
	}

	if id := pi.Metadata["payment_id"]; id != "" && id != req.PaymentId {
		return info, driver.ErrBadRequest.WithReason("PaymentId does not match the payment intent")
	}

	amount, err := stripeAmount(req.RefundAmount, string(pi.Currency))
	if err != nil {
		return info, err
	}
	if amount > pi.AmountReceived {
		return info, driver.ErrUnallowed.WithReason("refund amount exceeds paid amount")
	}

	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(pi.ID),
		Amount:        stripe.Int64(amount),
		Metadata: map[string]string{
			"payment_id":          req.PaymentId,
			"refund_id":           req.RefundId,
			"refund_reason":       req.RefundReason,
			"checkout_session_id": cd.CheckoutSessionId,
		},
	}

	if req.RefundReason != "" {
		params.Reason = stripe.String(string(stripe.RefundReasonRequestedByCustomer))
	}

	params.SetIdempotencyKey(idempotencyKey("refund", req.RefundId))
	refund, err := d.client.V1Refunds.Create(ctx, params)
	if err != nil {
		var e *stripe.Error
		mapped := wrapError(err)
		if errors.As(err, &e) && e.Type == stripe.ErrorTypeInvalidRequest &&
			e.Code != stripe.ErrorCodeBalanceInsufficient &&
			e.Code != stripe.ErrorCodeChargeAlreadyRefunded &&
			e.Code != stripe.ErrorCodeAmountTooSmall {
			mapped = driver.ErrUnallowed.WithReason(e.Msg)
		}
		return info, mapped
	}

	return refundToRefundInfo(refund), nil
}

func (d *Driver) resolvePaymentIntent(ctx context.Context, channelID, data string) (ChannelData, error) {
	cd := driver.DecodeChannelData[ChannelData](data)
	if channelID != "" && cd.CheckoutSessionId != "" && channelID != cd.CheckoutSessionId {
		return cd, driver.ErrBadRequest.WithReason("ChannelPaymentId does not match ChannelData")
	}

	cd.CheckoutSessionId = cmp.Or(channelID, cd.CheckoutSessionId)
	if cd.PaymentIntentId != "" {
		return cd, nil
	}

	session, err := d.retrieveSession(ctx, channelID, data)
	if err != nil {
		return cd, err
	}

	return sessionChannelData(session), nil
}

func (d *Driver) QueryRefund(ctx context.Context, req driver.QueryRefundRequest) (info driver.RefundInfo, ok bool, err error) {
	if req.ChannelRefundId != "" {
		refund, err := d.client.V1Refunds.Retrieve(ctx, req.ChannelRefundId, nil)
		if err != nil {
			if isStripeNotFound(err) {
				return info, false, nil
			}
			return info, false, wrapError(err)
		}

		info = refundToRefundInfo(refund)
		info.PaymentId = cmp.Or(info.PaymentId, req.PaymentId)
		info.RefundId = cmp.Or(info.RefundId, req.RefundId)
		return info, true, nil
	}

	if req.RefundId == "" {
		return info, false, driver.ErrBadRequest.WithReason("missing RefundId or ChannelRefundId")
	}

	cd, err := d.resolvePaymentIntent(ctx, req.ChannelPaymentId, req.ChannelData)
	if err != nil {
		if isStripeNotFound(err) {
			return info, false, nil
		}
		return info, false, wrapError(err)
	}

	if cd.PaymentIntentId == "" {
		return info, false, nil
	}

	params := &stripe.RefundListParams{
		PaymentIntent: stripe.String(cd.PaymentIntentId),
	}
	params.Limit = stripe.Int64(100)
	for refund, err := range d.client.V1Refunds.List(ctx, params).All(ctx) {
		if err != nil {
			return info, false, wrapError(err)
		}

		if refund.Metadata["refund_id"] == req.RefundId {
			info = refundToRefundInfo(refund)
			info.PaymentId = cmp.Or(info.PaymentId, req.PaymentId)
			return info, true, nil
		}
	}

	return info, false, nil
}

const maxWebhookBody = 1 << 20

func (d *Driver) ParseCallbackRequest(_ context.Context, req driver.CallbackRequest) (cb driver.CallbackInfo, err error) {
	r := req.Request
	if r == nil || r.Body == nil {
		return cb, driver.ErrBadRequest.WithReason("missing webhook request body")
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		return cb, err
	}
	if len(payload) > maxWebhookBody {
		return cb, driver.ErrBadRequest.WithReason("Stripe webhook body exceeds 1 MiB")
	}

	// Keep both signature timestamp tolerance and API version compatibility checks.
	event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), d.config.WebhookSecret)
	if err != nil {
		return cb, driver.ErrBadRequest.WithReasonf("invalid Stripe webhook: %v", err)
	}
	if event.Data == nil {
		return cb, driver.ErrBadRequest.WithReason("missing Stripe event data")
	}

	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted,
		stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded,
		stripe.EventTypeCheckoutSessionAsyncPaymentFailed,
		stripe.EventTypeCheckoutSessionExpired:

		var session stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
			return cb, err
		}

		if session.ID == "" {
			return cb, driver.ErrBadRequest.WithReason("missing Checkout Session ID")
		}

		// This driver creates one-time payments, not subscriptions or setup sessions.
		if session.Mode != stripe.CheckoutSessionModePayment {
			return cb, nil
		}

		info := sessionToPaymentInfo(&session)
		if event.Type == stripe.EventTypeCheckoutSessionAsyncPaymentFailed &&
			info.TaskStatus != driver.TaskStatusSuccess {
			info.TaskStatus = driver.TaskStatusFailure
			info.FailReason = cmp.Or(info.FailReason, "asynchronous payment failed")
		}

		if info.TaskStatus == driver.TaskStatusSuccess && event.Created > 0 {
			info.PayerPaidAt = time.Unix(event.Created, 0)
		}

		cb.Type, cb.PaymentInfo = driver.CallbackTypePayment, &info

	case stripe.EventTypeRefundCreated,
		stripe.EventTypeRefundUpdated,
		stripe.EventTypeRefundFailed:

		var refund stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &refund); err != nil {
			return cb, err
		}

		if refund.ID == "" {
			return cb, driver.ErrBadRequest.WithReason("missing refund ID")
		}

		info := refundToRefundInfo(&refund)
		if event.Type == stripe.EventTypeRefundFailed {
			info.TaskStatus = driver.TaskStatusFailure
		}

		previousStatus, _ := event.Data.PreviousAttributes["status"].(string)
		// A later refund.updated may only add a bank reference. It is not a
		// new completion; only use an event that actually enters succeeded.
		completed := event.Type == stripe.EventTypeRefundCreated ||
			(previousStatus != "" && previousStatus != "succeeded")
		if info.TaskStatus == driver.TaskStatusSuccess && completed && event.Created > 0 {
			info.RefundedAt = time.Unix(event.Created, 0)
		}

		cb.Type, cb.RefundInfo = driver.CallbackTypeRefund, &info
	}

	// Unhandled, valid events are acknowledged to avoid unnecessary retries.
	return cb, nil
}

func (d *Driver) SendCallbackResponse(_ context.Context, rw http.ResponseWriter, err error) {
	if err != nil {
		// Business persistence failures must also cause Stripe to retry delivery.
		http.Error(rw, "Stripe webhook processing failed", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(`{"received":true}`))
}

func sessionChannelData(session *stripe.CheckoutSession) ChannelData {
	cd := ChannelData{CheckoutSessionId: session.ID}
	if pi := session.PaymentIntent; pi != nil {
		cd.PaymentIntentId = pi.ID
		if pi.LatestCharge != nil {
			cd.LatestChargeId = pi.LatestCharge.ID
		}
	}
	return cd
}

func sessionToPaymentInfo(session *stripe.CheckoutSession) driver.PaymentInfo {
	info := driver.PaymentInfo{
		PaymentId:        cmp.Or(session.ClientReferenceID, session.Metadata["payment_id"]),
		ChannelPaymentId: session.ID,
		ChannelStatus:    fmt.Sprintf("session_status=%s,payment_status=%s", session.Status, session.PaymentStatus),
		ChannelData:      driver.EncodeChannelData(sessionChannelData(session)),
		TaskStatus:       driver.TaskStatusUnknown,
	}

	switch {
	case session.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid:
		info.TaskStatus = driver.TaskStatusSuccess

	case session.Status == stripe.CheckoutSessionStatusComplete &&
		session.PaymentStatus == stripe.CheckoutSessionPaymentStatusNoPaymentRequired:
		info.TaskStatus = driver.TaskStatusSuccess

	case session.Status == stripe.CheckoutSessionStatusExpired:
		info.TaskStatus = driver.TaskStatusClosed

	case session.Status == stripe.CheckoutSessionStatusOpen,
		session.Status == stripe.CheckoutSessionStatusComplete:
		info.TaskStatus = driver.TaskStatusProcessing
	}

	if session.Customer != nil {
		info.PayerId = session.Customer.ID
	}
	if session.CustomerDetails != nil {
		info.PayerId = cmp.Or(info.PayerId, session.CustomerDetails.Email)
	}

	if pi := session.PaymentIntent; pi != nil {
		info.PaymentId = cmp.Or(info.PaymentId, pi.Metadata["payment_id"])
		if pi.LatestCharge != nil {
			info.IsRefunded = pi.LatestCharge.Refunded
		}
		if pi.LastPaymentError != nil && info.TaskStatus != driver.TaskStatusSuccess {
			info.FailReason = pi.LastPaymentError.Msg
		}

		// A failed attempt while Checkout is open can be retried by the buyer.
		if session.Status == stripe.CheckoutSessionStatusComplete &&
			info.TaskStatus != driver.TaskStatusSuccess {
			switch pi.Status {
			case stripe.PaymentIntentStatusCanceled:
				info.TaskStatus = driver.TaskStatusClosed
			case stripe.PaymentIntentStatusRequiresPaymentMethod:
				if pi.LastPaymentError != nil {
					info.TaskStatus = driver.TaskStatusFailure
				}
			}
		}
	}

	if info.TaskStatus == driver.TaskStatusSuccess {
		info.PayerPaidCurrency = strings.ToUpper(string(session.Currency))
		info.PayerPaidAmount = paidAmount(session.AmountTotal, info.PayerPaidCurrency)
		if p := session.PresentmentDetails; p != nil {
			info.PayerPaidCurrency = strings.ToUpper(string(p.PresentmentCurrency))
			info.PayerPaidAmount = paidAmount(p.PresentmentAmount, info.PayerPaidCurrency)
		}
	}

	// Session/intent/charge creation is not payment completion. Queries leave
	// PayerPaidAt unset; successful webhook events supply the completion time.
	return info
}

func refundToRefundInfo(refund *stripe.Refund) driver.RefundInfo {
	cd := ChannelData{CheckoutSessionId: refund.Metadata["checkout_session_id"]}
	if refund.PaymentIntent != nil {
		cd.PaymentIntentId = refund.PaymentIntent.ID
	}
	if refund.Charge != nil {
		cd.LatestChargeId = refund.Charge.ID
	}

	info := driver.RefundInfo{
		PaymentId:       refund.Metadata["payment_id"],
		RefundId:        refund.Metadata["refund_id"],
		RefundReason:    cmp.Or(refund.Metadata["refund_reason"], string(refund.Reason)),
		ChannelRefundId: refund.ID,
		ChannelStatus:   string(refund.Status),
		ChannelData:     driver.EncodeChannelData(cd),
		FailReason:      string(refund.FailureReason),
		TaskStatus:      driver.TaskStatusUnknown,
	}

	switch refund.Status {
	case stripe.RefundStatusSucceeded:
		info.TaskStatus = driver.TaskStatusSuccess

	case stripe.RefundStatusFailed, stripe.RefundStatusCanceled:
		info.TaskStatus = driver.TaskStatusFailure

	case stripe.RefundStatusPending, stripe.RefundStatusRequiresAction:
		info.TaskStatus = driver.TaskStatusProcessing
	}

	// Refund.Created is the request time, including for asynchronous refunds.
	return info
}
