package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// RequestClaim queues a fresh sign-in link for the donor.
func (s *StaffService) RequestClaim(ctx context.Context, token string, id pgtype.UUID) error {
	_, err := staffWrite(ctx, s, token, "message.resend", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
		q := queries.New(tx)
		donor, err := q.StaffDonorByID(ctx, id)
		if err != nil {
			return struct{}{}, err
		}
		if err = queueLoginEmail(ctx, q, donor.Email, "", now); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, auditStaff(ctx, q, actor.ID, "message.claim_requested", id.String(), nil, now)
	})
	return err
}

type QueuedMessage struct {
	ID     pgtype.UUID `json:"id"`
	Queued bool        `json:"queued"`
}

// ResendMessage queues a copy of a delivered or parked receipt or annual
// summary. requestID makes the resend idempotent for its actor and source.
func (s *StaffService) ResendMessage(ctx context.Context, token string, messageID, requestID pgtype.UUID) (QueuedMessage, error) {
	if !ValidRequestID(requestID) {
		return QueuedMessage{}, ErrStaffInput
	}
	return staffWrite(ctx, s, token, "message.resend", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (QueuedMessage, error) {
		q := queries.New(tx)
		source, err := q.StaffMessageByID(ctx, messageID)
		if err != nil {
			return QueuedMessage{}, err
		}
		if _, err = q.LockDonor(ctx, source.DonorID); err != nil {
			return QueuedMessage{}, err
		}
		source, err = q.StaffMessageByID(ctx, messageID)
		if err != nil {
			return QueuedMessage{}, err
		}
		key := "resend:" + requestID.String()
		existing, err := q.MessageByDedupKey(ctx, key)
		if err == nil {
			return resent(existing, messageID, actor.ID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return QueuedMessage{}, err
		}
		if (source.Kind != "RECEIPT" && source.Kind != "ANNUAL") || source.SuppressedReason.Valid ||
			(!source.CompletedAt.Valid && source.Attempts < MaxDeliveryAttempts) {
			return QueuedMessage{}, ErrStaffConflict
		}
		recent, err := q.RecentDonorMessage(
			ctx,
			queries.RecentDonorMessageParams{DonorID: source.DonorID, Kind: source.Kind, CreatedAt: timestamp(now.Add(-time.Minute))},
		)
		if err != nil {
			return QueuedMessage{}, err
		}
		if recent {
			return QueuedMessage{}, ErrStaffConflict
		}
		var metadata map[string]any
		if err = json.Unmarshal(source.Context, &metadata); err != nil {
			return QueuedMessage{}, err
		}
		if metadata == nil {
			return QueuedMessage{}, ErrStaffConflict
		}
		metadata["resend_source"], metadata["resend_actor"] = messageID.String(), actor.ID.String()
		if err = queueMessage(ctx, q, source.DonorID, source.Kind, key, metadata, now); err != nil {
			return QueuedMessage{}, err
		}
		job, err := q.MessageByDedupKey(ctx, key)
		if err != nil {
			return QueuedMessage{}, err
		}
		// The same key may have been used meanwhile under another donor's lock.
		queued, err := resent(job, messageID, actor.ID)
		if err != nil {
			return QueuedMessage{}, err
		}
		return queued, auditStaff(
			ctx,
			q,
			actor.ID,
			"message.resend_requested",
			job.ID.String(),
			map[string]string{"source_id": messageID.String()},
			now,
		)
	})
}

// resent reports job as the resend of source by actor, or a conflict if its
// idempotency key was used for a different resend.
func resent(job queries.DonorMessage, source, actor pgtype.UUID) (QueuedMessage, error) {
	var metadata map[string]any
	if err := json.Unmarshal(job.Context, &metadata); err != nil {
		return QueuedMessage{}, err
	}
	if metadata["resend_source"] != source.String() || metadata["resend_actor"] != actor.String() {
		return QueuedMessage{}, ErrStaffConflict
	}
	return QueuedMessage{Queued: true, ID: job.ID}, nil
}
