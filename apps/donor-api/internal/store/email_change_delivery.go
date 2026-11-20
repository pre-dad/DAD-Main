package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type EmailChangeWorker LoginEmailWorker

type emailChangeSend struct {
	id      pgtype.UUID
	stage   string
	message mail.Message
}

// DeliverNext sends the next due email-change confirmation. It reports
// whether it found work.
func (w EmailChangeWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	site, err := mailSite(w.Settings, w.Origin, w.Transport)
	if err != nil {
		return false, err
	}
	send, worked, err := w.claim(ctx, site, now)
	if err != nil || send == nil {
		return worked, err
	}
	q := queries.New(w.DB)
	if err = w.Transport.Send(ctx, send.message); err != nil {
		err = q.RetryEmailChangeDelivery(
			ctx,
			queries.RetryEmailChangeDeliveryParams{ID: send.id, AvailableAt: timestamp(now.Add(time.Minute)), Stage: send.stage},
		)
	} else {
		err = q.EmailChangeDelivered(ctx, queries.EmailChangeDeliveredParams{ID: send.id, SentAt: timestamp(now), Stage: send.stage})
	}
	return err == nil, err
}

// claim selects the next confirmation under its donor's lock and reserves it
// for one send, which happens after the lock is released. Completion matches
// the claimed stage, so a confirmation that advanced meanwhile is not marked
// sent. It returns no send when the one found was cancelled or failed instead.
func (w EmailChangeWorker) claim(ctx context.Context, site publicSite, now time.Time) (*emailChangeSend, bool, error) {
	var send *emailChangeSend
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		owner, err := q.EmailChangeDeliveryOwner(ctx, queries.EmailChangeDeliveryOwnerParams{AvailableAt: timestamp(now), MaxAttempts: MaxDeliveryAttempts})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		donor, err := q.LockDonor(ctx, owner)
		if err != nil {
			return err
		}
		job, err := q.NextEmailChangeDelivery(ctx, queries.NextEmailChangeDeliveryParams{DonorID: owner, AvailableAt: timestamp(now), MaxAttempts: MaxDeliveryAttempts})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		if donor.Disabled || donor.Email != job.OldEmail || !job.ExpiresAt.Time.After(now) {
			return q.CancelEmailChanges(ctx, owner)
		}
		proof, err := w.Settings.EncryptionKeys.Decrypt(job.EncryptedToken)
		if err != nil {
			return q.EmailChangeDeliveryFailed(
				ctx,
				queries.EmailChangeDeliveryFailedParams{ID: job.ID, AvailableAt: timestamp(now.Add(time.Minute))},
			)
		}
		messageID, err := mail.MessageID(w.Settings.Sender, "email-change-"+job.Stage, job.ID.String())
		if err != nil {
			return err
		}
		recipient := job.OldEmail
		body := "Someone signed in to your DAD account requested changing its email to " + job.NewEmail + ". Confirm only if you requested this change."
		if job.Stage == "NEW" {
			recipient = job.NewEmail
			body = "Confirm this address to finish changing your DAD account email. This will sign out every existing session and remove your passkeys."
		}
		if err = q.ClaimEmailChangeDelivery(ctx, queries.ClaimEmailChangeDeliveryParams{ID: job.ID, AvailableAt: timestamp(now.Add(deliveryLease))}); err != nil {
			return err
		}
		message := mail.Message{
			From:    w.Settings.Sender,
			To:      recipient,
			ID:      messageID,
			Subject: "Confirm your DAD account email change",
			Body: body + "\n\n" + site.emailChange(
				string(proof),
			) + "\n\nConfirm on the page. This link is single-use and expires 15 minutes after it was requested. If you did not request this, ignore this message. No change will be made without both confirmations.\n",
		}
		send = &emailChangeSend{id: job.ID, stage: job.Stage, message: message}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return send, worked, nil
}
