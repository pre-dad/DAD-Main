package store

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPushUnconfigured refuses a device registration for a platform this
// deployment cannot deliver to, so no notification is queued for it.
var ErrPushUnconfigured = errors.New("push notifications are not configured for this platform")

// ErrPushToken refuses a token that is not the form its platform issues.
var ErrPushToken = errors.New("invalid push token")

var (
	// APNs device tokens are hex; Apple documents them as variable length.
	apnsToken = regexp.MustCompile(`^(?:[0-9a-f]{2}){32,100}$`)
	// FCM documents no length; tokens run to about 200 characters.
	fcmToken = regexp.MustCompile(`^[A-Za-z0-9_:-]+$`)
)

// pushLifetime is how long a notification stays worth showing. One that
// could not be sent by then, or that waited on an unconfigured provider, is
// dropped rather than shown late.
const pushLifetime = 24 * time.Hour

// PushDevice is the installation behind the caller's app session.
type PushDevice struct {
	Platform    string // push.IOS or push.Android
	Token       string
	Environment string // SANDBOX or PRODUCTION for iOS, where it defaults to PRODUCTION
}

// PushRegistration is the caller's session's push registration.
type PushRegistration struct {
	Registered  bool    `json:"registered"`
	Platform    *string `json:"platform"`
	Environment *string `json:"environment"`
}

type PushService struct {
	DB        *pgxpool.Pool
	Platforms []string // those with a configured provider
}

// normalize validates the device's token for its platform. APNs tokens are
// hex, which apps may print in either case.
func (d PushDevice) normalize() (PushDevice, error) {
	switch d.Platform {
	case push.IOS:
		d.Token = strings.ToLower(d.Token)
		if d.Environment == "" {
			d.Environment = "PRODUCTION"
		}
		if !apnsToken.MatchString(d.Token) || (d.Environment != "SANDBOX" && d.Environment != "PRODUCTION") {
			return d, ErrPushToken
		}
	case push.Android:
		if len(d.Token) < 32 || len(d.Token) > 1024 || !fcmToken.MatchString(d.Token) || d.Environment != "" {
			return d, ErrPushToken
		}
	default:
		return d, ErrPushToken
	}
	return d, nil
}

// RegisterDevice records device's token on the caller's app session,
// replacing any it held. A token held by another session, the donor's or
// anyone's, moves here: it names an installation, which signed in again.
func (s PushService) RegisterDevice(ctx context.Context, token string, device PushDevice) (PushRegistration, error) {
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
		user, err := lockSignedIn(ctx, q, token)
		if err != nil {
			return PushRegistration{}, err
		}
		if err := q.ReleasePartnerPushToken(ctx, text(device.Token)); err != nil {
			return PushRegistration{}, err
		}
		if err := q.ReleasePushToken(ctx, queries.ReleasePushTokenParams{PushToken: text(device.Token), ID: user.session}); err != nil {
			return PushRegistration{}, err
		}
		environment := pgtype.Text{String: device.Environment, Valid: device.Environment != ""}
		set, err := q.SetPushToken(
			ctx,
			queries.SetPushTokenParams{
				ID:              user.session,
				Platform:        text(device.Platform),
				PushToken:       text(device.Token),
				PushEnvironment: environment,
			},
		)
		if err != nil {
			return PushRegistration{}, err
		}
		// A browser session's cookie presented as a bearer token is no app.
		if set == 0 {
			return PushRegistration{}, ErrDenied
		}
		if err := recordDevice(ctx, q, user.ID, device.Token, time.Now().UTC()); err != nil {
			return PushRegistration{}, err
		}
		return PushRegistration{Registered: true, Platform: &device.Platform, Environment: textPointer(environment)}, nil
	})
}

// UnregisterDevice removes the caller's session's push token, if any.
func (s PushService) UnregisterDevice(ctx context.Context, token string) (PushRegistration, error) {
	return sessionWrite(ctx, s.DB, token, func(q *queries.Queries, user signedIn, _ time.Time) (PushRegistration, error) {
		return PushRegistration{}, q.ClearPushToken(ctx, user.session)
	})
}

// queuePushIntents queues the notifications the projection p calls for.
func queuePushIntents(ctx context.Context, q *queries.Queries, p projection, now time.Time) error {
	params := queries.QueuePushIntentsParams{
		DonorID:     p.DonorID,
		AsOf:        timestamp(now),
		ExpiresAt:   timestamp(now.Add(pushLifetime)),
		RecentSince: timestamp(now.Add(-pushLifetime)),
	}
	if p.CurrentStreak.Valid && p.streakSince != nil {
		run := recognition.Streak{Since: p.streakSince}
		ladder, err := configuredLadder(ctx, q, run, now, p.zone)
		if err != nil {
			return err
		}
		if reached := ladder.LastMilestone; reached > 0 && now.Sub(run.Reaches(reached, p.zone)) < pushLifetime {
			if err := recordServerEvent(ctx, q, p.DonorID, "milestone_reached", "milestone:"+p.DonorID.String()+":"+p.streakSince.String()+":"+strconv.FormatInt(reached, 10), run.Reaches(reached, p.zone)); err != nil {
				return err
			}
			params.Milestone = pgtype.Int4{Int32: int32(reached), Valid: true}
			params.RunStartedAt = timestamp(*p.streakSince)
		}
	}
	return q.QueuePushIntents(ctx, params)
}

// pushSuppression returns why a notification should no longer be sent, if
// it should not: it expired, the donor turned its category off, or the fact
// it announces no longer holds.
func pushSuppression(
	ctx context.Context,
	q *queries.Queries,
	job queries.DonorPushNotification,
	donor queries.Donor,
	now time.Time,
) (string, error) {
	if !now.Before(job.ExpiresAt.Time) {
		return "expired", nil
	}
	if donor.Disabled {
		return "donor_disabled", nil
	}
	pref, err := readPreferences(ctx, q, donor.ID)
	if err != nil {
		return "", err
	}
	data, err := payments.DecodeObject(job.Context)
	if err != nil {
		return "", err
	}
	switch job.Category {
	case "REFERRAL_CONVERTED":
		if !pref.PushReferralConverted {
			return "opted_out", nil
		}
		return referralLapsed(ctx, q, data, donor)
	case "GIFT_PROCESSED":
		if !pref.PushGiftProcessed {
			return "opted_out", nil
		}
		giftID, err := parseID(data.Text("gift_id"))
		if err != nil {
			return "", err
		}
		gift, err := q.ReceiptGift(ctx, queries.ReceiptGiftParams{ID: giftID, DonorID: donor.ID, PaidAt: timestamp(now)})
		if errors.Is(err, pgx.ErrNoRows) {
			return "gift_unavailable", nil
		}
		if err != nil {
			return "", err
		}
		refunds, err := q.ReceiptRefunds(ctx, gift.ID)
		if err != nil {
			return "", err
		}
		record, err := giftRecord(gift, refunds)
		if err != nil {
			return "", err
		}
		if record.Net == 0 {
			return "gift_refunded", nil
		}
		return "", nil
	case "STREAK_AT_RISK":
		if !pref.PushStreakAtRisk {
			return "opted_out", nil
		}
		if _, present := data["day"]; present {
			return recoverySuppression(ctx, q, data, donor, now)
		}
		_, reason, err := failureResolved(ctx, q, data, donor, now)
		return reason, err
	case "STREAK_MILESTONE":
		if !pref.PushStreakMilestone {
			return "opted_out", nil
		}
		days, err := data.Int("days")
		if err != nil {
			return "superseded", nil
		}
		current, err := q.Projection(ctx, donor.ID)
		if err != nil {
			return "", err
		}
		if !current.StreakSince.Valid {
			return "streak_ended", nil
		}
		run := recognition.Streak{Since: &current.StreakSince.Time}
		if run.Ladder(now, recognition.Zone(donor.Timezone.String)).Days < days {
			return "streak_ended", nil
		}
		return "", nil
	case "STREAK_LAPSED":
		if !pref.PushStreakLapsed {
			return "opted_out", nil
		}
		// The lapse holds unless the donor recovered: if the failed period that
		// ended the run is no longer FAILED (payment recovered), the lapse news
		// is stale. A cancelled subscription or ended grace does not un-lapse.
		_, reason, err := failureResolved(ctx, q, data, donor, now)
		if err != nil {
			return "", err
		}
		if reason == "payment_recovered" || reason == "subscription_cancelled" {
			return reason, nil
		}
		return "", nil
	case "BADGE_EARNED":
		if !pref.PushBadgeEarned {
			return "opted_out", nil
		}
		// The earn is permanent, but an admin may have revoked it between queue
		// and send (B-4); a revoked badge reads as unearned, so do not announce it.
		code := data.Text("code")
		if code == "" {
			return "", payments.ErrFacts
		}
		earned, err := q.BadgeEarned(ctx, queries.BadgeEarnedParams{DonorID: donor.ID, Code: code})
		if err != nil {
			return "", err
		}
		if !earned {
			return "badge_revoked", nil
		}
		return "", nil
	case "EVENT_PROMOTED":
		if !pref.PushEventPromoted {
			return "opted_out", nil
		}
		return reservationLapsed(ctx, q, data, donor)
	case "EVENT_REMINDER":
		if !pref.PushEventReminder {
			return "opted_out", nil
		}
		return reservationLapsed(ctx, q, data, donor)
	case eventNearbyCategory:
		if !pref.Volunteer || !pref.PushEventNearby {
			return "opted_out", nil
		}
		event, err := parseID(data.Text("event_id"))
		if err != nil {
			return "", err
		}
		e, err := q.EventByID(ctx, event)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if err != nil || e.CancelledAt.Valid || e.PublishState != "PUBLISHED" {
			return "event_withdrawn", nil
		}
		return "", nil
	case eventCancelledCategory:
		return "", nil
	}
	return "", payments.ErrFacts
}

// reservationLapsed reports why a notice about the donor's seat at an event no
// longer applies: they cancelled it or are back on the waitlist. These notices
// concern a reservation the donor made, so unlike new-event alerts they do not
// depend on the volunteer opt-in (E-1).
func reservationLapsed(ctx context.Context, q *queries.Queries, data payments.Object, donor queries.Donor) (string, error) {
	event, err := parseID(data.Text("event_id"))
	if err != nil {
		return "", err
	}
	reservation, err := q.ReservationFor(ctx, queries.ReservationForParams{EventID: event, DonorID: donor.ID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && reservation.Status != reservationReserved) {
		return "reservation_released", nil
	}
	return "", err
}

// pushContent is what a notification says. It names no one and no amount:
// notifications transit Apple's and Google's servers and show on lock
// screens, so the app fetches any detail through the API.
func pushContent(job queries.DonorPushNotification) (push.Notification, error) {
	data, err := payments.DecodeObject(job.Context)
	if err != nil {
		return push.Notification{}, err
	}
	n := push.Notification{ID: job.ID.String(), Category: job.Category, Path: dashboardPath, Expires: job.ExpiresAt.Time}
	switch job.Category {
	case "REFERRAL_CONVERTED":
		channel := map[string]string{"LINK": "a Link Referral", "IRL": "an IRL Referral"}[data.Text("kind")]
		if channel == "" {
			return n, payments.ErrFacts
		}
		n.Title = "Your invitation worked"
		n.Body = "Someone you invited through " + channel + " just made their first test gift."
	case "GIFT_PROCESSED":
		if data.Text("gift_id") == "" {
			return n, payments.ErrFacts
		}
		n.Title, n.Path = "Your gift was processed", "/account/gifts"
		n.Body = "Your test gift was processed. Open the app to see your giving history."
	case "STREAK_AT_RISK":
		n.Title, n.Path = "Your giving streak is at risk", paymentMethodPath
		n.Body = "Your monthly test payment was declined. Update your payment method to keep your streak going."
	case "STREAK_MILESTONE":
		days, err := data.Int("days")
		if err != nil || days < 1 || days > 1000000 {
			return n, payments.ErrFacts
		}
		n.Title, n.Body = milestoneWording(days)
	case "EVENT_PROMOTED":
		n.Title = "A spot opened up"
		n.Body = "You are off the waitlist — your spot at an upcoming event is confirmed. Open the app for details."
	case eventNearbyCategory:
		n.Title = "A new event near you"
		n.Body = "A volunteer event was just announced near you. Open the app for the details."
	case eventCancelledCategory:
		n.Title = "An event was cancelled"
		n.Body = "An event you signed up for has been cancelled. Open the app for the details."
	case "EVENT_REMINDER":
		n.Title = "Your event is coming up"
		if data.Text("window") == "2h" {
			n.Body = "An event you reserved starts in about 2 hours. Open the app for the details."
		} else {
			n.Body = "An event you reserved is tomorrow. Open the app for the details."
		}
	case "STREAK_LAPSED":
		n.Title, n.Path = "Your giving streak ended", paymentMethodPath
		n.Body = "Your monthly test payment did not recover in time, so your streak reset. Start a new streak whenever you are ready."
	case "BADGE_EARNED":
		if data.Text("code") == "" {
			return n, payments.ErrFacts
		}
		n.Title = "You earned a new badge"
		n.Body = "You just earned a badge for your giving. Open the app to see it."
	default:
		return n, payments.ErrFacts
	}
	return n, nil
}

type PushWorker struct {
	DB        *pgxpool.Pool
	Providers map[string]push.Provider
}

// pushTarget is one device a claimed notification goes to.
type pushTarget struct {
	session  pgtype.UUID
	provider push.Provider
	device   push.Device
}

type pushJob struct {
	queries.DonorPushNotification
	note    push.Notification
	targets []pushTarget
}

// pushOutcome is what the providers said about one delivery attempt.
type pushOutcome struct {
	sent     []pgtype.UUID
	dropped  []pushTarget
	retry    bool
	after    time.Duration
	rejected bool
}

// DeliverNext sends the next due push notification to each of its donor's
// devices it has not yet reached. It reports whether it found work.
func (w PushWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	job, worked, err := w.claim(ctx, now)
	if err != nil || job == nil {
		return worked, err
	}
	// The donor lock is released before the providers are called; the claim
	// keeps other workers off the job, and a resend after a crash replaces
	// the notification on the device rather than repeating it.
	var outcome pushOutcome
	for _, target := range job.targets {
		err := target.provider.Send(ctx, target.device, job.note)
		var unavailable push.Unavailable
		switch {
		case err == nil:
			outcome.sent = append(outcome.sent, target.session)
		case errors.Is(err, push.ErrUnregistered):
			outcome.dropped = append(outcome.dropped, target)
		case errors.Is(err, push.ErrRejected):
			outcome.rejected = true
		case errors.As(err, &unavailable):
			outcome.retry, outcome.after = true, max(outcome.after, unavailable.After)
		default:
			outcome.retry = true
		}
	}
	return true, w.record(ctx, job, outcome, now)
}

// claim selects the next due notification and, unless it should no longer
// be sent or has no device to reach, reserves it for one delivery attempt.
// It returns no job when the one found was finished or failed instead.
func (w PushWorker) claim(ctx context.Context, now time.Time) (*pushJob, bool, error) {
	var job *pushJob
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		row, err := q.NextPushNotification(ctx, timestamp(now))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		donor, err := q.LockDonor(ctx, row.DonorID)
		if err != nil {
			return err
		}
		reason, err := pushSuppression(ctx, q, row, donor, now)
		var note push.Notification
		if err == nil && reason == "" {
			note, err = pushContent(row)
			if err == nil && row.Category == "STREAK_MILESTONE" {
				err = customMilestone(ctx, q, row, &note)
			}
		}
		if err != nil {
			if !messageDataError(err) {
				return err
			}
			return q.FailPushNotification(
				ctx,
				queries.FailPushNotificationParams{
					ID:          row.ID,
					LastError:   text("InvalidNotification"),
					AvailableAt: messageRetryAt(now, row.Attempts+1),
					Attempts:    row.Attempts + 1,
				},
			)
		}
		if reason != "" {
			return completePush(ctx, q, row.ID, now, reason)
		}
		devices, err := q.PushTargets(ctx, queries.PushTargetsParams{DonorID: row.DonorID, AsOf: timestamp(now), SentTo: row.SentTo})
		if err != nil {
			return err
		}
		claimed := &pushJob{DonorPushNotification: row, note: note}
		for _, d := range devices {
			if provider := w.Providers[d.Platform.String]; provider != nil {
				claimed.targets = append(
					claimed.targets,
					pushTarget{d.ID, provider, push.Device{Token: d.PushToken.String, Environment: d.PushEnvironment.String}},
				)
			}
		}
		switch {
		case len(devices) == 0 && len(row.SentTo) == 0:
			return completePush(ctx, q, row.ID, now, "no_device")
		case len(devices) == 0:
			return completePush(ctx, q, row.ID, now, "")
		case len(claimed.targets) == 0:
			return completePush(ctx, q, row.ID, now, "push_unconfigured")
		}
		job = claimed
		return q.ClaimPushNotification(ctx, queries.ClaimPushNotificationParams{ID: row.ID, AvailableAt: timestamp(now.Add(deliveryLease))})
	})
	if err != nil {
		return nil, false, err
	}
	return job, worked, nil
}

// record applies one attempt's outcome: reached sessions are not sent to
// again, rejected tokens are dropped, and the job completes, retries with
// backoff, or parks for an operator when a provider refused its
// configuration.
func (w PushWorker) record(ctx context.Context, job *pushJob, outcome pushOutcome, now time.Time) error {
	return pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		if len(outcome.sent) > 0 {
			if err := q.RecordPushSent(ctx, queries.RecordPushSentParams{ID: job.ID, Sessions: outcome.sent}); err != nil {
				return err
			}
		}
		for _, target := range outcome.dropped {
			if err := q.DropPushToken(ctx, queries.DropPushTokenParams{ID: target.session, PushToken: text(target.device.Token)}); err != nil {
				return err
			}
		}
		attempt := job.Attempts + 1
		switch {
		case outcome.rejected:
			return q.FailPushNotification(
				ctx,
				queries.FailPushNotificationParams{
					ID:          job.ID,
					LastError:   text("ProviderRejected"),
					AvailableAt: timestamp(now),
					Attempts:    maxDeliveryAttempts,
				},
			)
		case outcome.retry:
			retryAt := messageRetryAt(now, attempt)
			if later := now.Add(outcome.after); later.After(retryAt.Time) {
				retryAt = timestamp(later)
			}
			return q.FailPushNotification(
				ctx,
				queries.FailPushNotificationParams{ID: job.ID, LastError: text("DeliveryError"), AvailableAt: retryAt, Attempts: attempt},
			)
		case len(outcome.sent) == 0 && len(job.SentTo) == 0:
			return completePush(ctx, q, job.ID, now, "unregistered")
		}
		return completePush(ctx, q, job.ID, now, "")
	})
}

func completePush(ctx context.Context, q *queries.Queries, id pgtype.UUID, now time.Time, reason string) error {
	return q.CompletePushNotification(
		ctx,
		queries.CompletePushNotificationParams{
			ID:               id,
			CompletedAt:      timestamp(now),
			SuppressedReason: pgtype.Text{String: reason, Valid: reason != ""},
		},
	)
}
