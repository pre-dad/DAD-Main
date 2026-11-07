package store

import (
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"dad/donor-api/internal/badges"
	"dad/donor-api/internal/recognition"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type history struct {
	Gifts   []recognition.Gift
	Refunds []recognition.Refund
	Periods []recognition.Period
}

func loadHistory(ctx context.Context, q *queries.Queries, donorID pgtype.UUID) (history, error) {
	var result history
	gifts, err := q.GiftHistory(ctx, donorID)
	if err != nil {
		return result, err
	}
	for _, g := range gifts {
		gift := recognition.Gift{
			ID:        g.StripeObjectID,
			DonorID:   g.DonorID.String(),
			Amount:    g.AmountMinor,
			Currency:  g.Currency,
			Frequency: g.Frequency,
			PaidAt:    g.PaidAt.Time,
		}
		if g.PeriodID.Valid {
			gift.PeriodID = g.PeriodID.String()
		}
		if g.ReferrerID.Valid {
			if !g.ReferralKind.Valid || !g.ReferralExpiresAt.Valid {
				return result, recognition.ErrInvalid
			}
			gift.Candidate = &recognition.Candidate{
				ReferrerID: g.ReferrerID.String(),
				Kind:       g.ReferralKind.String,
				ExpiresAt:  g.ReferralExpiresAt.Time,
			}
		}
		result.Gifts = append(result.Gifts, gift)
	}
	refunds, err := q.RefundHistory(ctx, donorID)
	if err != nil {
		return result, err
	}
	for _, r := range refunds {
		result.Refunds = append(
			result.Refunds,
			recognition.Refund{
				ID:        r.StripeRefundID,
				GiftID:    r.StripeObjectID,
				Amount:    r.AmountMinor,
				Currency:  r.Currency,
				Status:    r.Status,
				UpdatedAt: r.ObservedAt.Time,
			},
		)
	}
	periods, err := q.PeriodHistory(ctx, donorID)
	if err != nil {
		return result, err
	}
	for _, p := range periods {
		status := "EXPECTED"
		if p.Status == "FAILED" {
			status = "FAILED"
		}
		result.Periods = append(
			result.Periods,
			recognition.Period{ID: p.ID.String(), DonorID: p.DonorID.String(), DueAt: p.DueAt.Time, EndsAt: p.EndsAt.Time, Status: status},
		)
	}
	return result, nil
}

// latestMonthlyMinor is the amount of the most recent MONTHLY gift -- the
// donor's current pledge -- which annualizes to their network contribution.
// Returns 0 when there is no monthly gift.
func latestMonthlyMinor(gifts []recognition.Gift) int64 {
	var amount int64
	var latest time.Time
	for _, g := range gifts {
		if g.Frequency != "MONTHLY" {
			continue
		}
		if g.PaidAt.After(latest) {
			latest = g.PaidAt
			amount = g.Amount
		}
	}
	return amount
}

// historicalBest replays refund observations made by asOf so a full refund
// cannot erase a best streak the donor had already attained.
func historicalBest(
	ctx context.Context,
	id pgtype.UUID,
	h history,
	observations []queries.RefundObservationsRow,
	now time.Time,
	previous int64,
	stops []time.Time,
) (int64, error) {
	byID := map[string]recognition.Refund{}
	for _, r := range h.Refunds {
		byID[r.ID] = r
	}
	observed := map[string]bool{}
	history := []recognition.Refund{}
	for _, o := range observations {
		r, ok := byID[o.StripeRefundID]
		if !ok {
			return 0, recognition.ErrInvalid
		}
		r.Status = o.Status
		r.UpdatedAt = o.ObservedAt.Time
		observed[r.ID] = true
		history = append(history, r)
	}
	for _, r := range h.Refunds {
		if !observed[r.ID] && !r.UpdatedAt.After(now) {
			history = append(history, r)
		}
	}
	return replayBest(ctx, id.String(), h, history, now, previous, stops)
}

// refundsAsOf returns each refund's state as of asOf. A refund last updated
// after asOf, as another process with a later clock may record, takes its
// latest earlier observation; the later update is returned as a refresh boundary.
func refundsAsOf(
	current []recognition.Refund,
	observations []queries.RefundObservationsRow,
	asOf time.Time,
) ([]recognition.Refund, []time.Time) {
	latest := map[string]queries.RefundObservationsRow{}
	for _, o := range observations {
		latest[o.StripeRefundID] = o
	}
	result := make([]recognition.Refund, 0, len(current))
	var later []time.Time
	for _, r := range current {
		if r.UpdatedAt.After(asOf) {
			later = append(later, r.UpdatedAt)
			o, ok := latest[r.ID]
			if !ok {
				continue
			}
			r.Status, r.UpdatedAt = o.Status, o.ObservedAt.Time
		}
		result = append(result, r)
	}
	return result, later
}

func replayBest(
	ctx context.Context,
	donorID string,
	h history,
	history []recognition.Refund,
	now time.Time,
	previous int64,
	stops []time.Time,
) (int64, error) {
	slices.SortStableFunc(history, func(a, b recognition.Refund) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	gifts := slices.Clone(h.Gifts)
	slices.SortStableFunc(gifts, func(a, b recognition.Gift) int { return a.PaidAt.Compare(b.PaidAt) })
	points := map[time.Time]bool{}
	for _, g := range h.Gifts {
		if !g.PaidAt.After(now) {
			points[g.PaidAt.UTC()] = true
		}
	}
	for _, r := range history {
		points[r.UpdatedAt.UTC()] = true
	}
	ordered := slices.SortedFunc(maps.Keys(points), time.Time.Compare)
	best := previous
	latest := map[string]recognition.Refund{}
	paidPeriods := map[string]bool{}
	refundIndex, giftIndex := 0, 0
	for _, point := range ordered {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		for refundIndex < len(history) && !history[refundIndex].UpdatedAt.After(point) {
			r := history[refundIndex]
			latest[r.ID] = r
			refundIndex++
		}
		for giftIndex < len(gifts) && !gifts[giftIndex].PaidAt.After(point) {
			g := gifts[giftIndex]
			if g.DonorID == donorID && g.Frequency == "MONTHLY" {
				paidPeriods[g.PeriodID] = true
			}
			giftIndex++
		}
		// No history at this point can exceed its number of paid recurring periods.
		if int64(len(paidPeriods)) <= best {
			continue
		}
		refunds := slices.Collect(maps.Values(latest))
		value, err := recognition.GivingStreak(h.Gifts, refunds, h.Periods, donorID, point, best, stops)
		if err != nil && !errors.Is(err, recognition.ErrIncomplete) {
			return 0, err
		}
		if err == nil && value.Best > best {
			best = value.Best
		}
	}
	return best, nil
}

// projection is a donor's saved recognition with the start of the current
// streak run, which identifies the run but is not stored.
type projection struct {
	queries.SaveProjectionParams
	streakSince *time.Time
	donor       queries.Donor
	zone        *time.Location
}

// projectionRules versions the recognition rules. Raising it queues every
// projection computed under an earlier version for recomputation.
const projectionRules = 12

// A projection waits at most refreshCap for its next time boundary. Facts that
// change a projection refresh it when they arrive, so this is a backstop, not a
// schedule.
const (
	minRefreshInterval = 25 * 24 * time.Hour
	maxRefreshInterval = 35 * 24 * time.Hour
)

// refreshCap is the donor's own wait between minRefreshInterval and
// maxRefreshInterval, taken from the random part of their ID, so projections
// computed together, as after a rules version bump, fall due over ten days
// rather than at once.
func refreshCap(donor pgtype.UUID) time.Duration {
	spread := uint64((maxRefreshInterval - minRefreshInterval) / time.Second)
	return minRefreshInterval + time.Duration(binary.BigEndian.Uint64(donor.Bytes[8:])%spread)*time.Second
}

// projectFacts recomputes and saves the donor's recognition projection
// under the donor lock.
func projectFacts(ctx context.Context, q *queries.Queries, id pgtype.UUID, now time.Time) (projection, error) {
	out := queries.SaveProjectionParams{
		DonorID:       id,
		ComputedAt:    timestamp(now),
		NextRefreshAt: timestamp(now.Add(refreshCap(id))),
		RulesVersion:  projectionRules,
	}
	donor, err := q.LockDonor(ctx, id)
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	zone := recognition.Zone(donor.Timezone.String)
	if err := seedProjection(ctx, q, id, now); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	// A referred donor adjusts this row without taking this donor's lock.
	// Lock it before reading the aggregate so a later save cannot erase a delta.
	old, err := q.LockProjection(ctx, id)
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	out.ID = old.ID
	out.FoundingAwardedAt = old.FoundingAwardedAt
	out.BestStreak = old.BestStreak
	h, err := loadHistory(ctx, q, id)
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	if err = withholdSharedReferrals(ctx, q, id, h.Gifts); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	observations, err := q.RefundObservations(ctx, queries.RefundObservationsParams{DonorID: id, ObservedAt: timestamp(now)})
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	refunds, refundBoundaries := refundsAsOf(h.Refunds, observations, now)
	claimed := false
	if !out.FoundingAwardedAt.Valid {
		if claimed, err = q.FoundingClaimed(ctx, id); err != nil {
			return projection{SaveProjectionParams: out}, err
		}
	}
	if err = projectConversion(&out, h, id, claimed, now); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	if out.NetMinor, err = netMinor(h.Gifts, refunds, now); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	stops, err := renewalStops(ctx, q, id)
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	if err = saveRetention(ctx, q, id, h, observations, stops, zone, now); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	best, err := historicalBest(ctx, id, h, observations, now, int64(out.BestStreak), stops)
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	if best > math.MaxInt32 {
		return projection{SaveProjectionParams: out}, recognition.ErrInvalid
	}
	out.BestStreak = int32(best)
	resetAt, err := q.LatestStreakReset(ctx, queries.LatestStreakResetParams{DonorID: id, AsOf: timestamp(now)})
	if err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	streakGifts := h.Gifts
	if resetAt.Valid {
		streakGifts = slices.DeleteFunc(slices.Clone(h.Gifts), func(g recognition.Gift) bool { return !g.PaidAt.After(resetAt.Time) })
	}
	streak, err := recognition.GivingStreak(streakGifts, refundsForGifts(refunds, streakGifts), h.Periods, id.String(), now, best, stops)
	switch {
	case errors.Is(err, recognition.ErrIncomplete):
		out.NextRefreshAt = timestamp(now.Add(5 * time.Minute))
	case err != nil:
		return projection{SaveProjectionParams: out}, err
	default:
		if streak.Current > math.MaxInt32 || streak.Best > math.MaxInt32 {
			return projection{SaveProjectionParams: out}, recognition.ErrInvalid
		}
		out.CurrentStreak = pgtype.Int4{Int32: int32(streak.Current), Valid: true}
		out.BestStreak = int32(streak.Best)
		out.AtRisk = streak.AtRisk
		if streak.GraceEndsAt != nil {
			out.GraceEndsAt = timestamp(*streak.GraceEndsAt)
		}
		if streak.Since != nil {
			out.StreakSince = timestamp(*streak.Since)
		}
		// N-1/I-2 network inputs: a donor counts toward a referrer's active
		// network, at their current monthly pledge, exactly while their own
		// streak is alive (a live run has a start and a positive length). A
		// lapsed donor's run has reset to zero and drops out of the network
		// total until they give again.
		currentGift := streak
		if resetAt.Valid {
			natural, e := recognition.GivingStreak(h.Gifts, refunds, h.Periods, id.String(), now, best, stops)
			if e != nil && !errors.Is(e, recognition.ErrIncomplete) {
				return projection{}, e
			}
			if e == nil {
				currentGift = natural
			}
		}
		out.Active = currentGift.Since != nil && currentGift.Current > 0
		if out.Active {
			out.CurrentMonthlyMinor = latestMonthlyMinor(h.Gifts)
		}
	}
	for _, g := range h.Gifts {
		if g.PaidAt.After(now) || g.PaidAt.Before(now.AddDate(0, -13, 0)) {
			continue
		}
		if err := recordServerEvent(ctx, q, id, "gift_completed", "gift:"+g.ID, g.PaidAt); err != nil {
			return projection{}, err
		}
	}

	if old.AtRisk != out.AtRisk && out.AtRisk {
		if err := recordServerEvent(ctx, q, id, "gift_failed", "gift-failed:"+id.String()+":"+out.GraceEndsAt.Time.String(), now); err != nil {
			return projection{}, err
		}
	}
	if old.AtRisk && !out.AtRisk && out.Active {
		if err := recordServerEvent(ctx, q, id, "gift_recovered", "gift-recovered:"+id.String()+":"+old.GraceEndsAt.Time.String(), now); err != nil {
			return projection{}, err
		}
	}
	if old.Active && !out.Active && !out.AtRisk {
		if err := recordServerEvent(ctx, q, id, "gift_lapsed", "gift-lapsed:"+id.String()+":"+old.StreakSince.Time.String(), now); err != nil {
			return projection{}, err
		}
	}
	rules, err := recognitionRules(ctx, q)
	if err != nil {
		return projection{}, err
	}
	out.NextRefreshAt = nextRefresh(out, h, refundBoundaries, stops, now, zone, rules.Milestones)
	if err = recomputeReferredTotals(ctx, q, id, &out); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	if err = applyReferralDelta(ctx, q, old, out, now); err != nil {
		return projection{SaveProjectionParams: out}, err
	}
	// A donor converts at most one referral (R-3), so a later attribution
	// correction moves the credit without counting a second conversion.
	if out.ReferrerID.Valid {
		if err = recordServerEvent(ctx, q, id, "referral_converted", "referral:"+id.String(), now); err != nil {
			return projection{SaveProjectionParams: out}, err
		}
	}
	if old.FirstGiftID.Valid &&
		(old.FirstGiftID != out.FirstGiftID || old.ReferrerID != out.ReferrerID || old.ReferralKind != out.ReferralKind) {
		if err = q.InsertReferralCorrection(ctx, queries.InsertReferralCorrectionParams{
			DonorID: id, OldFirstGiftID: old.FirstGiftID.String, NewFirstGiftID: out.FirstGiftID,
			OldReferrerID: old.ReferrerID, NewReferrerID: out.ReferrerID,
			OldKind: old.ReferralKind, NewKind: out.ReferralKind,
			RecordedAt: timestamp(now), RulesVersion: projectionRules,
		}); err != nil {
			return projection{SaveProjectionParams: out}, err
		}
	}
	return projection{out, streak.Since, donor, zone}, q.SaveProjection(ctx, out)
}

// recomputeReferredTotals reconciles the locked row with committed referred
// projections. Writers of those projections lock this row before saving theirs.
func recomputeReferredTotals(ctx context.Context, q *queries.Queries, id pgtype.UUID, out *queries.SaveProjectionParams) error {
	totals, err := q.ReferralTotalsFromReferred(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		out.ReferredLinkCount, out.ReferredIrlCount, out.ReferredLinkMinor, out.ReferredIrlMinor = 0, 0, 0, 0
		return nil
	}
	if err != nil {
		return err
	}
	out.ReferredLinkCount = totals.LinkCount
	out.ReferredIrlCount = totals.IrlCount
	out.ReferredLinkMinor = totals.LinkMinor
	out.ReferredIrlMinor = totals.IrlMinor
	return nil
}

// referralContribution is a referred donor's contribution to its referrer's
// four denormalized dashboard totals: it mirrors the FILTER aggregate exactly
// (count only when net_minor>0; the minor sum carries net_minor, which the
// schema constrains to >= 0). An unset or non-LINK/IRL kind, or an invalid
// net, contributes nothing.
func referralContribution(referrer pgtype.UUID, kind pgtype.Text, net pgtype.Int8) (linkCount, irlCount, linkMinor, irlMinor int64) {
	if !referrer.Valid || !kind.Valid || !net.Valid {
		return 0, 0, 0, 0
	}
	switch kind.String {
	case "LINK":
		linkMinor = net.Int64
		if net.Int64 > 0 {
			linkCount = 1
		}
	case "IRL":
		irlMinor = net.Int64
		if net.Int64 > 0 {
			irlCount = 1
		}
	}
	return linkCount, irlCount, linkMinor, irlMinor
}

// applyReferralDelta transfers contributions atomically with the projection,
// and awards the referrer the referral badges their new counts reach.
func applyReferralDelta(
	ctx context.Context,
	q *queries.Queries,
	old queries.DonorMetricProjection,
	out queries.SaveProjectionParams,
	now time.Time,
) error {
	deltas := map[pgtype.UUID]queries.ApplyReferralDeltaParams{}
	add := func(referrer pgtype.UUID, kind pgtype.Text, net pgtype.Int8, sign int64) {
		if !referrer.Valid {
			return
		}
		link, irl, linkMinor, irlMinor := referralContribution(referrer, kind, net)
		d := deltas[referrer]
		d.DonorID = referrer
		d.ReferredLinkCount += sign * link
		d.ReferredIrlCount += sign * irl
		d.ReferredLinkMinor += sign * linkMinor
		d.ReferredIrlMinor += sign * irlMinor
		deltas[referrer] = d
	}
	add(old.ReferrerID, old.ReferralKind, old.NetMinor, -1)
	add(out.ReferrerID, out.ReferralKind, out.NetMinor, 1)
	// Lock both recipients in a stable order when attribution changes owners.
	for _, id := range slices.SortedFunc(maps.Keys(deltas), func(a, b pgtype.UUID) int { return strings.Compare(a.String(), b.String()) }) {
		d := deltas[id]
		if d.ReferredLinkCount == 0 && d.ReferredIrlCount == 0 && d.ReferredLinkMinor == 0 && d.ReferredIrlMinor == 0 {
			continue
		}
		if err := seedProjection(ctx, q, id, now); err != nil {
			return err
		}
		counts, err := q.ApplyReferralDelta(ctx, d)
		if err != nil {
			return err
		}
		referrals := badges.Signals{
			ConvertedReferrals: counts.ReferredLinkCount + counts.ReferredIrlCount,
			HasIRLReferral:     counts.ReferredIrlCount > 0,
		}
		rules, err := recognitionRules(ctx, q)
		if err != nil {
			return err
		}
		for _, rule := range rules.Badges {
			if !ruleEarned(rule, referrals) {
				continue
			}
			if err := earnBadge(ctx, q, id, rule.Code, pgtype.Text{}, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// nextRefresh is the earliest moment after now at which the projection can
// change without new facts arriving, and at most its default refresh time. The
// next streak milestone is one, so its badge and notice arrive on the day.
func nextRefresh(
	out queries.SaveProjectionParams,
	h history,
	refundBoundaries, stops []time.Time,
	now time.Time,
	zone *time.Location,
	configured ...[]int64,
) pgtype.Timestamptz {
	boundaries := slices.Concat(refundBoundaries, stops)
	if out.StreakSince.Valid {
		run := recognition.Streak{Since: &out.StreakSince.Time}
		milestones := recognition.LadderMilestones
		if len(configured) > 0 {
			milestones = configured[0]
		}
		if next := run.LadderWithMilestones(now, zone, milestones).NextMilestone; next != nil {
			boundaries = append(boundaries, run.Reaches(*next, zone))
		}
	}
	for _, p := range h.Periods {
		boundaries = append(boundaries, p.DueAt, p.EndsAt, p.EndsAt.Add(recognition.Grace))
	}
	for _, g := range h.Gifts {
		boundaries = append(boundaries, g.PaidAt)
	}
	if out.GraceEndsAt.Valid {
		boundaries = append(boundaries, out.GraceEndsAt.Time)
	}
	// A year's summary is queued once that year ends in New York.
	if len(h.Gifts) > 0 {
		boundaries = append(boundaries, time.Date(now.In(recognition.NewYork).Year()+1, 1, 1, 0, 0, 0, 0, recognition.NewYork))
	}
	next := out.NextRefreshAt
	for _, b := range boundaries {
		if b.After(now) && b.Before(next.Time) {
			next = timestamp(b)
		}
	}
	return next
}

// Arrival order must not determine attribution. The founding award stays earned.
// A donor on the website waitlist (claimed, D-6) earns it when matched.
func projectConversion(out *queries.SaveProjectionParams, h history, id pgtype.UUID, claimed bool, now time.Time) error {
	relations, err := recognition.Relationships(h.Gifts, now)
	if err != nil {
		return err
	}
	out.FirstGiftID, out.ReferrerID, out.ReferralKind = pgtype.Text{}, pgtype.UUID{}, pgtype.Text{}
	if relation, ok := relations[id.String()]; ok {
		out.FirstGiftID = text(relation.FirstGiftID)
		if relation.ReferrerID != nil {
			if err = out.ReferrerID.Scan(*relation.ReferrerID); err != nil {
				return err
			}
			out.ReferralKind = text(*relation.Kind)
		}
	}
	if out.FoundingAwardedAt.Valid {
		return nil
	}
	at, qualified, err := recognition.FoundingQualifiedAt(h.Gifts, id.String(), now)
	switch {
	case qualified:
		out.FoundingAwardedAt = timestamp(at)
	case claimed:
		out.FoundingAwardedAt = timestamp(now)
	}
	return err
}

func netMinor(gifts []recognition.Gift, refunds []recognition.Refund, now time.Time) (pgtype.Int8, error) {
	net, err := recognition.NetGifts(gifts, refunds, now)
	if err != nil {
		return pgtype.Int8{}, err
	}
	total := int64(0)
	for _, amount := range net {
		if amount > math.MaxInt64-total {
			return pgtype.Int8{}, recognition.ErrInvalid
		}
		total += amount
	}
	return pgtype.Int8{Int64: total, Valid: true}, nil
}

// renewalStops returns the renewal boundaries ended by cancellation.
func renewalStops(ctx context.Context, q *queries.Queries, id pgtype.UUID) ([]time.Time, error) {
	ended, err := q.StoppedPeriods(ctx, id)
	if err != nil {
		return nil, err
	}
	stops := make([]time.Time, 0, len(ended))
	for _, v := range ended {
		stops = append(stops, v.Time)
	}
	return stops, nil
}

func refundsForGifts(refunds []recognition.Refund, gifts []recognition.Gift) []recognition.Refund {
	ids := map[string]bool{}
	for _, g := range gifts {
		ids[g.ID] = true
	}
	return slices.DeleteFunc(slices.Clone(refunds), func(r recognition.Refund) bool { return !ids[r.GiftID] })
}
