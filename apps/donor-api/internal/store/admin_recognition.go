package store

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// repairReason trims and validates the reason a staff member gives for a repair.
func repairReason(reason string) (string, bool) {
	reason = identity.TrimSpace(reason)
	n := utf8.RuneCountInString(reason)
	return reason, n >= 10 && n <= 1000 && plainText(reason, true)
}

// RecomputeRecognition repairs derived state after a documented system error.
// The recorded payment history remains the authority for continuity and awards.
func (s *StaffService) RecomputeRecognition(ctx context.Context, token string, donor pgtype.UUID, reason string) error {
	reason, ok := repairReason(reason)
	if !ok {
		return ErrStaffInput
	}
	_, err := staffDonorWrite(
		ctx,
		s,
		token,
		"donor.recompute",
		donor,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			before, err := q.Projection(ctx, donor)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, err
			}
			after, err := refreshProjection(ctx, tx, donor, now)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "donor.recognition_recomputed", donor.String(), map[string]any{
				"reason": reason, "before_current": before.CurrentStreak, "after_current": after.CurrentStreak,
				"before_best": before.BestStreak, "after_best": after.BestStreak, "rules_version": projectionRules,
			}, now)
		},
	)
	return err
}

// ResetStreak restarts a donor's streak from now without touching payment history.
func (s *StaffService) ResetStreak(ctx context.Context, token string, donor pgtype.UUID, reason string) error {
	reason, ok := repairReason(reason)
	if !ok {
		return ErrStaffInput
	}
	_, err := staffDonorWrite(
		ctx,
		s,
		token,
		"donor.recompute",
		donor,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			id, err := NewID()
			if err != nil {
				return struct{}{}, err
			}
			if err = q.InsertStreakReset(ctx, queries.InsertStreakResetParams{ID: id, DonorID: donor, ResetAt: timestamp(now), ActorID: actor.ID, Reason: reason}); err != nil {
				return struct{}{}, err
			}
			if _, err = refreshProjection(ctx, tx, donor, now); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "donor.streak_reset", donor.String(), map[string]string{"reason": reason}, now)
		},
	)
	return err
}
