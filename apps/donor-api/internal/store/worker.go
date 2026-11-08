package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type EventWorker struct {
	DB      *pgxpool.Pool
	Gateway payments.FinancialGateway
}

// eventLease keeps a claimed event from other workers while its attempt runs.
// It outlasts workers.ProcessorStepBudget, so a live attempt never loses its
// claim, while a crashed one is retried when the lease runs out.
const eventLease = 6 * time.Minute

// maxRefetch bounds how often one attempt re-reads the processor because its
// state moved while the donor was locked.
const maxRefetch = 2

// errRefetch reports that applying an event needs processor state newer than
// what was fetched. The apply rolls back and the attempt fetches again, since
// no processor call is made while a transaction is open.
var errRefetch = errors.New("processor state changed while applying the event")

// eventApply is an event's effects, prepared from processor state fetched with
// no transaction open. It runs under the donor lock and returns the donor whose
// projection to refresh.
type eventApply func(ctx context.Context, q *queries.Queries) (pgtype.UUID, error)

// ProcessNext applies the next due Stripe event. It claims the event with a
// lease, fetches processor state with no transaction open, then applies the
// event and marks it processed in one transaction that first confirms the
// attempt still holds the lease. A failure is recorded on the event, which is
// retried with backoff, rather than returned: only a failure to record the
// outcome stops the worker. It reports whether it found an event.
func (w EventWorker) ProcessNext(ctx context.Context) (bool, error) {
	event, found, err := w.claim(ctx, time.Now().UTC())
	if err != nil || !found {
		return false, err
	}
	if err = w.resolve(ctx, event); err != nil {
		return false, err
	}
	return true, nil
}

func (w EventWorker) claim(ctx context.Context, now time.Time) (queries.DonorStripeEvent, bool, error) {
	event, err := queries.New(w.DB).ClaimStripeEvent(ctx, queries.ClaimStripeEventParams{
		LeaseUntil: timestamp(now.Add(eventLease)), AsOf: timestamp(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return event, false, nil
	}
	return event, err == nil, err
}

// resolve settles a claimed event. A failure of the processor or of the event's
// facts is recorded on the event; an error is returned only when the outcome
// could not be recorded, which leaves the lease to expire.
func (w EventWorker) resolve(ctx context.Context, event queries.DonorStripeEvent) error {
	failure := errRefetch
	for range 1 + maxRefetch {
		now := time.Now().UTC()
		apply, err := w.fetch(ctx, event, now)
		if err != nil {
			failure = err
			break
		}
		if err = w.applyClaimed(ctx, event, apply, now); err == nil {
			return nil
		} else if !errors.Is(err, errRefetch) {
			return w.abandon(ctx, event, err)
		}
	}
	done, cancel := detach(ctx)
	defer cancel()
	if errors.Is(ctx.Err(), context.Canceled) {
		return w.release(done, event, ctx.Err())
	}
	_, err := queries.New(w.DB).FailStripeEvent(done, failParams(event, failure))
	return err
}

// abandon handles an error that left the event unrecorded. A cancelled step
// gives its claim back; otherwise the lease expires and the event is retried.
func (w EventWorker) abandon(ctx context.Context, event queries.DonorStripeEvent, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		done, cancel := detach(ctx)
		defer cancel()
		return w.release(done, event, err)
	}
	return err
}

// release returns an interrupted attempt's claim without counting the attempt,
// and reports cause.
func (w EventWorker) release(ctx context.Context, event queries.DonorStripeEvent, cause error) error {
	_, err := queries.New(w.DB).ReleaseStripeEvent(ctx, queries.ReleaseStripeEventParams{
		ID: event.ID, Attempts: event.Attempts, AvailableAt: event.AvailableAt, AsOf: timestamp(time.Now().UTC()),
	})
	return errors.Join(cause, err)
}

// detach returns a short context that outlives ctx's cancellation, so a step
// that ran out of time can still record why.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func failParams(event queries.DonorStripeEvent, err error) queries.FailStripeEventParams {
	return queries.FailStripeEventParams{
		ID: event.ID, LastError: text(eventFailure(err)), Attempts: event.Attempts, AvailableAt: event.AvailableAt,
	}
}

// applyClaimed applies a fetched event in one transaction, provided the attempt
// still holds the claim: if another worker reclaimed the event after the lease
// expired, or it was reset or completed, nothing is changed. The event's
// effects commit in a savepoint, so a failed event keeps its attempt count and
// error while its partial effects roll back.
func (w EventWorker) applyClaimed(ctx context.Context, event queries.DonorStripeEvent, apply eventApply, now time.Time) error {
	return pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		_, err := q.LockClaimedStripeEvent(ctx, queries.LockClaimedStripeEventParams{
			ID: event.ID, Attempts: event.Attempts, AvailableAt: event.AvailableAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		applyErr := pgx.BeginFunc(ctx, tx, func(work pgx.Tx) error {
			donor, err := apply(ctx, queries.New(work))
			if err != nil {
				return err
			}
			_, err = refreshProjection(ctx, work, donor, now)
			return err
		})
		switch {
		case errors.Is(applyErr, errRefetch):
			return applyErr
		case applyErr != nil:
			_, err = q.FailStripeEvent(ctx, failParams(event, applyErr))
			return err
		}
		return q.CompleteStripeEvent(ctx, queries.CompleteStripeEventParams{ID: event.ID, ProcessedAt: timestamp(now)})
	})
}

// eventFailure classifies a failed event for operators without recording
// processor data that error text may carry.
func eventFailure(err error) string {
	switch {
	case errors.Is(err, payments.ErrProcessorUnavailable), errors.Is(err, payments.ErrUnconfigured):
		return "PaymentUnavailable"
	case errors.Is(err, pgx.ErrNoRows):
		return "MissingRecord"
	case errors.Is(err, payments.ErrFacts), errors.Is(err, recognition.ErrInvalid):
		return "InvalidFacts"
	}
	return "ProcessingError"
}

func parseID(value string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil || !id.Valid {
		return id, payments.ErrFacts
	}
	return id, nil
}

// fetch reads the processor state an event needs, with no transaction open, and
// returns the event's effects ready to apply under the donor lock. A slow
// processor therefore holds neither a connection nor a lock.
func (w EventWorker) fetch(ctx context.Context, event queries.DonorStripeEvent, now time.Time) (eventApply, error) {
	data, err := payments.DecodeObject(event.Data)
	if err != nil {
		return nil, err
	}
	q := queries.New(w.DB)
	// fetcher coalesces subscription reads for the span of this one attempt, so
	// a subscription resolved once is not fetched again within it. It refuses
	// further reads once the attempt holds the donor lock.
	fetcher := newSubFetcher(w.Gateway)
	defer fetcher.freeze()
	switch {
	case data.Text("payment_update_id") != "":
		return w.preparePaymentUpdateEvent(ctx, q, data, now)
	case strings.HasPrefix(event.EventType, "refund."):
		return w.prepareRefundEvent(ctx, q, event, data, now)
	}
	return w.prepareCheckoutEvent(ctx, q, event, data, fetcher, now)
}

// preparePaymentUpdateEvent settles a hosted card setup whose session ended.
func (w EventWorker) preparePaymentUpdateEvent(
	ctx context.Context,
	q *queries.Queries,
	data payments.Object,
	now time.Time,
) (eventApply, error) {
	id, err := parseID(data.Text("payment_update_id"))
	if err != nil {
		return nil, err
	}
	snapshot, err := q.PaymentUpdateSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	gateway, ok := w.Gateway.(payments.PaymentMethodGateway)
	if !ok {
		return nil, payments.ErrUnconfigured
	}
	facts, err := fetchPaymentUpdate(ctx, q, gateway, snapshot, data.Text("id"), now)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, q *queries.Queries) (pgtype.UUID, error) {
		if _, err := q.LockDonor(ctx, snapshot.DonorID); err != nil {
			return pgtype.UUID{}, err
		}
		row, err := q.PaymentUpdateByID(ctx, id)
		if err != nil {
			return pgtype.UUID{}, err
		}
		status, err := applyPaymentUpdate(ctx, q, row, facts, now)
		if err != nil {
			return pgtype.UUID{}, err
		}
		if !terminalPaymentUpdate(status) {
			return pgtype.UUID{}, payments.ErrFacts
		}
		return snapshot.DonorID, nil
	}, nil
}

// prepareRefundEvent records a refund's current processor state. If another
// refund state was recorded meanwhile, the apply asks for a fresh read, since
// webhook timestamps cannot order competing refund states.
func (w EventWorker) prepareRefundEvent(
	ctx context.Context,
	q *queries.Queries,
	event queries.DonorStripeEvent,
	data payments.Object,
	now time.Time,
) (eventApply, error) {
	payment := text(data.Text("payment_intent"))
	snapshot, err := q.GiftSnapshotByPayment(ctx, payment)
	if err != nil {
		return nil, err
	}
	observations, err := q.GiftRefundObservations(ctx, snapshot.ID)
	if err != nil {
		return nil, err
	}
	current, err := w.retrieveRefund(ctx, data.Text("id"))
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, q *queries.Queries) (pgtype.UUID, error) {
		if _, err := q.LockDonor(ctx, snapshot.DonorID); err != nil {
			return pgtype.UUID{}, err
		}
		gift, err := q.GiftByPayment(ctx, payment)
		if err != nil {
			return pgtype.UUID{}, err
		}
		recorded, err := q.GiftRefundObservations(ctx, gift.ID)
		if err != nil {
			return pgtype.UUID{}, err
		}
		if recorded != observations || gift.ID != snapshot.ID {
			return pgtype.UUID{}, errRefetch
		}
		return snapshot.DonorID, recordRefund(ctx, q, gift, current, event.ID, pgtype.UUID{}, now)
	}, nil
}

// prepareCheckoutEvent applies an event about a DAD checkout: its session, its
// subscription, or a payment it collected.
func (w EventWorker) prepareCheckoutEvent(
	ctx context.Context,
	q *queries.Queries,
	event queries.DonorStripeEvent,
	data payments.Object,
	fetcher *subFetcher,
	now time.Time,
) (eventApply, error) {
	checkoutID, err := parseID(data.Text("checkout_id"))
	if err != nil {
		return nil, err
	}
	draft, err := q.CheckoutDraft(ctx, checkoutID)
	if err != nil {
		return nil, err
	}
	facts, err := w.prefetch(ctx, q, event, data, draft, fetcher)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, q *queries.Queries) (pgtype.UUID, error) {
		if _, err := q.LockDonor(ctx, draft.DonorID); err != nil {
			return pgtype.UUID{}, err
		}
		attempt, err := q.AttemptForCheckout(ctx, checkoutID)
		if err != nil {
			return pgtype.UUID{}, err
		}
		if err = recordCard(ctx, q, draft.DonorID, facts.card, now); err != nil {
			return pgtype.UUID{}, err
		}
		if err = w.applyLockedCheckoutEvent(ctx, q, event, data, draft, attempt, facts, fetcher, now); err != nil {
			return pgtype.UUID{}, err
		}
		return draft.DonorID, queueDeletedAccountReversals(ctx, q, checkoutID, now)
	}, nil
}

func (w EventWorker) retrieveRefund(ctx context.Context, id string) (payments.Object, error) {
	if w.Gateway == nil {
		return nil, payments.ErrUnconfigured
	}
	current, err := w.Gateway.RetrieveRefund(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Text("id") != id {
		return nil, payments.ErrFacts
	}
	return current, nil
}

// eventFacts is processor state for a checkout event, fetched before the
// donor lock is taken.
type eventFacts struct {
	subscription *subscriptionSnapshot
	card         *paymentCard
}

func (w EventWorker) prefetch(
	ctx context.Context,
	q *queries.Queries,
	event queries.DonorStripeEvent,
	data payments.Object,
	draft queries.DonorCheckout,
	fetcher *subFetcher,
) (eventFacts, error) {
	var facts eventFacts
	subscription := data.Text("subscription")
	switch kind := event.EventType; {
	case strings.HasPrefix(kind, "customer.subscription."):
		subscription = data.Text("id")
	case kind == "checkout.session.completed":
		if subscription == "" {
			return facts, nil
		}
	case strings.HasPrefix(kind, "invoice."):
		pending, err := q.PendingAmountChange(ctx, draft.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return facts, err
		}
		if !pending {
			return facts, nil
		}
	default:
		return facts, nil
	}
	if !strings.HasPrefix(subscription, "sub_") {
		return facts, payments.ErrFacts
	}
	attempt, err := q.AttemptSnapshotForCheckout(ctx, draft.ID)
	if err != nil {
		return facts, err
	}
	if facts.subscription, err = fetchSubscription(ctx, q, fetcher, attempt.ID, subscription); err != nil {
		return facts, err
	}
	facts.card, err = subscriptionCard(ctx, q, w.Gateway, facts.subscription.remote)
	return facts, err
}

func bindSubscription(ctx context.Context, q *queries.Queries, attempt queries.DonorStripeCheckout, subscription string) error {
	if !strings.HasPrefix(subscription, "sub_") ||
		(attempt.StripeSubscriptionID.Valid && attempt.StripeSubscriptionID.String != subscription) {
		return payments.ErrFacts
	}
	return q.BindSubscription(ctx, queries.BindSubscriptionParams{ID: attempt.ID, StripeSubscriptionID: text(subscription)})
}

func (w EventWorker) applyLockedCheckoutEvent(
	ctx context.Context,
	q *queries.Queries,
	event queries.DonorStripeEvent,
	data payments.Object,
	draft queries.DonorCheckout,
	attempt queries.DonorStripeCheckout,
	facts eventFacts,
	fetcher *subFetcher,
	now time.Time,
) error {
	kind := event.EventType
	switch {
	case strings.HasPrefix(kind, "checkout.session."):
		if err := applySessionEvent(ctx, q, kind, data, attempt); err != nil || facts.subscription == nil {
			return err
		}
		// Syncing here shows a new monthly gift without waiting for a
		// subscription event, which Stripe may send before or after this one.
		attempt.StripeSubscriptionID = text(facts.subscription.id)
		_, err := reconcileSubscription(ctx, q, fetcher, draft, attempt, facts.subscription, now)
		return err
	case strings.HasPrefix(kind, "customer.subscription."):
		if err := bindSubscription(ctx, q, attempt, data.Text("id")); err != nil {
			return err
		}
		attempt.StripeSubscriptionID = text(data.Text("id"))
		_, err := reconcileSubscription(ctx, q, fetcher, draft, attempt, facts.subscription, now)
		return err
	}
	expected := draft.AmountMinor
	if strings.HasPrefix(kind, "invoice.") {
		var err error
		if expected, err = w.invoiceAmount(ctx, q, data, draft, attempt, facts.subscription, fetcher, now); err != nil {
			return err
		}
	}
	if kind == "invoice.payment_failed" {
		amount, err := data.Int("amount_due")
		if err != nil || draft.Frequency != "MONTHLY" || data.Text("status") != "open" || data.Text("currency") != "usd" ||
			amount != expected ||
			!billingReason(data) {
			return payments.ErrFacts
		}
		if err = bindSubscription(ctx, q, attempt, data.Text("subscription")); err != nil {
			return err
		}
		period, err := recordPeriod(ctx, q, draft, data, expected, false)
		if err != nil {
			return err
		}
		return recordBillingObservation(ctx, q, period, event, "FAILED", amount, data.Text("billing_reason"), event.OccurredAt.Time, now)
	}
	payment, err := paymentFacts(ctx, q, event, data, draft, attempt, expected)
	if err != nil {
		return err
	}
	if err = recordGift(ctx, q, draft, payment, now); err != nil {
		return err
	}
	if payment.period.Valid {
		return recordBillingObservation(
			ctx,
			q,
			payment.period,
			event,
			"PAID",
			payment.amount,
			data.Text("billing_reason"),
			payment.paid,
			now,
		)
	}
	return nil
}

func recordBillingObservation(
	ctx context.Context,
	q *queries.Queries,
	period pgtype.UUID,
	event queries.DonorStripeEvent,
	status string,
	amount int64,
	reason string,
	occurred, recorded time.Time,
) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.InsertBillingObservation(ctx, queries.InsertBillingObservationParams{
		ID: id, PeriodID: period, SourceEventID: event.ID, Status: status, AmountMinor: amount,
		BillingReason: reason, OccurredAt: timestamp(occurred), EventCreatedAt: event.OccurredAt, RecordedAt: timestamp(recorded),
	})
}

func applySessionEvent(
	ctx context.Context,
	q *queries.Queries,
	kind string,
	data payments.Object,
	attempt queries.DonorStripeCheckout,
) error {
	if attempt.StripeSessionID.Valid && attempt.StripeSessionID.String != data.Text("id") {
		return payments.ErrFacts
	}
	status := attempt.Status
	if kind == "checkout.session.expired" {
		if data.Text("status") != "expired" {
			return payments.ErrFacts
		}
		if status != "COMPLETE" && status != "CANCELLED" {
			status = "EXPIRED"
			if err := q.ReleaseReservation(ctx, attempt.ID); err != nil {
				return err
			}
		}
	} else {
		if data.Text("status") != "complete" {
			return payments.ErrFacts
		}
		if status != "CANCELLED" {
			status = "COMPLETE"
		}
		if subscription := data.Text("subscription"); subscription != "" {
			if err := bindSubscription(ctx, q, attempt, subscription); err != nil {
				return err
			}
		}
	}
	return q.RecordCheckoutSession(
		ctx,
		queries.RecordCheckoutSessionParams{ID: attempt.ID, StripeSessionID: text(data.Text("id")), Status: status},
	)
}

// invoiceAmount returns the amount the invoice's period must charge: a
// confirmed amount change applies from its effective renewal, never prorated.
func (w EventWorker) invoiceAmount(
	ctx context.Context,
	q *queries.Queries,
	data payments.Object,
	draft queries.DonorCheckout,
	attempt queries.DonorStripeCheckout,
	snapshot *subscriptionSnapshot,
	fetcher *subFetcher,
	now time.Time,
) (int64, error) {
	pending, err := q.PendingAmountChange(ctx, draft.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if pending {
		if err = bindSubscription(ctx, q, attempt, data.Text("subscription")); err != nil {
			return 0, err
		}
		attempt.StripeSubscriptionID = text(data.Text("subscription"))
		if _, err = reconcileSubscription(ctx, q, fetcher, draft, attempt, snapshot, now); err != nil {
			return 0, err
		}
	}
	start, _, err := invoiceBounds(data)
	if err != nil {
		return 0, err
	}
	amount, err := q.InvoiceAmountChange(ctx, queries.InvoiceAmountChangeParams{CheckoutID: draft.ID, EffectiveAt: timestamp(start)})
	if errors.Is(err, pgx.ErrNoRows) {
		return draft.AmountMinor, nil
	}
	return amount, err
}

type paidGift struct {
	objectID      string
	amount        int64
	paid          time.Time
	paymentIntent string
	period        pgtype.UUID
}

func paymentFacts(
	ctx context.Context,
	q *queries.Queries,
	event queries.DonorStripeEvent,
	data payments.Object,
	draft queries.DonorCheckout,
	attempt queries.DonorStripeCheckout,
	expected int64,
) (paidGift, error) {
	out := paidGift{objectID: data.Text("id")}
	var err error
	switch event.EventType {
	case "invoice.payment_succeeded":
		before, e1 := data.IntDefault("starting_balance")
		after, e2 := data.IntDefault("ending_balance")
		total, e3 := data.Int("total")
		due, e4 := data.Int("amount_due")
		out.amount, err = data.Int("amount_paid")
		if draft.Frequency != "MONTHLY" || data.Text("status") != "paid" || !billingReason(data) || e1 != nil || e2 != nil || e3 != nil ||
			e4 != nil ||
			err != nil ||
			before != 0 ||
			after != 0 ||
			out.amount != total ||
			out.amount != due {
			return out, payments.ErrFacts
		}
		if err = bindSubscription(ctx, q, attempt, data.Text("subscription")); err != nil {
			return out, err
		}
		if out.paid, err = data.Unix("paid_at"); err != nil {
			return out, err
		}
		if out.paymentIntent, err = invoicePaymentIntent(data, out.amount); err != nil {
			return out, err
		}
		if out.period, err = recordPeriod(ctx, q, draft, data, expected, true); err != nil {
			return out, err
		}
	default:
		return out, payments.ErrFacts
	}
	if out.amount != expected || out.amount <= 0 || data.Text("currency") != "usd" {
		return out, payments.ErrFacts
	}
	return out, nil
}

// recordGift writes the payment with the checkout's attribution as it stood
// when charged; an expired referral window leaves the gift unattributed.
func recordGift(ctx context.Context, q *queries.Queries, draft queries.DonorCheckout, p paidGift, now time.Time) error {
	donor, err := q.LockDonor(ctx, draft.DonorID)
	if err != nil {
		return err
	}
	hadGift, err := q.HasGift(ctx, donor.ID)
	if err != nil {
		return err
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	var referrer pgtype.UUID
	var kind pgtype.Text
	if draft.ReferrerID.Valid && p.paid.Before(draft.ReferralExpiresAt.Time) {
		referrer, kind = draft.ReferrerID, draft.ReferralKind
	}
	pi := text(p.paymentIntent)
	if err = q.InsertGift(ctx, queries.InsertGiftParams{ID: id, DonorID: donor.ID, CheckoutID: draft.ID, StripeObjectID: p.objectID, AmountMinor: p.amount, Frequency: draft.Frequency, PaidAt: timestamp(p.paid), StripePaymentIntentID: pi, PeriodID: p.period, ReferrerID: referrer, ReferralKind: kind}); err != nil {
		return err
	}
	gift, err := q.GiftByStripeObject(ctx, p.objectID)
	if err != nil {
		return err
	}
	if gift.DonorID != donor.ID || gift.CheckoutID != draft.ID || gift.AmountMinor != p.amount || gift.Frequency != draft.Frequency ||
		gift.ReferrerID != referrer ||
		gift.ReferralKind != kind ||
		(gift.StripePaymentIntentID.Valid && gift.StripePaymentIntentID != pi) ||
		(gift.PeriodID.Valid && gift.PeriodID != p.period) {
		return payments.ErrFacts
	}
	if err = q.EnrichGift(ctx, queries.EnrichGiftParams{ID: gift.ID, StripePaymentIntentID: pi, PeriodID: p.period}); err != nil {
		return err
	}
	if !hadGift && !donor.ClaimedAt.Valid {
		return queueLoginEmail(ctx, q, donor.Email, "", now)
	}
	return nil
}

func billingReason(data payments.Object) bool {
	return data.Text("billing_reason") == "subscription_create" || data.Text("billing_reason") == "subscription_cycle"
}

// invoiceBounds returns the period of an invoice's single, complete line.
func invoiceBounds(data payments.Object) (time.Time, time.Time, error) {
	lines, err := data.Array("lines")
	if err != nil || !data.BoolIs("lines_complete", true) || len(lines) != 1 {
		return time.Time{}, time.Time{}, payments.ErrFacts
	}
	bounds, err := lines[0].Object("period")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return monthlyPeriod(bounds, "start", "end")
}

func recordPeriod(
	ctx context.Context,
	q *queries.Queries,
	draft queries.DonorCheckout,
	data payments.Object,
	expected int64,
	paid bool,
) (pgtype.UUID, error) {
	empty := pgtype.UUID{}
	start, end, err := invoiceBounds(data)
	if err != nil {
		return empty, err
	}
	lines, err := data.Array("lines")
	if err != nil {
		return empty, err
	}
	line := lines[0]
	amount, err := line.Int("amount")
	if err != nil || amount != expected || line.Text("parent_type") != "subscription_item_details" || !line.BoolIs("proration", false) ||
		line.Text("subscription") != data.Text("subscription") ||
		line.Text("currency") != "usd" {
		return empty, payments.ErrFacts
	}
	id, err := NewID()
	if err != nil {
		return empty, err
	}
	if err = q.InsertBillingPeriod(ctx, queries.InsertBillingPeriodParams{ID: id, DonorID: draft.DonorID, CheckoutID: draft.ID, StripeInvoiceID: data.Text("id"), StripeSubscriptionID: data.Text("subscription"), DueAt: timestamp(start), EndsAt: timestamp(end)}); err != nil {
		return empty, err
	}
	p, err := q.BillingPeriodByInvoice(ctx, data.Text("id"))
	if err != nil {
		return empty, err
	}
	if p.CheckoutID != draft.ID || p.StripeSubscriptionID != data.Text("subscription") || !p.DueAt.Time.Equal(start) ||
		!p.EndsAt.Time.Equal(end) {
		return empty, payments.ErrFacts
	}
	if paid {
		if err = q.MarkPeriodPaid(ctx, p.ID); err != nil {
			return empty, err
		}
	}
	return p.ID, nil
}

func invoicePaymentIntent(data payments.Object, amount int64) (string, error) {
	rows, err := data.Array("payments")
	if err != nil || !data.BoolIs("payments_complete", true) || len(rows) != 1 {
		return "", payments.ErrFacts
	}
	p := rows[0]
	paid, err := p.Int("amount_paid")
	if err != nil || paid != amount || p.Text("status") != "paid" || p.Text("currency") != "usd" || p.Text("invoice") != data.Text("id") ||
		!p.BoolIs("livemode", false) ||
		p.Text("type") != "payment_intent" ||
		!strings.HasPrefix(p.Text("payment_intent"), "pi_") {
		return "", payments.ErrFacts
	}
	return p.Text("payment_intent"), nil
}
