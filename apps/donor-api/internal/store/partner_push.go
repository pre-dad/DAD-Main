package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"dad/donor-api/internal/push"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartnerPushService registers the native partner session's push device.
type PartnerPushService struct {
	DB        *pgxpool.Pool
	Platforms []string
}

// RegisterDevice binds a push device to the calling native partner session,
// moving it from any other session or donor installation that held it.
func (s PartnerPushService) RegisterDevice(ctx context.Context, token string, device PushDevice) (PushRegistration, error) {
	device, err := device.normalize()
	if err != nil {
		return PushRegistration{}, err
	}
	if !slices.Contains(s.Platforms, device.Platform) {
		return PushRegistration{}, ErrPushUnconfigured
	}
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (PushRegistration, error) {
		q := queries.New(tx)
		if err := q.LockPushInstallation(ctx, device.Token); err != nil {
			return PushRegistration{}, err
		}
		actor, err := authenticatePartner(ctx, q, token, time.Now().UTC())
		if err != nil {
			return PushRegistration{}, err
		}
		if _, err := q.LockPartnerOrg(ctx, actor.PartnerID); err != nil {
			return PushRegistration{}, err
		}
		now := time.Now().UTC()
		if actor, err = authenticatePartner(ctx, q, token, now); err != nil {
			return PushRegistration{}, err
		}
		environment := pgtype.Text{String: device.Environment, Valid: device.Environment != ""}
		// Refuse browser sessions before releasing any other installation's token.
		if _, err := q.NativePartnerSession(ctx, HashToken(token)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				err = ErrPartnerDenied
			}
			return PushRegistration{}, err
		}
		if err := q.ReleasePartnerPushToken(ctx, text(device.Token)); err != nil {
			return PushRegistration{}, err
		}
		if _, err := q.SetPartnerPushToken(ctx, queries.SetPartnerPushTokenParams{
			TokenHash: HashToken(token), Platform: text(device.Platform), PushToken: text(device.Token), PushEnvironment: environment,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				err = ErrPartnerDenied
			}
			return PushRegistration{}, err
		}
		// zeroUUID matches no donor session, so every donor session holding the token releases it.
		if err := q.ReleasePushToken(ctx, queries.ReleasePushTokenParams{PushToken: text(device.Token), ID: zeroUUID}); err != nil {
			return PushRegistration{}, err
		}
		// A newly approved organization has no installation until its first sign-in.
		org, err := q.PartnerOrgByID(ctx, actor.PartnerID)
		if err != nil {
			return PushRegistration{}, err
		}
		if now.Sub(org.ApprovedAt.Time) < pushLifetime {
			if err := q.QueuePartnerPush(ctx, queries.QueuePartnerPushParams{
				PartnerID: actor.PartnerID, Kind: "APPROVED", DedupKey: "partner-approved:" + actor.PartnerID.String(), AsOf: timestamp(now),
			}); err != nil {
				return PushRegistration{}, err
			}
		}
		return PushRegistration{Registered: true, Platform: &device.Platform, Environment: textPointer(environment)}, nil
	})
}

// UnregisterDevice clears the calling partner session's push device.
func (s PartnerPushService) UnregisterDevice(ctx context.Context, token string) (PushRegistration, error) {
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (PushRegistration, error) {
		q := queries.New(tx)
		if _, err := authenticatePartner(ctx, q, token, time.Now().UTC()); err != nil {
			return PushRegistration{}, err
		}
		return PushRegistration{}, q.ClearPartnerPushToken(ctx, HashToken(token))
	})
}

// PartnerPushWorker delivers queued partner push notifications.
type PartnerPushWorker struct {
	DB        *pgxpool.Pool
	Providers map[string]push.Provider
}

// partnerPushContent renders a notification; ok is false for a kind it has no wording for.
func partnerPushContent(row queries.DonorPartnerPushNotification) (n push.Notification, ok bool) {
	n = push.Notification{ID: row.ID.String(), Category: "PARTNER_" + row.Kind, Path: "/partner", Expires: row.ExpiresAt.Time}
	switch row.Kind {
	case "APPROVED":
		n.Title, n.Body = "Your partnership is approved", "Your sandbox partner portal is ready. Open the app to see it."
	case "METRICS_VERIFIED":
		n.Title, n.Body = "New verified figures", "Verified figures have been published. Open your partner portal for details."
	case "INITIATIVE_COMPLETE":
		n.Title, n.Body = "An initiative is complete", "An initiative has been marked complete. Open your partner portal for details."
	default:
		return n, false
	}
	return n, true
}

// DeliverNext sends the next due partner notification, reporting whether it
// found one.
func (w PartnerPushWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	var job *queries.DonorPartnerPushNotification
	var target queries.PartnerPushTargetRow
	var content push.Notification
	worked := false
	complete := func(q *queries.Queries, row queries.DonorPartnerPushNotification, reason string) error {
		return q.CompletePartnerPush(
			ctx,
			queries.CompletePartnerPushParams{ID: row.ID, CompletedAt: timestamp(now), SuppressedReason: text(reason)},
		)
	}
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		row, err := q.NextPartnerPush(ctx, timestamp(now))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		if !now.Before(row.ExpiresAt.Time) {
			return complete(q, row, "expired")
		}
		target, err = q.PartnerPushTarget(ctx, queries.PartnerPushTargetParams{ID: row.SessionID, PartnerID: row.PartnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return complete(q, row, "registration_ended")
		}
		if err != nil {
			return err
		}
		if w.Providers[target.Platform.String] == nil {
			return complete(q, row, "push_unconfigured")
		}
		var ok bool
		if content, ok = partnerPushContent(row); !ok {
			return complete(q, row, "unsupported_kind")
		}
		job = &row
		return q.ClaimPartnerPush(ctx, queries.ClaimPartnerPushParams{ID: row.ID, AvailableAt: timestamp(now.Add(deliveryLease))})
	})
	if err != nil || job == nil {
		return worked, err
	}
	sendErr := w.Providers[target.Platform.String].Send(
		ctx,
		push.Device{Token: target.PushToken.String, Environment: target.PushEnvironment.String},
		content,
	)
	return true, pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		if sendErr == nil {
			return complete(q, *job, "")
		}
		if errors.Is(sendErr, push.ErrUnregistered) {
			if err := q.DropPartnerPushToken(ctx, queries.DropPartnerPushTokenParams{ID: job.SessionID, PushToken: target.PushToken}); err != nil {
				return err
			}
			return complete(q, *job, "unregistered")
		}
		attempt := job.Attempts + 1
		retry := messageRetryAt(now, attempt)
		code := "DeliveryError"
		var unavailable push.Unavailable
		if errors.As(sendErr, &unavailable) && now.Add(unavailable.After).After(retry.Time) {
			retry = timestamp(now.Add(unavailable.After))
		}
		if errors.Is(sendErr, push.ErrRejected) {
			attempt, code = maxDeliveryAttempts, "ProviderRejected"
		}
		return q.FailPartnerPush(
			ctx,
			queries.FailPartnerPushParams{ID: job.ID, AvailableAt: retry, Attempts: attempt, LastError: text(code)},
		)
	})
}
