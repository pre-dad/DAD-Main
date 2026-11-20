package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"
)

type FinancialGateway interface {
	RetrievePaymentIntent(context.Context, string) (Object, error)
	RetrievePaymentMethod(context.Context, string) (Object, error)
	RetrieveRefund(context.Context, string) (Object, error)
	RetrieveSubscription(context.Context, string) (Object, error)
}

type PaymentMethodGateway interface {
	FinancialGateway
	RetrieveCheckout(context.Context, string) (Object, error)
	RetrieveSetupIntent(context.Context, string) (Object, error)
	UpdateSubscriptionPaymentMethod(context.Context, string, string, string) error
}

type SubscriptionGateway interface {
	PaymentMethodGateway
	CheckoutGateway
	ScheduleCancellation(context.Context, string, int64, string) error
	ChangeMonthlyAmount(context.Context, string, string, string, int64, string) error
}

// ReversalGateway undoes a checkout that completed after its donor deleted
// the account.
type ReversalGateway interface {
	RetrieveSubscription(context.Context, string) (Object, error)
	RefundAfterDeletion(context.Context, string, int64, string) error
	CancelSubscriptionNow(context.Context, string, string) error
}

type StaffFinanceGateway interface {
	SubscriptionGateway
	ListPaymentRefunds(context.Context, string) ([]Object, error)
	CreateRefund(context.Context, string, int64, string) (Object, error)
}

// ReconcileGateway lists the charges and refunds Stripe created in a window,
// read-only, so the daily reconciliation job can diff them against the
// database without any mutating call. A fake satisfies it in tests.
type ReconcileGateway interface {
	ListInvoicePayments(context.Context, string, string) ([]Object, error)
	ListChargesByCreated(context.Context, time.Time, time.Time) ([]Object, error)
	ListRefundsByCreated(context.Context, time.Time, time.Time) ([]Object, error)
	RetrievePaymentIntent(context.Context, string) (Object, error)
	RetrieveRefund(context.Context, string) (Object, error)
}

func (g *StripeGateway) ListInvoicePayments(ctx context.Context, invoice, payment string) ([]Object, error) {
	if (invoice == "") == (payment == "") {
		return nil, ErrFacts
	}
	if err := g.acquire(ctx, BucketReconcile); err != nil {
		return nil, err
	}
	params := &stripe.InvoicePaymentListParams{Status: stripe.String("paid"), ListParams: stripe.ListParams{Limit: stripe.Int64(100)}}
	if invoice != "" {
		params.Invoice = stripe.String(invoice)
	} else {
		params.Payment = &stripe.InvoicePaymentListPaymentParams{Type: stripe.String("payment_intent"), PaymentIntent: stripe.String(payment)}
	}
	page := g.client.V1InvoicePayments.List(ctx, params)
	if err := page.Err(); err != nil {
		return nil, processorError(err)
	}
	raw, err := rawObject(page.LastResponse())
	if err != nil || !raw.BoolIs("has_more", false) || raw.Absent("data") {
		return nil, ErrFacts
	}
	rows, err := raw.Array("data")
	if err != nil || len(rows) > 100 {
		return nil, ErrFacts
	}
	for i, row := range rows {
		if !row.BoolIs("livemode", false) {
			return nil, ErrFacts
		}
		facts := Object{}
		for _, key := range []string{"id", "invoice", "amount_paid", "currency", "status", "livemode", "created"} {
			facts[key] = row[key]
		}
		payment, err := row.Object("payment")
		if err != nil || row.Text("invoice") == "" || payment.Text("payment_intent") == "" {
			return nil, ErrFacts
		}
		reference := Object{"type": payment["type"], "payment_intent": payment["payment_intent"]}
		facts["payment"], err = json.Marshal(reference)
		if err != nil {
			return nil, ErrFacts
		}
		rows[i] = facts
	}
	return rows, nil
}

func ReconciliationFacts(o Object) Object {
	out := Object{}
	for _, key := range []string{"id", "payment_intent", "amount", "amount_captured", "currency", "status", "paid", "captured", "livemode", "created"} {
		if value, ok := o[key]; ok {
			out[key] = value
		}
	}
	return out
}

// StripeGateway calls Stripe in test mode. A failed call is reported as
// ErrProcessorUnavailable or a *ProcessorError, never as the SDK error, which
// can carry donor inputs.
// Every call first spends from budget.
type StripeGateway struct {
	client *stripe.Client
	budget Budget
}

func NewStripeGateway(key string) (*StripeGateway, error) {
	if key == "" {
		return nil, ErrProcessorUnavailable
	}
	if !strings.HasPrefix(key, "sk_test_") {
		return nil, errors.New("this release accepts only Stripe test keys")
	}
	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		HTTPClient: &http.Client{Timeout: 15 * time.Second}, MaxNetworkRetries: stripe.Int64(2),
		// Provider errors may contain donor inputs. Application logs record operation
		// classes separately, never the SDK's raw request/error diagnostics.
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	return &StripeGateway{client: stripe.NewClient(key, stripe.WithBackends(backends)), budget: DefaultRateBudget()}, nil
}

// processorError classifies an SDK failure. Network failures, timeouts, 408,
// 409 (a request in flight under the same idempotency key), 429 and 5xx may
// succeed when the same request is repeated, and map to ErrProcessorUnavailable.
// Any other 4xx is a verdict on the request itself, and maps to a
// *ProcessorError that keeps only the status, error code and request ID.
func processorError(err error) error {
	var api *stripe.Error
	if !errors.As(err, &api) {
		return ErrProcessorUnavailable
	}
	switch status := api.HTTPStatusCode; {
	case status < 400 || status >= 500,
		status == http.StatusRequestTimeout,
		status == http.StatusConflict,
		status == http.StatusTooManyRequests:
		return ErrProcessorUnavailable
	default:
		return &ProcessorError{Status: status, Code: string(api.Code), RequestID: api.RequestID}
	}
}

// UseBudget replaces the process-local budget the gateway starts with, such as
// with one shared by every replica.
func (g *StripeGateway) UseBudget(budget Budget) { g.budget = budget }

func (g *StripeGateway) CreateCheckout(ctx context.Context, raw []byte, key string) (RemoteCheckout, error) {
	var params stripe.CheckoutSessionCreateParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	// Refuse an old/unknown saved parameter rather than silently dropping it and
	// changing an idempotent request. Such attempts require reconciliation.
	if err := decoder.Decode(&params); err != nil {
		return RemoteCheckout{}, fmt.Errorf("%w: saved parameters are not accepted", ErrCheckoutRejected)
	}
	params.SetIdempotencyKey(key)
	if err := g.acquire(ctx, BucketCheckout); err != nil {
		return RemoteCheckout{}, err
	}
	session, err := g.client.V1CheckoutSessions.Create(ctx, &params)
	if err != nil {
		return RemoteCheckout{}, processorError(err)
	}
	var result RemoteCheckout
	if session.LastResponse == nil || json.Unmarshal(session.LastResponse.RawJSON, &result) != nil {
		return result, ErrProcessorUnavailable
	}
	return result, nil
}

// ExpireCheckout expires an open Checkout Session. If Stripe refuses because
// the session is no longer open, the session is read back so a payment that
// won the race is reported as "complete" rather than as a failure.
func (g *StripeGateway) ExpireCheckout(ctx context.Context, sessionID, key string) (string, error) {
	params := &stripe.CheckoutSessionExpireParams{}
	params.SetIdempotencyKey(key)
	if err := g.acquire(ctx, BucketCheckout); err != nil {
		return "", err
	}
	session, err := g.client.V1CheckoutSessions.Expire(ctx, sessionID, params)
	if err != nil {
		if err = g.acquire(ctx, BucketCheckout); err != nil {
			return "", err
		}
		session, err = g.client.V1CheckoutSessions.Retrieve(ctx, sessionID, nil)
		if err != nil {
			return "", processorError(err)
		}
	}
	raw, err := rawObject(session.LastResponse)
	if err != nil || !raw.BoolIs("livemode", false) || raw.Text("id") != sessionID {
		return "", ErrProcessorUnavailable
	}
	if status := raw.Text("status"); status == "expired" || status == "complete" {
		return status, nil
	}
	return "", ErrProcessorUnavailable
}

// Recurring changes never prorate: a new amount or cancellation takes
// effect at a renewal boundary.
func (g *StripeGateway) mutateSubscription(ctx context.Context, id string, params *stripe.SubscriptionUpdateParams, key string) error {
	params.ProrationBehavior = stripe.String("none")
	params.SetIdempotencyKey(key)
	if err := g.acquire(ctx, BucketStaff); err != nil {
		return err
	}
	if _, err := g.client.V1Subscriptions.Update(ctx, id, params); err != nil {
		return processorError(err)
	}
	return nil
}

func (g *StripeGateway) ScheduleCancellation(ctx context.Context, id string, end int64, key string) error {
	return g.mutateSubscription(ctx, id, &stripe.SubscriptionUpdateParams{CancelAt: stripe.Int64(end)}, key)
}

func (g *StripeGateway) ChangeMonthlyAmount(ctx context.Context, id, item, product string, amount int64, key string) error {
	price := &stripe.SubscriptionUpdateItemPriceDataParams{
		Currency:   stripe.String("usd"),
		Product:    stripe.String(product),
		Recurring:  &stripe.SubscriptionUpdateItemPriceDataRecurringParams{Interval: stripe.String("month")},
		UnitAmount: stripe.Int64(amount),
	}
	return g.mutateSubscription(
		ctx,
		id,
		&stripe.SubscriptionUpdateParams{
			Items: []*stripe.SubscriptionUpdateItemParams{{ID: stripe.String(item), Quantity: stripe.Int64(1), PriceData: price}},
		},
		key,
	)
}

func (g *StripeGateway) UpdateSubscriptionPaymentMethod(ctx context.Context, id, method, key string) error {
	return g.mutateSubscription(ctx, id, &stripe.SubscriptionUpdateParams{DefaultPaymentMethod: stripe.String(method)}, key)
}

// acquire spends a token from the named bucket before an SDK call, blocking
// until one is free or ctx is done. A gateway built directly in a test may have
// no budget; that path is unlimited.
func (g *StripeGateway) acquire(ctx context.Context, bucket string) error {
	if g.budget == nil {
		return ctx.Err()
	}
	return g.budget.Acquire(ctx, bucket)
}

// rawObject returns the JSON the processor sent for a call, so callers verify
// the processor's own fields rather than the SDK's typed view of them.
func rawObject(response *stripe.APIResponse) (Object, error) {
	if response == nil {
		return nil, ErrProcessorUnavailable
	}
	return DecodeObject(response.RawJSON)
}

func (g *StripeGateway) RetrieveCheckout(ctx context.Context, id string) (Object, error) {
	if err := g.acquire(ctx, BucketWorker); err != nil {
		return nil, err
	}
	result, err := g.client.V1CheckoutSessions.Retrieve(ctx, id, nil)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

func (g *StripeGateway) RetrieveSetupIntent(ctx context.Context, id string) (Object, error) {
	if err := g.acquire(ctx, BucketWorker); err != nil {
		return nil, err
	}
	result, err := g.client.V1SetupIntents.Retrieve(ctx, id, nil)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

// RetrievePaymentIntent expands the latest charge, whose creation is when a
// card payment succeeded.
func (g *StripeGateway) RetrievePaymentIntent(ctx context.Context, id string) (Object, error) {
	params := &stripe.PaymentIntentRetrieveParams{}
	params.AddExpand("latest_charge")
	if err := g.acquire(ctx, BucketWorker); err != nil {
		return nil, err
	}
	result, err := g.client.V1PaymentIntents.Retrieve(ctx, id, params)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

// RetrievePaymentMethod reads a payment method, whose card fingerprint names
// the card across customers.
func (g *StripeGateway) RetrievePaymentMethod(ctx context.Context, id string) (Object, error) {
	if err := g.acquire(ctx, BucketWorker); err != nil {
		return nil, err
	}
	result, err := g.client.V1PaymentMethods.Retrieve(ctx, id, nil)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

func (g *StripeGateway) RetrieveRefund(ctx context.Context, id string) (Object, error) {
	if err := g.acquire(ctx, BucketWorker); err != nil {
		return nil, err
	}
	result, err := g.client.V1Refunds.Retrieve(ctx, id, nil)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

func (g *StripeGateway) RetrieveSubscription(ctx context.Context, id string) (Object, error) {
	if err := g.acquire(ctx, BucketWorker); err != nil {
		return nil, err
	}
	result, err := g.client.V1Subscriptions.Retrieve(ctx, id, nil)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

func (g *StripeGateway) CreateRefund(ctx context.Context, payment string, amount int64, command string) (Object, error) {
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(payment),
		Amount:        stripe.Int64(amount),
		Reason:        stripe.String("requested_by_customer"),
		Metadata:      map[string]string{"dad_staff_action": command},
	}
	params.SetIdempotencyKey("dad-staff-refund-" + command)
	if err := g.acquire(ctx, BucketStaff); err != nil {
		return nil, err
	}
	result, err := g.client.V1Refunds.Create(ctx, params)
	if err != nil {
		return nil, processorError(err)
	}
	return rawObject(result.LastResponse)
}

// RefundAfterDeletion refunds a payment taken after its donor deleted the
// account. reversal names the queued reversal, so a retry is the same request.
func (g *StripeGateway) RefundAfterDeletion(ctx context.Context, payment string, amount int64, reversal string) error {
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(payment),
		Amount:        stripe.Int64(amount),
		Reason:        stripe.String("requested_by_customer"),
		Metadata:      map[string]string{"dad_deleted_account_reversal": reversal},
	}
	params.SetIdempotencyKey("dad-deleted-account-refund-" + reversal)
	if err := g.acquire(ctx, BucketStaff); err != nil {
		return err
	}
	if _, err := g.client.V1Refunds.Create(ctx, params); err != nil {
		return processorError(err)
	}
	return nil
}

// CancelSubscriptionNow ends a subscription immediately, without a final
// invoice or proration credit.
func (g *StripeGateway) CancelSubscriptionNow(ctx context.Context, id, key string) error {
	params := &stripe.SubscriptionCancelParams{InvoiceNow: stripe.Bool(false), Prorate: stripe.Bool(false)}
	params.SetIdempotencyKey(key)
	if err := g.acquire(ctx, BucketStaff); err != nil {
		return err
	}
	if _, err := g.client.V1Subscriptions.Cancel(ctx, id, params); err != nil {
		return processorError(err)
	}
	return nil
}

func (g *StripeGateway) ListPaymentRefunds(ctx context.Context, payment string) ([]Object, error) {
	return g.paginate(ctx, BucketStaff, 10, ErrProcessorUnavailable, func(after string) listPage {
		params := &stripe.RefundListParams{PaymentIntent: stripe.String(payment), ListParams: stripe.ListParams{Limit: stripe.Int64(100)}}
		if after != "" {
			params.StartingAfter = stripe.String(after)
		}
		return g.client.V1Refunds.List(ctx, params)
	}, func(row Object) (Object, error) { return row, nil })
}

// ListChargesByCreated lists every charge Stripe created in [start, end],
// read-only, for the daily reconciliation job. It rejects any live-mode
// object, so this test-only build never reconciles a live fact.
func (g *StripeGateway) ListChargesByCreated(ctx context.Context, start, end time.Time) ([]Object, error) {
	return g.listByCreated(ctx, start, end, func(ctx context.Context, created *stripe.RangeQueryParams, after string) listPage {
		params := &stripe.ChargeListParams{CreatedRange: created, ListParams: stripe.ListParams{Limit: stripe.Int64(100)}}
		if after != "" {
			params.StartingAfter = stripe.String(after)
		}
		return g.client.V1Charges.List(ctx, params)
	})
}

// ListRefundsByCreated lists every refund Stripe created in [start, end],
// read-only, with the same discipline as ListChargesByCreated.
func (g *StripeGateway) ListRefundsByCreated(ctx context.Context, start, end time.Time) ([]Object, error) {
	return g.listByCreated(ctx, start, end, func(ctx context.Context, created *stripe.RangeQueryParams, after string) listPage {
		params := &stripe.RefundListParams{CreatedRange: created, ListParams: stripe.ListParams{Limit: stripe.Int64(100)}}
		if after != "" {
			params.StartingAfter = stripe.String(after)
		}
		return g.client.V1Refunds.List(ctx, params)
	})
}

// listPage is the subset of the SDK's V1List iterator the paging loop reads,
// satisfied by both V1List[*Charge] and V1List[*Refund].
type listPage interface {
	Err() error
	LastResponse() *stripe.APIResponse
}

// listByCreated lists the objects created in [start, end] for reconciliation,
// each of which must be livemode:false.
func (g *StripeGateway) listByCreated(
	ctx context.Context,
	start, end time.Time,
	list func(context.Context, *stripe.RangeQueryParams, string) listPage,
) ([]Object, error) {
	created := &stripe.RangeQueryParams{GreaterThanOrEqual: start.Unix(), LesserThanOrEqual: end.Unix()}
	// A sub-day slice should fit well within the cap; a slice that reaches it
	// returns ErrListPageCap so the caller halves the range and retries, rather
	// than silently truncating or failing a legitimately busy day.
	return g.paginate(ctx, BucketReconcile, listPageCap, ErrListPageCap, func(after string) listPage {
		return list(ctx, created, after)
	}, func(row Object) (Object, error) {
		if !row.BoolIs("livemode", false) {
			return nil, ErrFacts
		}
		return ReconciliationFacts(row), nil
	})
}

// paginate reads at most pages pages of 100 rows, following starting_after
// until has_more is false, and keeps each row as keep returns it. A page that
// does not advance the cursor is refused; a longer list returns exhausted.
func (g *StripeGateway) paginate(
	ctx context.Context,
	bucket string,
	pages int,
	exhausted error,
	list func(after string) listPage,
	keep func(Object) (Object, error),
) ([]Object, error) {
	result := []Object{}
	after := ""
	for range pages {
		if err := g.acquire(ctx, bucket); err != nil {
			return nil, err
		}
		page := list(after)
		if err := page.Err(); err != nil {
			return nil, processorError(err)
		}
		raw, err := rawObject(page.LastResponse())
		if err != nil {
			return nil, err
		}
		rows, err := raw.Array("data")
		if err != nil || len(rows) > 100 {
			return nil, ErrFacts
		}
		for _, row := range rows {
			kept, err := keep(row)
			if err != nil {
				return nil, err
			}
			result = append(result, kept)
		}
		if raw.BoolIs("has_more", false) {
			return result, nil
		}
		if !raw.BoolIs("has_more", true) || len(rows) == 0 {
			return nil, ErrFacts
		}
		last := rows[len(rows)-1].Text("id")
		if last == "" || last == after {
			return nil, ErrFacts
		}
		after = last
	}
	return nil, exhausted
}

// listPageCap bounds pages per listByCreated call. At 100 rows per page it caps
// one call near 100k objects, well above any sub-day slice, so hitting it means
// the time range should be split rather than that the data is unreadable.
const listPageCap = 1000
