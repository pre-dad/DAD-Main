package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"strconv"
	"time"

	"dad/donor-api/internal/partnerreport"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// minimumGroup is the smallest number of employees any partner figure may
// describe (EM-3); a smaller group is suppressed.
const minimumGroup = 5

// PartnerOverview is the §7.3 dashboard header (P-10). Figures are published
// and verified only, never estimated. EmployeesEnrolled and EnrolledPercent
// are nil while fewer than minimumGroup employees are linked, and
// EnrolledPercent also without a workforce size. UpdatedAt is when any figure
// last changed, nil before the partner has a published initiative. Role is the
// caller's own, so a client can offer only the actions it may take (§7.2).
// UserID is the calling user's own id, so a client can recognise itself in the
// users list (an admin cannot remove themselves).
type PartnerOverview struct {
	HasLogo               bool       `json:"has_logo"`
	CommittedVerified     bool       `json:"committed_verified"`
	DeployedVerified      bool       `json:"deployed_verified"`
	PeopleReachedVerified bool       `json:"people_reached_verified"`
	UserID                string     `json:"user_id"`
	Organization          string     `json:"organization"`
	Description           string     `json:"description"`
	Role                  string     `json:"role"`
	CompanyCode           string     `json:"company_code"`
	CommittedMinor        int64      `json:"committed_minor"`
	DeployedMinor         int64      `json:"deployed_minor"`
	DeployedPercent       int        `json:"deployed_percent"`
	PeopleReached         int64      `json:"people_reached"`
	EmployeesEnrolled     *int64     `json:"employees_enrolled"`
	WorkforceSize         int32      `json:"workforce_size"`
	EnrolledPercent       *int       `json:"enrolled_percent"`
	UpdatedAt             *time.Time `json:"updated_at"`
}

// PartnerMetric is one published, verified headline metric of an initiative.
type PartnerMetric struct {
	Label      string `json:"label"`
	Value      int64  `json:"value"`
	Unit       string `json:"unit"`
	VerifiedOn string `json:"verified_on"`
}

// PartnerInitiative is one co-initiative with its detail (P-11): every
// published metric, in display order, and its spend by category.
type PartnerInitiative struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Summary        string            `json:"summary"`
	Status         string            `json:"status"`
	Region         string            `json:"region"`
	CountyCount    int32             `json:"county_count"`
	CommittedMinor int64             `json:"committed_minor"`
	DeployedMinor  int64             `json:"deployed_minor"`
	Metrics        []PartnerMetric   `json:"metrics"`
	Spend          []PartnerSpendRow `json:"spend"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// PartnerSpendRow is one category of spend, as a total (P-12).
type PartnerSpendRow struct {
	Category    string `json:"category"`
	AmountMinor int64  `json:"amount_minor"`
}

// PartnerReportRow is one past export in the §7.3 history (P-14).
type PartnerReportRow struct {
	Downloadable          bool      `json:"downloadable"`
	PeopleReachedVerified bool      `json:"people_reached_verified"`
	ID                    string    `json:"id"`
	CommittedMinor        int64     `json:"committed_minor"`
	DeployedMinor         int64     `json:"deployed_minor"`
	PeopleReached         int64     `json:"people_reached"`
	ExportedBy            string    `json:"exported_by"`
	CreatedAt             time.Time `json:"created_at"`
}

// PartnerDashboardService is the partner portal: the session and user
// management of PartnerPortalService plus the dashboard reads (§7.3).
type PartnerDashboardService struct{ PartnerPortalService }

func partnerOverview(actor PartnerActor, t queries.PartnerOverviewTotalsRow) PartnerOverview {
	o := PartnerOverview{
		HasLogo: t.HasLogo, CommittedVerified: t.CommittedVerified, DeployedVerified: t.DeployedVerified, PeopleReachedVerified: t.PeopleReachedVerified,
		UserID:         actor.UserID.String(),
		Organization:   actor.Organization,
		Description:    t.Description,
		Role:           actor.Role,
		CompanyCode:    t.CompanyCode.String,
		CommittedMinor: t.CommittedMinor, DeployedMinor: t.DeployedMinor,
		DeployedPercent: percent(t.DeployedMinor, t.CommittedMinor),
		PeopleReached:   t.PeopleReached,
		WorkforceSize:   t.WorkforceSize,
	}
	if t.EmployeesEnrolled >= minimumGroup {
		o.EmployeesEnrolled = &t.EmployeesEnrolled
		if t.WorkforceSize > 0 {
			p := percent(t.EmployeesEnrolled, int64(t.WorkforceSize))
			o.EnrolledPercent = &p
		}
	}
	if t.UpdatedAt.Valid {
		o.UpdatedAt = &t.UpdatedAt.Time
	}
	return o
}

// percent is part as a whole percentage of whole, at most 100.
func percent(part, whole int64) int {
	if whole <= 0 || part <= 0 {
		return 0
	}
	if part >= whole {
		return 100
	}
	hi, lo := bits.Mul64(uint64(part), 100)
	value, _ := bits.Div64(hi, lo, uint64(whole))
	return int(value)
}

// Overview computes the header tiles for the calling partner.
func (s PartnerDashboardService) Overview(ctx context.Context, token string) (PartnerOverview, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) (PartnerOverview, error) {
		t, err := q.PartnerOverviewTotals(ctx, actor.PartnerID)
		if err != nil {
			return PartnerOverview{}, err
		}
		return partnerOverview(actor, t), nil
	})
}

// Initiatives lists the partner's published co-initiatives with their detail.
func (s PartnerDashboardService) Initiatives(ctx context.Context, token string) ([]PartnerInitiative, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) ([]PartnerInitiative, error) {
		rows, err := q.PartnerInitiativesList(ctx, actor.PartnerID)
		if err != nil {
			return nil, err
		}
		metrics, err := q.PartnerInitiativeMetrics(ctx, actor.PartnerID)
		if err != nil {
			return nil, err
		}
		spend, err := q.PartnerInitiativeSpend(ctx, actor.PartnerID)
		if err != nil {
			return nil, err
		}
		out := make([]PartnerInitiative, 0, len(rows))
		index := make(map[[16]byte]int, len(rows))
		for i, r := range rows {
			index[r.ID.Bytes] = i
			out = append(out, PartnerInitiative{
				ID: r.ID.String(), Title: r.Title, Summary: r.Summary, Status: r.Status, Region: r.Region,
				CountyCount: r.CountyCount, CommittedMinor: r.CommittedMinor, DeployedMinor: r.DeployedMinor,
				Metrics: []PartnerMetric{}, Spend: []PartnerSpendRow{}, UpdatedAt: r.UpdatedAt.Time,
			})
		}
		for _, m := range metrics {
			if i, ok := index[m.InitiativeID.Bytes]; ok {
				out[i].Metrics = append(out[i].Metrics, PartnerMetric{
					Label: m.Label, Value: m.Value, Unit: m.Unit, VerifiedOn: m.VerifiedOn.Time.Format(time.DateOnly),
				})
			}
		}
		for _, c := range spend {
			if i, ok := index[c.InitiativeID.Bytes]; ok {
				out[i].Spend = append(out[i].Spend, PartnerSpendRow{Category: c.Category, AmountMinor: c.AmountMinor})
			}
		}
		return out, nil
	})
}

// Spend lists spend by category across the partner's initiatives (P-12).
func (s PartnerDashboardService) Spend(ctx context.Context, token string) ([]PartnerSpendRow, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) ([]PartnerSpendRow, error) {
		rows, err := q.PartnerSpendByCategory(ctx, queries.PartnerSpendByCategoryParams{PartnerID: actor.PartnerID})
		if err != nil {
			return nil, err
		}
		out := make([]PartnerSpendRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, PartnerSpendRow{Category: r.Category, AmountMinor: r.AmountMinor})
		}
		return out, nil
	})
}

// Exports lists the partner's previous report exports, newest first (P-14).
func (s PartnerDashboardService) Exports(ctx context.Context, token string) ([]PartnerReportRow, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) ([]PartnerReportRow, error) {
		rows, err := q.PartnerReports(ctx, actor.PartnerID)
		if err != nil {
			return nil, err
		}
		out := make([]PartnerReportRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, PartnerReportRow{
				Downloadable: r.Downloadable, PeopleReachedVerified: r.PeopleReachedVerified,
				ID: r.ID.String(), CommittedMinor: r.CommittedMinor, DeployedMinor: r.DeployedMinor,
				PeopleReached: r.PeopleReached, ExportedBy: r.ExportedByName, CreatedAt: r.CreatedAt.Time,
			})
		}
		return out, nil
	})
}

const (
	partnerReportLimit   = 1000
	partnerReportBytes   = 128 << 20
	partnerReportsPerDay = 20
	// partnerReportPDFMax bounds a single rendered report PDF.
	partnerReportPDFMax = 2 << 20
	// partnerReportRetryBackoff is the base delay, jittered, before retrying a conflicting export.
	partnerReportRetryBackoff = 25 * time.Millisecond
)

// ErrPartnerReportStorage is returned when the organization's stored reports reach the count or byte limit.
var ErrPartnerReportStorage = errors.New("report storage limit reached; existing reports remain available")

// PartnerReportDailyLimit is returned when the organization has created its quota of new reports today.
// RetryAt is the next UTC midnight.
type PartnerReportDailyLimit struct{ RetryAt time.Time }

func (PartnerReportDailyLimit) Error() string {
	return "Today's new report limit has been reached. Download an existing report or try again after midnight UTC."
}

// ExportReport returns the organization's report PDF, reusing the stored one when the figures are unchanged.
// Serialization conflicts are retried a few times with a short jittered pause.
func (s PartnerDashboardService) ExportReport(ctx context.Context, token string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		pdf, err := s.exportReport(ctx, token)
		var databaseError *pgconn.PgError
		retryable := errors.As(err, &databaseError) && (databaseError.Code == "40001" ||
			(databaseError.Code == "23505" && databaseError.ConstraintName == "donor_partner_reports_snapshot_key"))
		if !retryable || attempt >= 2 || ctx.Err() != nil {
			return pdf, err
		}
		pause := partnerReportRetryBackoff * time.Duration(attempt+1)
		select {
		case <-time.After(pause/2 + rand.N(pause)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s PartnerDashboardService) exportReport(ctx context.Context, token string) ([]byte, error) {
	// Serializable retries preserve coherent figures and the per-organization quota under concurrent exports.
	return partnerAdminTransaction(ctx, s.DB, token, pgx.TxOptions{IsoLevel: pgx.Serializable},
		func(tx pgx.Tx, actor PartnerActor, now time.Time) ([]byte, error) {
			q := queries.New(tx)
			totals, err := q.PartnerOverviewTotals(ctx, actor.PartnerID)
			if err != nil {
				return nil, err
			}
			spend, err := q.PartnerSpendByCategory(
				ctx,
				queries.PartnerSpendByCategoryParams{
					PartnerID: actor.PartnerID,
					MaxRows:   pgtype.Int4{Int32: partnerreport.MaxSpendRows + 1, Valid: true},
				},
			)
			if err != nil {
				return nil, err
			}
			if len(spend) > partnerreport.MaxSpendRows {
				return nil, ErrPartnerReportTooLarge
			}
			logo, err := q.PartnerLogo(ctx, actor.PartnerID)
			if err != nil {
				return nil, err
			}
			overview := partnerOverview(actor, totals)
			normalized := overview
			normalized.UserID, normalized.Role = "", ""
			day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
			content, err := json.Marshal(struct {
				Version  int
				Day      string
				Overview PartnerOverview
				Spend    []queries.PartnerSpendByCategoryRow
				Logo     []byte
			}{partnerreport.Version, day.Format(time.DateOnly), normalized, spend, logo})
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(content)
			pdf, err := q.PartnerReportSnapshot(
				ctx,
				queries.PartnerReportSnapshotParams{PartnerID: actor.PartnerID, SnapshotDigest: digest[:]},
			)
			if err == nil {
				return pdf, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			usage, err := q.PartnerReportUsage(ctx, queries.PartnerReportUsageParams{PartnerID: actor.PartnerID, DayStart: timestamp(day)})
			if err != nil {
				return nil, err
			}
			if usage.Reports >= partnerReportLimit || usage.Bytes >= partnerReportBytes {
				return nil, ErrPartnerReportStorage
			}
			if usage.Today >= partnerReportsPerDay {
				return nil, PartnerReportDailyLimit{RetryAt: day.AddDate(0, 0, 1)}
			}
			pdf, err = renderPartnerReport(ctx, overview, spend, logo, now)
			if err != nil {
				return nil, err
			}
			if len(pdf) > partnerReportPDFMax {
				return nil, ErrPartnerReportTooLarge
			}
			if usage.Bytes+int64(len(pdf)) > partnerReportBytes {
				return nil, ErrPartnerReportStorage
			}
			snapshot, err := json.Marshal(struct {
				Overview PartnerOverview                     `json:"overview"`
				Spend    []queries.PartnerSpendByCategoryRow `json:"spend"`
			}{overview, spend})
			if err != nil {
				return nil, err
			}
			id, err := NewID()
			if err != nil {
				return nil, err
			}
			if err = q.InsertPartnerReport(ctx, queries.InsertPartnerReportParams{
				Pdf: pdf, Snapshot: snapshot, SnapshotDigest: digest[:], ID: id, PartnerID: actor.PartnerID, ExportedBy: actor.UserID,
				CommittedMinor: totals.CommittedMinor, DeployedMinor: totals.DeployedMinor, PeopleReached: totals.PeopleReached, CreatedAt: timestamp(now),
			}); err != nil {
				return nil, err
			}
			if err = recordServerEvent(ctx, q, pgtype.UUID{}, "partner_report_exported", "partner-report:"+id.String(), now); err != nil {
				return nil, err
			}
			return pdf, nil
		})
}

func (s PartnerDashboardService) ReportArtifact(ctx context.Context, token string, id pgtype.UUID) ([]byte, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) ([]byte, error) {
		return q.PartnerReportArtifact(ctx, queries.PartnerReportArtifactParams{ID: id, PartnerID: actor.PartnerID})
	})
}

var ErrPartnerReportTooLarge = partnerreport.ErrTooLarge

func renderPartnerReport(
	ctx context.Context,
	o PartnerOverview,
	spend []queries.PartnerSpendByCategoryRow,
	logo []byte,
	now time.Time,
) ([]byte, error) {
	data := partnerreport.Data{Organization: o.Organization, ExportedAt: now, UpdatedAt: o.UpdatedAt, Logo: logo}
	tile := func(value, label string) {
		data.Metrics = append(data.Metrics, partnerreport.Metric{Label: label, Value: value})
	}
	if o.CommittedVerified {
		tile(money(o.CommittedMinor), "Committed")
	}
	if o.DeployedVerified {
		tile(fmt.Sprintf("%s  (%d%% of committed)", money(o.DeployedMinor), o.DeployedPercent), "Deployed")
	}
	if o.PeopleReachedVerified {
		tile(strconv.FormatInt(o.PeopleReached, 10), "People reached (verified)")
	}
	if o.EmployeesEnrolled != nil {
		enrolled := strconv.FormatInt(*o.EmployeesEnrolled, 10)
		if o.EnrolledPercent != nil {
			enrolled += fmt.Sprintf("  (%d%% of workforce)", *o.EnrolledPercent)
		}
		tile(enrolled, "Employees enrolled")
	}
	for _, row := range spend {
		data.Spend = append(data.Spend, partnerreport.Spend{Category: row.Category, Amount: money(row.AmountMinor)})
	}
	return partnerreport.Render(ctx, data)
}
