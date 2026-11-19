package store

import (
	"context"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// ReportingService combines committed financial facts with consented journey
// observations. Missing observations remain unavailable rather than becoming zeros.
type ReportingService struct {
	Staff *StaffService
}

// reportReadPermission gates the report. It is held by every staff role (see
// rolePermissions): the org-wide KPIs inform each operator's area, and the read
// grants no mutation.
const reportReadPermission = "report.read"

// shareProxyUnavailable explains a window without consented share observations.
const shareProxyUnavailable = "no consented share-action observations exist in this reporting window; " +
	"this metric cannot be computed from server facts"

// Metric is one §4 number with its availability. A metric the server cannot
// compute yet carries Available=false and a Reason, so the report never shows a
// fabricated zero for a deferred browser metric.
type Metric struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Value     int64  `json:"value"`
}

// SourceConversions is the first-gift conversion count and gross collected
// dollars for one attribution source (DIRECT/LINK/IRL), split by gift
// frequency so first-gift donor mix is visible without conflating gifts and
// donors.
type SourceConversions struct {
	Source     string `json:"source"`
	Frequency  string `json:"frequency"`
	Donors     int64  `json:"donors"`
	GrossMinor int64  `json:"gross_minor"`
}

// GiftMix is the successful gift count and collected dollars for one frequency.
type GiftMix struct {
	Frequency      string `json:"frequency"`
	Gifts          int64  `json:"gifts"`
	CollectedMinor int64  `json:"collected_minor"`
}

// ViralCoefficient reports gross and currently-qualified conversions over the
// first-gift donor cohort separately, so refund behaviour cannot silently rewrite the
// acquisition number. The ratio is left to the reader (cohort may be zero).
type ViralCoefficient struct {
	Cohort               int64 `json:"cohort"`
	GrossConversions     int64 `json:"gross_conversions"`
	QualifiedConversions int64 `json:"qualified_conversions"`
}

// InvoluntaryChurn is the lost scheduled value from failed renewals past the
// retry deadline, with the recovered count/value against that same cohort.
type InvoluntaryChurn struct {
	Available            bool   `json:"available"`
	Reason               string `json:"reason,omitempty"`
	MissingHistory       int64  `json:"missing_history"`
	RecoveredWithinGrace int64  `json:"recovered_within_grace"`
	FailedPeriods        int64  `json:"failed_periods"`
	LostMinor            int64  `json:"lost_minor"`
	RecoveredPeriods     int64  `json:"recovered_periods"`
	RecoveredMinor       int64  `json:"recovered_minor"`
}

// Retention is historical survival, with M3/M6 retained as aliases for 90/180 days.
type Retention struct {
	Available  bool                 `json:"available"`
	Reason     string               `json:"reason,omitempty"`
	Milestones []RetentionMilestone `json:"milestones"`
	ReachedM3  int64                `json:"reached_m3"`
	RetainedM3 int64                `json:"retained_m3"`
	ReachedM6  int64                `json:"reached_m6"`
	RetainedM6 int64                `json:"retained_m6"`
}

type RetentionMilestone struct {
	Days     int32 `json:"days"`
	Eligible int64 `json:"eligible"`
	Retained int64 `json:"retained"`
	Unknown  int64 `json:"unknown"`
}

// TimeToFirstGift requires an observed landing linked to a verified first gift.
type TimeToFirstGift struct {
	Available     bool   `json:"available"`
	Reason        string `json:"reason,omitempty"`
	Sample        int64  `json:"sample"`
	MedianSeconds int64  `json:"median_seconds"`
}

// Report is the §4 success-metrics snapshot over a half-open [start, end)
// window. PeriodStart/End and ObservationCutoff are echoed so the numbers carry
// their window and the as-of time (analytics.md requires periods to state their
// observation cutoff and timezone; all timestamps are UTC). CoverageNote
// discloses the committed-facts coverage boundary.
type Report struct {
	PeriodStart       time.Time           `json:"period_start"`
	PeriodEnd         time.Time           `json:"period_end"`
	ObservationCutoff time.Time           `json:"observation_cutoff"`
	Timezone          string              `json:"timezone"`
	CoverageNote      string              `json:"coverage_note"`
	FoundingDonors    int64               `json:"founding_donors"`
	FirstGiftBySource []SourceConversions `json:"first_gift_by_source"`
	GiftMix           []GiftMix           `json:"gift_mix"`
	ViralCoefficient  ViralCoefficient    `json:"viral_coefficient"`
	ReferralLinkMinor int64               `json:"referral_link_minor"`
	ReferralIRLMinor  int64               `json:"referral_irl_minor"`
	InvoluntaryChurn  InvoluntaryChurn    `json:"involuntary_churn"`
	TimeToFirstGift   TimeToFirstGift     `json:"time_to_first_gift"`
	Retention         Retention           `json:"retention"`
	SevenDayShareRate Metric              `json:"seven_day_share_rate"`
}

const coverageNote = "Computed from committed server facts (gifts, billing periods, refunds, projection) as of the " +
	"observation cutoff, with consented journey observations for time to first gift and seven-day sharing. " +
	"Metrics without sufficient observations are unavailable."

// Report computes the §4 metrics over [start, end). end also serves as the
// observation cutoff: the involuntary-churn deadline (due_at + 7 days) is
// measured against it, so a failed renewal still inside its retry window is not
// yet counted as lost. It is a pure read, so one snapshotted read transaction
// gives every metric a consistent view.
func (s *ReportingService) Report(ctx context.Context, token string, start, end time.Time) (Report, error) {
	if end.Before(start) || end.Equal(start) {
		return Report{}, ErrStaffInput
	}
	start, end = start.UTC(), end.UTC()
	return transact(ctx, s.Staff.DB, readSnapshot, func(tx pgx.Tx) (Report, error) {
		q := queries.New(tx)
		if _, err := requireStaff(ctx, q, token, reportReadPermission, time.Now().UTC()); err != nil {
			return Report{}, err
		}
		startTS, endTS := timestamp(start), timestamp(end)
		report := Report{
			PeriodStart:       start,
			PeriodEnd:         end,
			ObservationCutoff: end,
			Timezone:          "UTC",
			CoverageNote:      coverageNote,
			SevenDayShareRate: Metric{Available: false, Reason: shareProxyUnavailable},
			TimeToFirstGift: TimeToFirstGift{
				Reason: "No observed checkout journey has a verified first gift in this reporting window; account creation is not a landing timestamp.",
			},
		}

		founding, err := q.ReportFoundingDonors(
			ctx,
			queries.ReportFoundingDonorsParams{FoundingAwardedAt: startTS, FoundingAwardedAt_2: endTS},
		)
		if err != nil {
			return Report{}, err
		}
		report.FoundingDonors = founding

		sources, err := q.ReportFirstGiftBySource(ctx, queries.ReportFirstGiftBySourceParams{PaidAt: startTS, PaidAt_2: endTS})
		if err != nil {
			return Report{}, err
		}
		report.FirstGiftBySource = make([]SourceConversions, 0, len(sources))
		for _, row := range sources {
			source, _ := row.Source.(string)
			report.FirstGiftBySource = append(report.FirstGiftBySource, SourceConversions{
				Source:     source,
				Frequency:  row.Frequency,
				Donors:     row.Donors,
				GrossMinor: row.GrossMinor,
			})
		}

		mix, err := q.ReportGiftMix(ctx, queries.ReportGiftMixParams{PaidAt: startTS, PaidAt_2: endTS})
		if err != nil {
			return Report{}, err
		}
		report.GiftMix = make([]GiftMix, 0, len(mix))
		for _, row := range mix {
			report.GiftMix = append(report.GiftMix, GiftMix{Frequency: row.Frequency, Gifts: row.Gifts, CollectedMinor: row.CollectedMinor})
		}

		viral, err := q.ReportViralCoefficient(ctx, queries.ReportViralCoefficientParams{StartAt: startTS, EndAt: endTS, Cutoff: endTS})
		if err != nil {
			return Report{}, err
		}
		report.ViralCoefficient = ViralCoefficient{
			Cohort:               viral.Cohort,
			GrossConversions:     viral.GrossConversions,
			QualifiedConversions: viral.QualifiedConversions,
		}

		dollars, err := q.ReportReferralDollars(ctx, queries.ReportReferralDollarsParams{StartAt: startTS, EndAt: endTS, Cutoff: endTS})
		if err != nil {
			return Report{}, err
		}
		report.ReferralLinkMinor = dollars.LinkMinor
		report.ReferralIRLMinor = dollars.IrlMinor

		churn, err := q.ReportInvoluntaryChurn(ctx, queries.ReportInvoluntaryChurnParams{StartAt: startTS, EndAt: endTS, Cutoff: endTS})
		if err != nil {
			return Report{}, err
		}
		report.InvoluntaryChurn = InvoluntaryChurn{
			Available:            churn.MissingHistory == 0 && churn.InconsistentPeriods == 0,
			MissingHistory:       churn.MissingHistory,
			RecoveredWithinGrace: churn.RecoveredWithinGrace,
			FailedPeriods:        churn.FailedPeriods,
			LostMinor:            churn.LostMinor,
			RecoveredPeriods:     churn.RecoveredPeriods,
			RecoveredMinor:       churn.RecoveredMinor,
		}
		if !report.InvoluntaryChurn.Available {
			report.InvoluntaryChurn.Reason = "Billing history is missing or contains inconsistent invoice amounts; totals cover verified observations only."
		}

		retention, err := q.ReportRetention(ctx, queries.ReportRetentionParams{StartAt: startTS, EndAt: endTS, Cutoff: endTS})
		if err != nil {
			return Report{}, err
		}
		missing, err := q.ReportRetentionMissing(ctx, queries.ReportRetentionMissingParams{PaidAt: startTS, PaidAt_2: endTS})
		if err != nil {
			return Report{}, err
		}
		report.Retention = Retention{Available: !missing, Milestones: []RetentionMilestone{}}
		for _, row := range retention {
			report.Retention.Milestones = append(report.Retention.Milestones, RetentionMilestone{
				Days: row.Days, Eligible: row.Eligible, Retained: row.Retained, Unknown: row.Unknown,
			})
			if row.Unknown > 0 {
				report.Retention.Available = false
			}
			switch row.Days {
			case 90:
				report.Retention.ReachedM3, report.Retention.RetainedM3 = row.Eligible, row.Retained
			case 180:
				report.Retention.ReachedM6, report.Retention.RetainedM6 = row.Eligible, row.Retained
			}
		}
		if !report.Retention.Available {
			report.Retention.Reason = "Historical survival is incomplete; run the projections worker to backfill milestones. Unknown outcomes are excluded from retained totals."
		}

		journeys, err := q.JourneyReport(ctx, queries.JourneyReportParams{StartAt: startTS, EndAt: endTS})
		if err != nil {
			return Report{}, err
		}
		if journeys.Sample > 0 {
			report.TimeToFirstGift = TimeToFirstGift{Available: true, Sample: journeys.Sample, MedianSeconds: journeys.MedianSeconds}
		}
		share, err := q.SevenDayShareReport(ctx, queries.SevenDayShareReportParams{StartAt: startTS, EndAt: endTS})
		if err != nil {
			return Report{}, err
		}
		if share.Eligible > 0 {
			report.SevenDayShareRate = Metric{Available: true, Value: share.Shared * 100 / share.Eligible}
		}
		return report, nil
	})
}
