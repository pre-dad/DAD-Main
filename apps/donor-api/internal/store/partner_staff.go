package store

import (
	"context"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// StaffPartner is one approved partner org in the §8 Partnerships list.
type StaffPartner struct {
	ID              string             `json:"id"`
	Organization    string             `json:"organization"`
	Type            string             `json:"type"`
	CompanyCode     string             `json:"company_code"`
	WorkforceSize   int32              `json:"workforce_size"`
	MatchRateBP     int32              `json:"match_rate_bp"`
	MatchCapMinor   int64              `json:"match_cap_minor"`
	ApprovedAt      pgtype.Timestamptz `json:"approved_at"`
	UserCount       int64              `json:"user_count"`
	InitiativeCount int64              `json:"initiative_count"`
}

type StaffPartnerPage struct {
	Partners   []StaffPartner `json:"partners"`
	NextOffset *int64         `json:"next_offset"`
}

type StaffPartnerInitiative struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	PublishState   string `json:"publish_state"`
	CommittedMinor int64  `json:"committed_minor"`
}

type StaffPartnerUser struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Accepted bool   `json:"accepted"`
}

// StaffPartnerDetail is the §8 partner detail screen: links, match and users.
type StaffPartnerDetail struct {
	StaffPartner
	Website     string                   `json:"website"`
	HasLogo     bool                     `json:"has_logo"`
	Initiatives []StaffPartnerInitiative `json:"initiatives"`
	Users       []StaffPartnerUser       `json:"users"`
}

func (s PartnerApplicationService) Partners(ctx context.Context, token string, offset int64) (StaffPartnerPage, error) {
	if !pageOffsetValid(offset) {
		return StaffPartnerPage{}, ErrStaffInput
	}
	return staffRead(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (StaffPartnerPage, error) {
			rows, err := queries.New(tx).StaffPartnersPage(ctx, offset)
			if err != nil {
				return StaffPartnerPage{}, err
			}
			rows, next := page(rows, offset)
			out := StaffPartnerPage{Partners: make([]StaffPartner, 0, len(rows)), NextOffset: next}
			for _, r := range rows {
				out.Partners = append(out.Partners, StaffPartner{
					ID: r.ID.String(), Organization: r.Organization, Type: r.PartnerType, CompanyCode: r.CompanyCode.String,
					WorkforceSize: r.WorkforceSize, MatchRateBP: r.MatchRateBp, MatchCapMinor: r.MatchCapMinor,
					ApprovedAt: r.ApprovedAt, UserCount: r.UserCount, InitiativeCount: r.InitiativeCount,
				})
			}
			return out, nil
		})
}

func (s PartnerApplicationService) Partner(ctx context.Context, token string, partnerID pgtype.UUID) (StaffPartnerDetail, error) {
	return staffRead(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) (StaffPartnerDetail, error) {
			q := queries.New(tx)
			org, err := q.PartnerOrgByID(ctx, partnerID)
			if err != nil {
				return StaffPartnerDetail{}, err
			}
			if !org.ApprovedAt.Valid {
				return StaffPartnerDetail{}, pgx.ErrNoRows
			}
			initiatives, err := q.StaffPartnerInitiatives(ctx, partnerID)
			if err != nil {
				return StaffPartnerDetail{}, err
			}
			users, err := q.PartnerUsersForPartner(ctx, partnerID)
			if err != nil {
				return StaffPartnerDetail{}, err
			}
			out := StaffPartnerDetail{
				StaffPartner: StaffPartner{
					ID: org.ID.String(), Organization: org.Organization, Type: org.PartnerType, CompanyCode: org.CompanyCode.String,
					WorkforceSize: org.WorkforceSize, MatchRateBP: org.MatchRateBp, MatchCapMinor: org.MatchCapMinor,
					ApprovedAt: org.ApprovedAt, UserCount: int64(len(users)), InitiativeCount: int64(len(initiatives)),
				},
				Website:     org.Website,
				HasLogo:     org.HasLogo,
				Initiatives: make([]StaffPartnerInitiative, 0, len(initiatives)),
				Users:       make([]StaffPartnerUser, 0, len(users)),
			}
			for _, i := range initiatives {
				out.Initiatives = append(out.Initiatives, StaffPartnerInitiative{
					ID: i.ID.String(), Title: i.Title, Status: i.Status, PublishState: i.PublishState, CommittedMinor: i.CommittedMinor,
				})
			}
			for _, u := range users {
				out.Users = append(out.Users, StaffPartnerUser{
					ID: u.ID.String(), Email: u.Email, Name: u.Name, Role: u.Role, Accepted: u.AcceptedAt.Valid,
				})
			}
			return out, nil
		})
}

// InviteUser adds a user to a partner org on its behalf (§8 "manage partner
// users"), for example when the org's only admin has left. Audited.
func (s PartnerApplicationService) InviteUser(ctx context.Context, token string, partnerID pgtype.UUID, email, name, role string) error {
	email, name, err := validPartnerUser(email, name, role)
	if err != nil {
		return err
	}
	_, err = staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if _, err := q.LockPartnerOrg(ctx, partnerID); err != nil {
				return struct{}{}, err
			}
			org, err := q.PartnerOrgByID(ctx, partnerID)
			if err != nil {
				return struct{}{}, err
			}
			if !org.ApprovedAt.Valid {
				return struct{}{}, pgx.ErrNoRows
			}
			if err = reserveEmailIdentity(ctx, q, email, identityPartner, partnerID); err != nil {
				return struct{}{}, err
			}
			id, err := NewID()
			if err != nil {
				return struct{}{}, err
			}
			user, err := q.InvitePartnerUser(ctx, queries.InvitePartnerUserParams{
				ID: id, PartnerID: partnerID, Email: email, Name: name, Role: role, CreatedAt: timestamp(now),
			})
			if err != nil {
				return struct{}{}, err
			}
			if err = queuePartnerLogin(ctx, q, user, partnerLinkInvited, now); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "partner.user_invited", partnerID.String(),
				map[string]string{"user_id": user.String(), "role": role}, now)
		})
	return err
}

// RemoveUser disables a partner user and ends their sessions. Audited.
func (s PartnerApplicationService) RemoveUser(ctx context.Context, token string, partnerID, userID pgtype.UUID) error {
	_, err := staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			if _, err := q.LockPartnerOrg(ctx, partnerID); err != nil {
				return struct{}{}, err
			}
			removed, err := q.DisablePartnerUser(ctx, queries.DisablePartnerUserParams{PartnerID: partnerID, ID: userID})
			if err != nil {
				return struct{}{}, err
			}
			if removed == 0 {
				return struct{}{}, pgx.ErrNoRows
			}
			if err = q.RevokePartnerUserSessions(ctx, queries.RevokePartnerUserSessionsParams{
				PartnerUserID: userID, RevokedAt: timestamp(now),
			}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, "partner.user_removed", partnerID.String(),
				map[string]string{"user_id": userID.String()}, now)
		})
	return err
}
