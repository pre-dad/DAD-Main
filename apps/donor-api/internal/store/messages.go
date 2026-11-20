package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type MessageWorker struct {
	DB        *pgxpool.Pool
	Settings  mail.Settings
	Origin    string
	Transport mail.Transport
}

// messageSuppression returns why job should no longer be sent, if it should
// not: optional messages recheck consent and every message rechecks that its
// facts still hold.
func messageSuppression(
	ctx context.Context,
	q *queries.Queries,
	job queries.DonorMessage,
	donor queries.Donor,
	now time.Time,
) (string, error) {
	if donor.Disabled {
		return "donor_disabled", nil
	}
	data, err := payments.DecodeObject(job.Context)
	if err != nil {
		return "", err
	}
	switch job.Kind {
	case "REFERRAL":
		return referralSuppression(ctx, q, data, donor)
	case "RECOVERY":
		return recoverySuppression(ctx, q, data, donor, now)
	case "LAPSED":
		return lapsedSuppression(ctx, q, data, donor, now)
	case eventPromotedCategory:
		return reservationLapsed(ctx, q, data, donor)
	}
	return "", nil
}

// lapsedSuppression stops a streak-lapsed notice when the lapse did not happen
// after all: the payment recovered or the gift was cancelled before grace ran
// out. Once grace has ended on a still-FAILED period the lapse is real and the
// notice stands. Like the recovery reminder it is a transactional status email
// with no opt-out (only marketing and referral emails are opt-in).
func lapsedSuppression(
	ctx context.Context,
	q *queries.Queries,
	data payments.Object,
	donor queries.Donor,
	now time.Time,
) (string, error) {
	period, reason, err := failureResolved(ctx, q, data, donor, now)
	if err != nil {
		return "", err
	}
	if reason == "payment_recovered" || reason == "subscription_cancelled" {
		return reason, nil
	}
	// The intent is available only at grace end, but guard against an early
	// delivery: if grace has not elapsed the run has not lapsed yet.
	if now.Sub(period.DueAt.Time) < recognition.Grace {
		return "grace_not_ended", nil
	}
	return "", nil
}

// referralSuppression stops a referral notice the donor opted out of, or
// whose referred donor no longer counts.
func referralSuppression(ctx context.Context, q *queries.Queries, data payments.Object, donor queries.Donor) (string, error) {
	pref, err := readPreferences(ctx, q, donor.ID)
	if err != nil {
		return "", err
	}
	if !pref.ReferralEmails {
		return "opted_out", nil
	}
	return referralLapsed(ctx, q, data, donor)
}

// referralLapsed reports why a conversion the donor would be told about no
// longer counts, if it does not: the referred donor's gifts were refunded or
// the relationship differs from the one recorded.
func referralLapsed(ctx context.Context, q *queries.Queries, data payments.Object, donor queries.Donor) (string, error) {
	id, err := parseID(data.Text("referred_id"))
	if err != nil {
		return "", err
	}
	projection, err := q.Projection(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "referral_no_longer_qualifies", nil
	}
	if err != nil {
		return "", err
	}
	if projection.ReferrerID != donor.ID || projection.ReferralKind.String != data.Text("kind") || !projection.NetMinor.Valid ||
		projection.NetMinor.Int64 <= 0 {
		return "referral_no_longer_qualifies", nil
	}
	return "", nil
}

// recoverySuppression stops a failed-payment reminder once the payment
// recovers, the gift is cancelled, grace ends, or a later reminder is due.
// Reminders go out on the failure day and day 5 of the 7-day grace (PRD §6.11
// "Streak at risk (grace day 5)"); the lapse at day 7 is a separate trigger.
func recoverySuppression(
	ctx context.Context,
	q *queries.Queries,
	data payments.Object,
	donor queries.Donor,
	now time.Time,
) (string, error) {
	period, reason, err := failureResolved(ctx, q, data, donor, now)
	if reason != "" || err != nil {
		return reason, err
	}
	elapsed := now.Sub(period.DueAt.Time)
	latest := int64(0)
	if elapsed >= 5*24*time.Hour {
		latest = 5
	}
	day, err := data.Int("day")
	if err != nil || (day != 0 && day != 5) {
		return "", payments.ErrFacts
	}
	if day < latest {
		return "newer_reminder_due", nil
	}
	return "", nil
}

// failureResolved reports why a failed renewal no longer threatens the
// donor's giving, if it does not: the payment recovered, the gift was
// cancelled, or grace ended.
func failureResolved(
	ctx context.Context,
	q *queries.Queries,
	data payments.Object,
	donor queries.Donor,
	now time.Time,
) (queries.DonorBillingPeriod, string, error) {
	id, err := parseID(data.Text("period_id"))
	if err != nil {
		return queries.DonorBillingPeriod{}, "", err
	}
	period, err := q.MessagePeriod(ctx, queries.MessagePeriodParams{ID: id, DonorID: donor.ID})
	if err != nil {
		return period, "", err
	}
	if period.Status != "FAILED" {
		return period, "payment_recovered", nil
	}
	sub, err := q.SubscriptionForCheckout(ctx, period.CheckoutID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return period, "", err
	}
	if err == nil && (sub.StopsAt.Valid || terminalSubscription(sub.Status)) {
		return period, "subscription_cancelled", nil
	}
	elapsed := now.Sub(period.DueAt.Time)
	if elapsed >= recognition.Grace {
		return period, "grace_ended", nil
	}
	if elapsed < 0 {
		return period, "", payments.ErrFacts
	}
	return period, "", nil
}

func renderMessage(
	ctx context.Context,
	q *queries.Queries,
	job queries.DonorMessage,
	donor queries.Donor,
	site publicSite,
	now time.Time,
) (string, string, error) {
	data, err := payments.DecodeObject(job.Context)
	if err != nil {
		return "", "", err
	}
	switch job.Kind {
	case "RECEIPT":
		id, err := parseID(data.Text("gift_id"))
		if err != nil {
			return "", "", err
		}
		receipt, err := readReceipt(ctx, q, job.DonorID, id, now)
		if err != nil {
			return "", "", err
		}
		return "Your DAD sandbox gift record", renderReceipt(receipt), nil
	case "ANNUAL":
		year, err := data.Int("year")
		if err != nil || year < 2026 || year > 9999 {
			return "", "", ErrReceipt
		}
		summary, err := readAnnualSummary(ctx, q, job.DonorID, int(year), now)
		if err != nil {
			return "", "", err
		}
		return "Your DAD sandbox annual summary", renderSummary(summary), nil
	case "REFERRAL":
		referral, ok := map[string]struct{ subject, phrase string }{
			"LINK": {"A Link Referral", "a Link Referral"},
			"IRL":  {"An IRL Referral", "an IRL Referral"},
		}[data.Text("kind")]
		if !ok {
			return "", "", payments.ErrFacts
		}
		return referral.subject + " completed a first test gift",
			"Someone you invited through " + referral.phrase + " completed their first test gift.\n" +
				"See your circle's impact: " + site.dashboard() + "\n\n" +
				"Turn off referral notifications: " + site.profile() + "\n" +
				"This is a sandbox notification. DAD launches in " + launchMonth + ".\n", nil
	case "RECOVERY":
		id, err := parseID(data.Text("period_id"))
		if err != nil {
			return "", "", err
		}
		period, err := q.MessagePeriod(ctx, queries.MessagePeriodParams{ID: id, DonorID: job.DonorID})
		if err != nil {
			return "", "", err
		}
		return "Your DAD test payment needs attention",
			"Your monthly test payment was declined. Your giving streak is at risk.\n" +
				"The recovery grace period ends " + iso(period.DueAt.Time.Add(recognition.Grace)) + ".\n" +
				"Update your payment method securely: " + site.paymentMethod() + "\n" +
				"If signed out, request a sign-in link for the email used to give, then return to payment methods.\n" +
				"Updating a card does not itself confirm payment recovery. Stripe's retry sequence handles collection.\n" +
				"This essential payment notice concerns sandbox giving, not a real charge.\n", nil
	case "LAPSED":
		return "Your DAD giving streak has ended",
			"Your monthly test payment did not recover within the grace period, so your giving streak has reset to zero.\n" +
				"You can start a new streak any time — your next monthly test gift begins it.\n" +
				"Start giving again securely: " + site.paymentMethod() + "\n" +
				"If signed out, request a sign-in link for the email used to give.\n" +
				"This is an essential status notice about sandbox giving, not a real charge.\n", nil
	case eventPromotedCategory:
		id, err := parseID(data.Text("event_id"))
		if err != nil {
			return "", "", err
		}
		event, err := q.EventByID(ctx, id)
		if err != nil {
			return "", "", err
		}
		return "You're off the waitlist for " + event.Title, renderPromotion(event, donor, site), nil
	case eventCancelledCategory:
		id, err := parseID(data.Text("event_id"))
		if err != nil {
			return "", "", err
		}
		event, err := q.EventByID(ctx, id)
		if err != nil {
			return "", "", err
		}
		return event.Title + " has been cancelled", renderCancellation(event, donor, site), nil
	case "PASSKEY_ADDED":
		added := data.Text("added_at")
		if added == "" {
			return "", "", payments.ErrFacts
		}
		passkey := "A passkey"
		if name := data.Text("name"); name != "" {
			passkey += " named \"" + name + "\""
		}
		return "A passkey was added to your DAD account", passkey + " was added to your DAD account at " + added + ". It can sign in to your account without an email link.\n\n" +
			"If you added it, there is nothing to do.\n" +
			"If you did not, sign in with an email link, remove the passkey from your profile, and choose Sign out on every device: " + site.profile() + "\n\n" +
			"This is an essential account security notice. It is sent whenever a passkey is added and cannot be turned off.\n", nil
	default:
		return "", "", payments.ErrFacts
	}
}

// renderCancellation tells a donor an event they signed up for will not go
// ahead, with the time in their own zone when their ZIP gives one.
func renderCancellation(event queries.DonorEvent, donor queries.Donor, site publicSite) string {
	return "We're sorry: " + event.Title + ", planned for " + eventTime(event, donor) + ", has been cancelled.\n\n" +
		"Your place has been released, so there is nothing you need to do.\n" +
		"See other events near you: " + site.dashboard() + "\n"
}

// eventTime is when an event starts, in the donor's zone when known.
func eventTime(event queries.DonorEvent, donor queries.Donor) string {
	zone := time.UTC
	if loc, err := time.LoadLocation(donor.Timezone.String); donor.Timezone.Valid && err == nil {
		zone = loc
	}
	return event.StartsAt.Time.In(zone).Format("Monday, January 2, 2006 at 3:04 PM MST")
}

// renderPromotion tells a donor a waitlisted spot is now theirs.
func renderPromotion(event queries.DonorEvent, donor queries.Donor, site publicSite) string {
	where := event.Venue
	if event.Address != "" {
		where += ", " + event.Address
	}
	return "A spot opened up at " + event.Title + ", and it's yours: your reservation is confirmed.\n\n" +
		"When: " + eventTime(event, donor) + "\n" +
		"Where: " + where + "\n\n" +
		"See the event and add it to your calendar: " + site.event(event.ID.String()) + "\n" +
		"If you can no longer go, cancel there before it starts so the next person on the waitlist gets your spot.\n"
}

// launchMonth is when sandbox notices say DAD goes live.
const launchMonth = "April 2027"

// deliveryLease reserves a claimed job while its send is in flight. It outlasts
// a worker step, so a claim abandoned by a crashed worker is retried later.
const deliveryLease = 5 * time.Minute

// MaxDeliveryAttempts parks a job for operator attention. Every query that
// selects due work, and the queue monitor, takes it as an argument.
const MaxDeliveryAttempts = 5

func messageRetryAt(now time.Time, attempts int32) pgtype.Timestamptz {
	return timestamp(now.Add(time.Duration(min(60, 1<<min(attempts, 6))) * time.Minute))
}

func failMessage(ctx context.Context, q *queries.Queries, job queries.DonorMessage, now time.Time, category string) error {
	if err := q.IncrementMessageAttempt(ctx, job.ID); err != nil {
		return err
	}
	return q.FailMessage(
		ctx,
		queries.FailMessageParams{ID: job.ID, LastError: text(category), AvailableAt: messageRetryAt(now, job.Attempts+1)},
	)
}

func completeMessage(ctx context.Context, q *queries.Queries, id pgtype.UUID, now time.Time, reason string) error {
	return q.CompleteMessage(
		ctx,
		queries.CompleteMessageParams{
			ID:               id,
			CompletedAt:      timestamp(now),
			SuppressedReason: pgtype.Text{String: reason, Valid: reason != ""},
		},
	)
}

func messageDataError(err error) bool {
	return errors.Is(err, payments.ErrFacts) || errors.Is(err, ErrReceipt) || errors.Is(err, pgx.ErrNoRows)
}

// prepare selects the next due message, suppressing it if it should no longer
// be sent and rendering its body on first use. It returns no job when the one
// found was finished or failed instead.
func (w MessageWorker) prepare(ctx context.Context, site publicSite, now time.Time) (pgtype.UUID, bool, error) {
	var id pgtype.UUID
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		job, err := q.NextMessage(ctx, queries.NextMessageParams{AvailableAt: timestamp(now), MaxAttempts: MaxDeliveryAttempts})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		donor, err := q.LockDonor(ctx, job.DonorID)
		if err != nil {
			return err
		}
		reason, err := messageSuppression(ctx, q, job, donor, now)
		if err == nil && reason != "" {
			return completeMessage(ctx, q, job.ID, now, reason)
		}
		if err == nil && !job.Body.Valid {
			var subject, body string
			if subject, body, err = renderMessage(ctx, q, job, donor, site, now); err == nil {
				err = q.RenderMessage(ctx, queries.RenderMessageParams{ID: job.ID, Subject: text(subject), Body: text(body)})
			}
		}
		if err != nil {
			if !messageDataError(err) {
				return err
			}
			return failMessage(ctx, q, job, now, "InvalidMessage")
		}
		id = job.ID
		return nil
	})
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	return id, worked, nil
}

// DeliverNext renders and sends the next due financial, referral, or account notice.
// It reports whether it found work.
func (w MessageWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	site, err := mailSite(w.Settings, w.Origin, w.Transport)
	if err != nil {
		return false, err
	}
	id, worked, err := w.prepare(ctx, site, now)
	if err != nil || !worked || !id.Valid {
		return worked, err
	}
	message, attempts, err := w.claim(ctx, id, now)
	if err != nil || message == nil {
		return err == nil, err
	}
	// The donor lock is released before SMTP; the claim keeps other workers off
	// the job, and a crash before completion resends with the same Message-ID.
	sendErr := w.Transport.Send(ctx, *message)
	q := queries.New(w.DB)
	if sendErr != nil {
		err = q.FailMessage(
			ctx,
			queries.FailMessageParams{ID: id, LastError: text("DeliveryError"), AvailableAt: messageRetryAt(now, attempts)},
		)
	} else {
		err = completeMessage(ctx, q, id, now, "")
	}
	return err == nil, err
}

// claim rechecks eligibility under the donor lock and reserves the job for one
// send. It returns no message when the job was completed, suppressed or failed
// instead, and otherwise the attempt the send will be.
func (w MessageWorker) claim(ctx context.Context, id pgtype.UUID, now time.Time) (*mail.Message, int32, error) {
	var message *mail.Message
	var attempt int32
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		job, err := q.MessageByID(ctx, id)
		if err != nil {
			return err
		}
		if job.CompletedAt.Valid || job.Attempts >= MaxDeliveryAttempts || job.AvailableAt.Time.After(now) {
			return nil
		}
		donor, err := q.LockDonor(ctx, job.DonorID)
		if err != nil {
			return err
		}
		reason, err := messageSuppression(ctx, q, job, donor, now)
		if err != nil {
			if !messageDataError(err) {
				return err
			}
			return failMessage(ctx, q, job, now, "InvalidMessage")
		}
		if reason != "" {
			return completeMessage(ctx, q, id, now, reason)
		}
		messageID, err := mail.MessageID(w.Settings.Sender, "message", id.String())
		if err != nil {
			return err
		}
		if err = q.ClaimMessage(ctx, queries.ClaimMessageParams{ID: id, AvailableAt: timestamp(now.Add(deliveryLease))}); err != nil {
			return err
		}
		message = &mail.Message{From: w.Settings.Sender, To: donor.Email, Subject: job.Subject.String, Body: job.Body.String, ID: messageID}
		attempt = job.Attempts + 1
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return message, attempt, nil
}
