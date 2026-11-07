package store

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// partnerPermission gates every staff partner action (§8: Partnerships
// operators and super admins).
const partnerPermission = "partner.manage"

// ErrPartnerApplication is a validation failure on a partner application.
var ErrPartnerApplication = errors.New("invalid partner application")

// ErrPartnerDecision is returned when a decision targets an application that is
// not pending (already approved/rejected, or unknown).
var ErrPartnerDecision = errors.New("application is not pending")

// PartnerApplication is a P-1 application in the P-2 approval machine.
type PartnerApplication struct {
	ID              string  `json:"id"`
	Organization    string  `json:"organization"`
	Website         string  `json:"website"`
	Type            string  `json:"type"`
	ContactName     string  `json:"contact_name"`
	Title           string  `json:"title"`
	WorkEmail       string  `json:"work_email"`
	Phone           string  `json:"phone"`
	WorkforceSize   int32   `json:"workforce_size"`
	Status          string  `json:"status"`
	DecisionReason  string  `json:"decision_reason"`
	ApprovedPartner *string `json:"approved_partner_id"`
}

// PartnerApplicationInput is a P-1 submission.
type PartnerApplicationInput struct {
	Organization  string `json:"organization"`
	Website       string `json:"website"`
	Type          string `json:"type"`
	ContactName   string `json:"contact_name"`
	Title         string `json:"title"`
	WorkEmail     string `json:"work_email"`
	Phone         string `json:"phone"`
	WorkforceSize int32  `json:"workforce_size"`
}

// PartnerApplicationService is the apply + approve/reject surface (PRD §7.1,
// §8 Partnerships). Applying is public; deciding needs partner.manage.
type PartnerApplicationService struct {
	DB    *pgxpool.Pool
	Staff *StaffService
}

// validate checks a P-1 application, returning ErrPartnerApplication on any
// problem and the normalized params.
func (in PartnerApplicationInput) validate(id pgtype.UUID, now time.Time) (queries.InsertPartnerApplicationParams, error) {
	var p queries.InsertPartnerApplicationParams
	if !ValidRequestID(id) {
		return p, ErrPartnerApplication
	}
	in.Organization = identity.TrimSpace(in.Organization)
	in.ContactName = identity.TrimSpace(in.ContactName)
	in.Type = identity.TrimSpace(in.Type)
	in.Title = identity.TrimSpace(in.Title)
	in.Phone = identity.TrimSpace(in.Phone)
	in.Website = identity.TrimSpace(in.Website)
	for _, f := range []struct {
		value   string
		maximum int
		require bool
	}{
		{in.Organization, 200, true}, {in.ContactName, 100, true},
		{in.Type, 80, false}, {in.Title, 100, false}, {in.Phone, 40, false},
	} {
		if (f.require && f.value == "") || utf8.RuneCountInString(f.value) > f.maximum || !plainText(f.value, false) {
			return p, ErrPartnerApplication
		}
	}
	email, err := identity.NormalizeEmail(in.WorkEmail)
	if err != nil {
		return p, ErrPartnerApplication
	}
	if in.Website != "" {
		u, err := url.Parse(in.Website)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || utf8.RuneCountInString(in.Website) > 500 {
			return p, ErrPartnerApplication
		}
	}
	if in.WorkforceSize < 0 {
		return p, ErrPartnerApplication
	}
	return queries.InsertPartnerApplicationParams{
		ID: id, Organization: in.Organization, Website: in.Website, Type: in.Type,
		ContactName: in.ContactName, Title: in.Title, WorkEmail: email, Phone: in.Phone,
		WorkforceSize: in.WorkforceSize, CreatedAt: timestamp(now),
	}, nil
}

// Apply records a new PENDING partner application (P-1/P-2). id makes a retry
// idempotent, even one that races the original; a second PENDING application
// for the same work email is refused by the partial unique index (surfaced as
// ErrPartnerApplication).
func (s PartnerApplicationService) Apply(ctx context.Context, id pgtype.UUID, in PartnerApplicationInput) error {
	params, err := in.validate(id, time.Now().UTC())
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		return applyPartner(ctx, queries.New(tx), params)
	})
}

func applyPartner(ctx context.Context, q *queries.Queries, params queries.InsertPartnerApplicationParams) error {
	inserted, err := q.InsertPartnerApplication(ctx, params)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return ErrPartnerApplication
	}
	if err != nil || inserted == 1 {
		return err
	}
	existing, err := q.PartnerApplicationByID(ctx, params.ID)
	if err != nil {
		return err
	}
	if existing.Organization == params.Organization && existing.Website == params.Website && existing.Type == params.Type &&
		existing.ContactName == params.ContactName && existing.Title == params.Title && existing.WorkEmail == params.WorkEmail &&
		existing.Phone == params.Phone && existing.WorkforceSize == params.WorkforceSize {
		return nil
	}
	return ErrPartnerApplication
}

var applicationReceiptToken = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

const applicationReceiptLifetime = 30 * 24 * time.Hour

func (s PartnerApplicationService) ApplyWithReceipt(ctx context.Context, id pgtype.UUID, in PartnerApplicationInput, receipt string) error {
	if !applicationReceiptToken.MatchString(receipt) {
		return ErrPartnerApplication
	}
	now := time.Now().UTC()
	params, err := in.validate(id, now)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		if err := applyPartner(ctx, q, params); err != nil {
			return err
		}
		n, err := q.AttachApplicationReceipt(ctx, queries.AttachApplicationReceiptParams{
			ID: id, ReceiptHash: text(HashToken(receipt)), ReceiptExpiresAt: timestamp(now.Add(applicationReceiptLifetime)),
		})
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
			return ErrPartnerApplication
		}
		if err == nil && n != 1 {
			err = ErrPartnerApplication
		}
		return err
	})
}

type ApplicationStatus struct {
	Organization string `json:"organization"`
	Status       string `json:"status"`
}

func (s PartnerApplicationService) ApplicationStatus(ctx context.Context, receipt string) (ApplicationStatus, error) {
	if !applicationReceiptToken.MatchString(receipt) {
		return ApplicationStatus{}, ErrPartnerDenied
	}
	row, err := queries.New(s.DB).ApplicationStatusByReceipt(ctx, text(HashToken(receipt)))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplicationStatus{}, ErrPartnerDenied
	}
	return ApplicationStatus{Organization: row.Organization, Status: row.Status}, err
}

// Approve approves a pending application (§8), promotes its organization into
// the partner-org anchor initiatives reference with its company code (EM-1),
// and makes the signer the first partner ADMIN, emailing them a sign-in link
// (P-3). Returns the new partner-org id. Audited.
func (s PartnerApplicationService) Approve(ctx context.Context, token string, appID pgtype.UUID) (pgtype.UUID, error) {
	return staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (pgtype.UUID, error) {
			q := queries.New(tx)
			app, err := q.PartnerApplicationByID(ctx, appID)
			if errors.Is(err, pgx.ErrNoRows) {
				return pgtype.UUID{}, ErrPartnerDecision
			}
			if err != nil {
				return pgtype.UUID{}, err
			}
			if app.Status != "PENDING" {
				return pgtype.UUID{}, ErrPartnerDecision
			}
			partnerID, err := NewID()
			if err != nil {
				return pgtype.UUID{}, err
			}
			code, err := newCompanyCode(app.Organization)
			if err != nil {
				return pgtype.UUID{}, err
			}
			if err = q.PromotePartnerOrg(ctx, queries.PromotePartnerOrgParams{
				ID: partnerID, Organization: app.Organization, Name: app.ContactName,
				Email: app.WorkEmail, Website: app.Website, ApprovedAt: timestamp(now),
				PartnerType: app.Type, WorkforceSize: app.WorkforceSize, CompanyCode: text(code),
			}); err != nil {
				return pgtype.UUID{}, err
			}
			if err = q.DecidePartnerApplication(ctx, queries.DecidePartnerApplicationParams{
				ID: appID, Status: "APPROVED", DecidedBy: actor.ID, DecidedAt: timestamp(now),
				ApprovedPartnerID: partnerID,
			}); err != nil {
				return pgtype.UUID{}, err
			}
			if err = reserveEmailIdentity(ctx, q, app.WorkEmail, identityPartner, partnerID); err != nil {
				return pgtype.UUID{}, err
			}
			userID, err := NewID()
			if err != nil {
				return pgtype.UUID{}, err
			}
			admin, err := q.InvitePartnerUser(ctx, queries.InvitePartnerUserParams{
				ID: userID, PartnerID: partnerID, Email: app.WorkEmail, Name: app.ContactName,
				Role: "ADMIN", CreatedAt: timestamp(now),
			})
			if err != nil {
				return pgtype.UUID{}, err
			}
			if err = queuePartnerLogin(ctx, q, admin, partnerLinkApproved, now); err != nil {
				return pgtype.UUID{}, err
			}
			if err := recordServerEvent(ctx, q, pgtype.UUID{}, "partner_approved", "partner-approved:"+partnerID.String(), now); err != nil {
				return pgtype.UUID{}, err
			}
			return partnerID, auditStaff(ctx, q, actor.ID, "partner.approved", appID.String(),
				map[string]string{"partner_id": partnerID.String(), "organization": app.Organization}, now)
		})
}

// companyCodeAlphabet leaves out characters easily misread when a code is
// typed from a poster or a slide: 0/O, 1/I/L.
const companyCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// newCompanyCode returns an employer's company code (EM-1): up to six letters
// and digits of the organization name, so employees recognise it, then six
// random characters, which keep codes unguessable and collision-free in
// practice.
func newCompanyCode(organization string) (string, error) {
	var prefix strings.Builder
	for _, r := range strings.ToUpper(organization) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			prefix.WriteRune(r)
			if prefix.Len() == 6 {
				break
			}
		}
	}
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	for i, b := range random {
		random[i] = companyCodeAlphabet[int(b)%len(companyCodeAlphabet)]
	}
	if prefix.Len() == 0 {
		return string(random), nil
	}
	return prefix.String() + "-" + string(random), nil
}

// Reject rejects a pending application with a reason (§8). Audited.
func (s PartnerApplicationService) Reject(ctx context.Context, token string, appID pgtype.UUID, reason string) error {
	if utf8.RuneCountInString(reason) > 500 || !plainText(reason, true) {
		return ErrPartnerApplication
	}
	_, err := staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			app, err := q.PartnerApplicationByID(ctx, appID)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, ErrPartnerDecision
			}
			if err != nil {
				return struct{}{}, err
			}
			if app.Status != "PENDING" {
				return struct{}{}, ErrPartnerDecision
			}
			if err = q.DecidePartnerApplication(ctx, queries.DecidePartnerApplicationParams{
				ID: appID, Status: "REJECTED", DecidedBy: actor.ID, DecidedAt: timestamp(now),
				DecisionReason: reason,
			}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "partner.rejected", appID.String(), nil, now)
		})
	return err
}

// LinkInitiative links an initiative to an approved partner org (§8/P-3). Audited.
func (s PartnerApplicationService) LinkInitiative(
	ctx context.Context,
	token string,
	partnerID, initiativeID pgtype.UUID,
	commitment ...int64,
) error {
	var amount pgtype.Int8
	if len(commitment) > 1 || (len(commitment) == 1 && commitment[0] < 0) {
		return ErrStaffInput
	}
	if len(commitment) == 1 {
		amount = pgtype.Int8{Int64: commitment[0], Valid: true}
	}
	_, err := staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			org, err := q.PartnerOrgByID(ctx, partnerID)
			if err != nil {
				return struct{}{}, err
			}
			if !org.ApprovedAt.Valid {
				return struct{}{}, pgx.ErrNoRows
			}
			linked, err := q.LinkInitiativeToPartner(ctx, queries.LinkInitiativeToPartnerParams{
				InitiativeID: initiativeID, PartnerID: partnerID, CommittedMinor: amount, UpdatedAt: timestamp(now),
			})
			if err != nil {
				return struct{}{}, err
			}
			if linked == 0 {
				return struct{}{}, pgx.ErrNoRows
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "partner.initiative_linked", partnerID.String(),
				map[string]string{"initiative_id": initiativeID.String()}, now)
		})
	return err
}

// SetMatch sets a partner's workforce size, match rate (basis points) and cap
// (§8 Partnerships). Audited.
func (s PartnerApplicationService) SetMatch(
	ctx context.Context,
	token string,
	partnerID pgtype.UUID,
	workforce, rateBP int32,
	capMinor int64,
) error {
	if workforce < 0 || rateBP < 0 || capMinor < 0 {
		return ErrPartnerApplication
	}
	_, err := staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			updated, err := q.SetPartnerMatch(ctx, queries.SetPartnerMatchParams{
				ID: partnerID, WorkforceSize: workforce, MatchRateBp: rateBP, MatchCapMinor: capMinor,
			})
			if err != nil {
				return struct{}{}, err
			}
			if updated == 0 {
				return struct{}{}, pgx.ErrNoRows
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "partner.match_set", partnerID.String(), nil, now)
		})
	return err
}

// PendingApplications returns the §8 approval queue (one page past 50).
func (s PartnerApplicationService) PendingApplications(ctx context.Context, token string, offset int64) ([]PartnerApplication, error) {
	return staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) ([]PartnerApplication, error) {
			rows, err := queries.New(tx).PendingPartnerApplications(ctx, offset)
			if err != nil {
				return nil, err
			}
			out := make([]PartnerApplication, 0, len(rows))
			for _, r := range rows {
				out = append(out, PartnerApplication{
					ID: r.ID.String(), Organization: r.Organization, Website: r.Website, Type: r.Type,
					ContactName: r.ContactName, Title: r.Title, WorkEmail: r.WorkEmail, Phone: r.Phone,
					WorkforceSize: r.WorkforceSize, Status: r.Status, DecisionReason: r.DecisionReason,
				})
			}
			return out, nil
		})
}
