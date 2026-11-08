package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queuePartnerMessage enqueues one org-level partner email intent, deduped by
// key so the same event enqueues at most once. It commits with the caller's
// transaction (the approval, metric publish, or completion that triggers it).
func queuePartnerMessage(
	ctx context.Context,
	q *queries.Queries,
	partner pgtype.UUID,
	kind, key string,
	details any,
	at time.Time,
) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	data, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if kind == "METRICS_VERIFIED" || kind == "INITIATIVE_COMPLETE" {
		if err := q.QueuePartnerPush(ctx, queries.QueuePartnerPushParams{
			PartnerID: partner, Kind: kind, DedupKey: key, AsOf: timestamp(at),
		}); err != nil {
			return err
		}
	}
	return q.QueuePartnerMessage(ctx, queries.QueuePartnerMessageParams{
		ID: id, PartnerID: partner, Kind: kind, DedupKey: key, Context: data, AvailableAt: timestamp(at),
	})
}

// PartnerMessageWorker delivers queued partner status emails (§7.5): metrics
// verified, initiative complete, and the monthly report. The approval email is
// the signer's sign-in link (PartnerLoginEmailWorker). It reuses the donor mail
// transport and the same claim + lease + bounded-retry shape as MessageWorker.
// Each intent is expanded into one delivery per current recipient, and every
// delivery is sent, retried and parked independently.
type PartnerMessageWorker struct {
	DB        *pgxpool.Pool
	Settings  mail.Settings
	Origin    string
	Transport mail.Transport
}

// renderPartnerMessage builds the subject and body for a partner intent. The
// body links into the partner portal; partner status emails carry no per-user
// detail, so one rendering serves every recipient of the org.
func renderPartnerMessage(
	ctx context.Context,
	q *queries.Queries,
	job queries.DonorPartnerMessage,
	site publicSite,
) (string, string, error) {
	partner, err := q.PartnerForMessage(ctx, job.PartnerID)
	if err != nil {
		return "", "", err
	}
	org := partner.Organization
	portal := string(site) + "/partner"
	switch job.Kind {
	case "METRICS_VERIFIED":
		return "New verified figures for " + org,
			"A DAD-verified figure for one of your initiatives has been published.\n" +
				"View your updated dashboard: " + portal + "\n" +
				"This is a sandbox notification. DAD launches in April 2027.\n", nil
	case "INITIATIVE_COMPLETE":
		return "An initiative has been marked complete",
			"One of " + org + "'s initiatives has been marked complete in DAD.\n" +
				"View it in your partner portal: " + portal + "\n" +
				"This is a sandbox notification. DAD launches in April 2027.\n", nil
	case "MONTHLY_REPORT":
		return "Your monthly DAD impact summary",
			"Your monthly partner summary for " + org + " is ready.\n" +
				"Open your dashboard to review committed and deployed funds and your verified figures: " + portal + "\n" +
				"This is a sandbox notification. DAD launches in April 2027.\n", nil
	default:
		return "", "", ErrReceipt
	}
}

// partnerMessageSuppression returns why a partner intent should no longer be
// sent, if it should not. Approval-dependent notices stop if the partner is no
// longer approved (an edge case: an application reverted before delivery).
func partnerMessageSuppression(ctx context.Context, q *queries.Queries, job queries.DonorPartnerMessage) (string, error) {
	partner, err := q.PartnerForMessage(ctx, job.PartnerID)
	if err != nil {
		return "", err
	}
	if !partner.ApprovedAt.Valid {
		return "partner_not_approved", nil
	}
	return "", nil
}

// DeliverNext does one bounded step of partner mail: it expands the next due
// intent into per-recipient deliveries, or else sends one due delivery. It
// reports whether it found work.
func (w PartnerMessageWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	site, err := mailSite(w.Settings, w.Origin, w.Transport)
	if err != nil {
		return false, err
	}
	if worked, err := w.fanOut(ctx, site, now); err != nil || worked {
		return worked, err
	}
	return w.deliver(ctx, now)
}

// fanOut claims the next due intent and expands it into one delivery per
// current recipient, suppressing it when the partner is no longer approved or
// has nobody to mail. The expansion is one set-based statement, so it is a
// short step however large the organisation, and repeating it adds nothing. It
// also expands intents queued before deliveries existed.
func (w PartnerMessageWorker) fanOut(ctx context.Context, site publicSite, now time.Time) (bool, error) {
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		row, err := q.NextPartnerMessage(ctx, timestamp(now))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		reason, err := partnerMessageSuppression(ctx, q, row)
		if err != nil {
			return err
		}
		if reason != "" {
			return q.CompletePartnerMessage(ctx, queries.CompletePartnerMessageParams{
				ID: row.ID, CompletedAt: timestamp(now), SuppressedReason: text(reason),
			})
		}
		if !row.Body.Valid {
			subject, body, err := renderPartnerMessage(ctx, q, row, site)
			if err != nil {
				if !messageDataError(err) {
					return err
				}
				return q.FailPartnerMessage(ctx, queries.FailPartnerMessageParams{
					ID: row.ID, LastError: text("InvalidMessage"), AvailableAt: messageRetryAt(now, row.Attempts+1),
				})
			}
			if err = q.RenderPartnerMessage(ctx, queries.RenderPartnerMessageParams{ID: row.ID, Subject: text(subject), Body: text(body)}); err != nil {
				return err
			}
		}
		recipients, err := q.FanOutPartnerMessage(ctx, queries.FanOutPartnerMessageParams{
			MessageID: row.ID, PartnerID: row.PartnerID, AvailableAt: timestamp(now),
		})
		if err != nil {
			return err
		}
		if recipients == 0 {
			return q.CompletePartnerMessage(ctx, queries.CompletePartnerMessageParams{
				ID: row.ID, CompletedAt: timestamp(now), SuppressedReason: text("no_recipients"),
			})
		}
		return q.FinishPartnerMessageFanOut(ctx, queries.FinishPartnerMessageFanOutParams{ID: row.ID, FannedOutAt: timestamp(now)})
	})
	return worked, err
}

// deliver sends the next due delivery. The claim counts the attempt and takes
// the lease before the send, so a crash mid-send is retried after the lease and
// parked after maxDeliveryAttempts, and the stable Message-ID lets a lost
// acknowledgement resend the same email.
func (w PartnerMessageWorker) deliver(ctx context.Context, now time.Time) (bool, error) {
	var message *mail.Message
	var id pgtype.UUID
	var attempts int32
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		row, err := q.NextPartnerDelivery(ctx, queries.NextPartnerDeliveryParams{
			AsOf: timestamp(now), LeaseUntil: timestamp(now.Add(deliveryLease)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked, id, attempts = true, row.ID, row.Attempts
		reason := ""
		switch {
		case !row.PartnerApproved:
			reason = "partner_not_approved"
		case row.Disabled || !row.Accepted:
			reason = "recipient_inactive"
		}
		if reason != "" {
			return q.CompletePartnerDelivery(ctx, queries.CompletePartnerDeliveryParams{
				ID: row.ID, CompletedAt: timestamp(now), SuppressedReason: text(reason),
			})
		}
		// The Message-ID must be @-free (MessageID rejects '@'), so it is keyed
		// by the message id and the recipient ordinal rather than the address.
		messageID, err := mail.MessageID(w.Settings.Sender, "partner", row.MessageID.String()+":"+strconv.Itoa(int(row.Ordinal)))
		if err != nil {
			return err
		}
		message = &mail.Message{
			From: w.Settings.Sender, To: row.Email, Subject: row.Subject.String, Body: row.Body.String, ID: messageID,
		}
		return nil
	})
	if err != nil || message == nil {
		return worked, err
	}
	q := queries.New(w.DB)
	if sendErr := w.Transport.Send(ctx, *message); sendErr != nil {
		return true, q.FailPartnerDelivery(ctx, queries.FailPartnerDeliveryParams{
			ID: id, LastError: text("DeliveryError"), AvailableAt: messageRetryAt(now, attempts),
		})
	}
	return true, q.CompletePartnerDelivery(ctx, queries.CompletePartnerDeliveryParams{ID: id, CompletedAt: timestamp(now)})
}

// PartnerMonthlyService queues the monthly partner report (§6.11) once per
// approved partner per calendar month. It is a sweep, like the event-reminder
// sweep, so a frequent run is cheap and idempotent: the dedup_key carries the
// year-month and PartnersDueMonthlyReport excludes partners already queued.
type PartnerMonthlyService struct {
	DB *pgxpool.Pool
}

// partnerMonthlyBatch bounds one sweep step's work.
const partnerMonthlyBatch = 64

// QueueDueMonthly enqueues the monthly report for approved partners that have
// not had one queued for the current month. It returns how many it queued.
func (s PartnerMonthlyService) QueueDueMonthly(ctx context.Context, now time.Time) (int, error) {
	period := now.In(recognition.NewYork).Format("2006-01")
	queued := 0
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		ids, err := q.PartnersDueMonthlyReport(ctx, queries.PartnersDueMonthlyReportParams{Period: period, Batch: partnerMonthlyBatch})
		if err != nil {
			return err
		}
		for _, id := range ids {
			key := "partner-monthly:" + id.String() + ":" + period
			if err := queuePartnerMessage(ctx, q, id, "MONTHLY_REPORT", key, map[string]string{"period": period}, now); err != nil {
				return err
			}
			queued++
		}
		return nil
	})
	return queued, err
}
