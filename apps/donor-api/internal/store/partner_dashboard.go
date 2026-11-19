package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/go-pdf/fpdf"
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
		rows, err := q.PartnerSpendByCategory(ctx, actor.PartnerID)
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
)

var ErrPartnerReportStorage = errors.New("report storage limit reached; existing reports remain available")

type PartnerReportDailyLimit struct{ RetryAt time.Time }

func (PartnerReportDailyLimit) Error() string {
	return "Today's new report limit has been reached. Download an existing report or try again after midnight UTC."
}

func (s PartnerDashboardService) ExportReport(ctx context.Context, token string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		pdf, err := s.exportReport(ctx, token)
		var databaseError *pgconn.PgError
		retryable := errors.As(err, &databaseError) && (databaseError.Code == "40001" ||
			(databaseError.Code == "23505" && databaseError.ConstraintName == "donor_partner_reports_snapshot_key"))
		if !retryable || attempt >= 2 || ctx.Err() != nil {
			return pdf, err
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
			spend, err := q.PartnerSpendByCategory(ctx, actor.PartnerID)
			if err != nil {
				return nil, err
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
			}{1, day.Format(time.DateOnly), normalized, spend, logo})
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
			pdf, err = renderPartnerReport(overview, spend, logo, now)
			if err != nil {
				return nil, err
			}
			if len(pdf) > 2<<20 {
				return nil, ErrPartnerApplication
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

// renderPartnerReport builds the branded PDF: the DAD header band,
// the partner organization, the dashboard figures, spend by category, the
// export time, and the "Figures verified by DAD" statement (P-14). The core
// fonts are cp1252, so every string is translated rather than written as UTF-8.
func renderPartnerReport(o PartnerOverview, spend []queries.PartnerSpendByCategoryRow, logo []byte, now time.Time) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCreationDate(now.UTC())
	pdf.SetModificationDate(now.UTC())
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.SetMargins(18, 32, 18)
	pdf.SetAutoPageBreak(true, 30)
	pdf.SetHeaderFunc(func() {
		pdf.SetFillColor(16, 32, 31)
		pdf.Rect(0, 0, 210, 26, "F")
		pdf.SetY(8)
		pdf.SetFont("Helvetica", "B", 20)
		pdf.SetTextColor(255, 255, 255)
		pdf.CellFormat(0, 10, tr("DAD — Impact Report"), "", 1, "L", false, 0, "")
		if len(logo) > 0 {
			pdf.RegisterImageOptionsReader("partner-logo", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(logo))
			image := pdf.GetImageInfo("partner-logo")
			if image != nil {
				width, height := image.Extent()
				scale := min(30/width, 15/height)
				pdf.ImageOptions(
					"partner-logo",
					192-width*scale,
					6+(15-height*scale)/2,
					width*scale,
					height*scale,
					false,
					fpdf.ImageOptions{ImageType: "PNG"},
					0,
					"",
				)
			}
		}
		pdf.SetY(32)
	})
	pdf.SetFooterFunc(func() {
		pdf.SetY(-24)
		pdf.SetDrawColor(200, 200, 200)
		pdf.Line(18, pdf.GetY(), 192, pdf.GetY())
		pdf.Ln(3)
		pdf.SetFont("Helvetica", "I", 9)
		pdf.SetTextColor(90, 90, 90)
		pdf.MultiCell(
			0,
			5,
			"Sandbox preview. Figures verified by DAD. Only published, verified figures are shown; unverified figures are never estimated.",
			"",
			"L",
			false,
		)
		pdf.CellFormat(0, 4, fmt.Sprintf("Page %d", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()

	pdf.SetTextColor(20, 20, 20)
	pdf.Ln(10)
	pdf.SetFont("Helvetica", "B", 15)
	pdf.MultiCell(0, 8, tr(o.Organization), "", "L", false)
	pdf.SetFont("Helvetica", "", 10)
	pdf.SetTextColor(90, 90, 90)
	pdf.CellFormat(0, 6, "Exported "+now.UTC().Format("2 January 2006, 15:04 UTC"), "", 1, "L", false, 0, "")
	if o.UpdatedAt != nil {
		pdf.CellFormat(0, 6, "Figures last updated "+o.UpdatedAt.UTC().Format("2 January 2006, 15:04 UTC"), "", 1, "L", false, 0, "")
	}
	pdf.Ln(6)

	pdf.SetTextColor(20, 20, 20)
	tile := func(value, label string) {
		pdf.SetFont("Helvetica", "B", 16)
		pdf.CellFormat(0, 8, tr(value), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(90, 90, 90)
		pdf.CellFormat(0, 6, tr(label), "", 1, "L", false, 0, "")
		pdf.SetTextColor(20, 20, 20)
		pdf.Ln(3)
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

	if len(spend) > 0 {
		pdf.Ln(4)
		pdf.SetFont("Helvetica", "B", 12)
		pdf.CellFormat(0, 8, "Spend by category", "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		for _, row := range spend {
			height := 6 * float64(len(pdf.SplitLines([]byte(tr(row.Category)), 120)))
			if pdf.GetY()+height > 267 {
				pdf.AddPage()
				pdf.SetFont("Helvetica", "", 10)
				pdf.SetTextColor(20, 20, 20)
			}
			y := pdf.GetY()
			pdf.MultiCell(120, 6, tr(row.Category), "", "L", false)
			pdf.SetXY(138, y)
			pdf.CellFormat(54, height, money(row.AmountMinor), "", 0, "R", false, 0, "")
			pdf.SetXY(18, y+height)
		}
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
