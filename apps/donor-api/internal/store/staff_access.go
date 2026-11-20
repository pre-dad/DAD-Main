package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const staffInviteLifetime = 24 * time.Hour

func createStaffInvite(ctx context.Context, q *queries.Queries, id pgtype.UUID, now time.Time) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	inviteID, err := NewID()
	if err != nil {
		return "", err
	}
	err = q.InsertStaffInvite(ctx, queries.InsertStaffInviteParams{
		ID:        inviteID,
		StaffID:   id,
		TokenHash: HashToken(token),
		ExpiresAt: timestamp(now.Add(staffInviteLifetime)),
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func inviteStaff(ctx context.Context, q *queries.Queries, actor pgtype.UUID, email, name, role string, now time.Time) (string, error) {
	canonical, err := identity.NormalizeEmail(email)
	if err != nil || !plainText(name, false) || identity.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 100 ||
		len(staffPermissions(role)) == 0 {
		return "", ErrStaffInput
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	if err = reserveEmailIdentity(ctx, q, canonical, identityStaff, pgtype.UUID{}); err != nil {
		return "", err
	}
	if _, err = q.InsertStaff(ctx, queries.InsertStaffParams{ID: id, Email: canonical, Name: identity.TrimSpace(name), Role: role}); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
			return "", ErrStaffExists
		}
		return "", err
	}
	token, err := createStaffInvite(ctx, q, id, now)
	if err != nil {
		return "", err
	}
	if err = auditStaff(ctx, q, actor, "staff.invited", id.String(), map[string]string{"role": role}, now); err != nil {
		return "", err
	}
	return token, nil
}

// Bootstrap invites the first administrator. It refuses once any staff
// account exists, so it cannot be used to add access later.
func (s *StaffService) Bootstrap(ctx context.Context, email, name string) (string, error) {
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (string, error) {
		if _, err := tx.Exec(ctx, "LOCK TABLE donor_staff IN EXCLUSIVE MODE"); err != nil {
			return "", err
		}
		q := queries.New(tx)
		rows, err := q.LockStaffAccounts(ctx)
		if err != nil {
			return "", err
		}
		if len(rows) > 0 {
			return "", ErrStaffConflict
		}
		return inviteStaff(ctx, q, pgtype.UUID{}, email, name, superAdminRole, time.Now().UTC())
	})
}

func (s *StaffService) Invite(ctx context.Context, token, email, name, role string) (string, error) {
	return staffWrite(ctx, s, token, "staff.manage", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (string, error) {
		return inviteStaff(ctx, queries.New(tx), actor.ID, email, name, role, now)
	})
}

func (s *StaffService) Accounts(ctx context.Context, token string) ([]StaffView, error) {
	return staffRead(ctx, s, token, "staff.manage", func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) ([]StaffView, error) {
		rows, err := queries.New(tx).LockStaffAccounts(ctx)
		if err != nil {
			return nil, err
		}
		slices.SortFunc(rows, func(a, b queries.LockStaffAccountsRow) int {
			return cmp.Compare(a.DonorStaff.Email, b.DonorStaff.Email)
		})
		result := make([]StaffView, 0, len(rows))
		for _, row := range rows {
			result = append(result, staffView(row.DonorStaff, row.Active))
		}
		return result, nil
	})
}

// AccessChange reports the effect of a staff access action. Token is the
// one-use recovery invitation and is never serialized.
type AccessChange struct {
	Disabled bool       `json:"disabled,omitempty"`
	Staff    *StaffView `json:"staff,omitempty"`
	Token    string     `json:"-"`
}

// ChangeAccess disables another account, changes its role, or replaces its
// passkeys through a recovery invitation, revoking its sessions. It locks
// every account so at least one other active administrator always remains.
func (s *StaffService) ChangeAccess(ctx context.Context, token string, targetID pgtype.UUID, action, role string) (AccessChange, error) {
	return staffRead(ctx, s, token, "staff.manage", func(tx pgx.Tx, actor queries.DonorStaff, _ time.Time) (AccessChange, error) {
		q := queries.New(tx)
		rows, err := q.LockStaffAccounts(ctx)
		if err != nil {
			return AccessChange{}, err
		}
		now := time.Now().UTC()
		if _, err = requireStaff(ctx, q, token, "staff.manage", now); err != nil {
			return AccessChange{}, err
		}
		if targetID == actor.ID {
			return AccessChange{}, ErrStaffConflict
		}
		var target queries.LockStaffAccountsRow
		otherAdmins := 0
		for _, row := range rows {
			if row.DonorStaff.ID == targetID {
				target = row
			} else if row.DonorStaff.Role == superAdminRole && !row.DonorStaff.Disabled && row.Active {
				otherAdmins++
			}
		}
		if !target.DonorStaff.ID.Valid {
			return AccessChange{}, pgx.ErrNoRows
		}
		if otherAdmins == 0 {
			return AccessChange{}, ErrStaffConflict
		}
		return changeAccess(ctx, q, actor.ID, target, action, role, now)
	})
}

// Recover is the break-glass path for when no administrator can sign in: it
// replaces an enabled administrator's passkeys through a recovery invitation.
// Like Bootstrap it needs the operator's database access rather than a staff
// session, so its audit record has no actor.
func (s *StaffService) Recover(ctx context.Context, email string) (string, error) {
	canonical, err := identity.NormalizeEmail(email)
	if err != nil {
		return "", ErrStaffInput
	}
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (string, error) {
		q := queries.New(tx)
		rows, err := q.LockStaffAccounts(ctx)
		if err != nil {
			return "", err
		}
		var target queries.LockStaffAccountsRow
		for _, row := range rows {
			if row.DonorStaff.Email == canonical && row.DonorStaff.Role == superAdminRole && !row.DonorStaff.Disabled {
				target = row
			}
		}
		if !target.DonorStaff.ID.Valid {
			return "", pgx.ErrNoRows
		}
		change, err := changeAccess(ctx, q, pgtype.UUID{}, target, "recovery", "", time.Now().UTC())
		return change.Token, err
	})
}

func changeAccess(
	ctx context.Context,
	q *queries.Queries,
	actor pgtype.UUID,
	row queries.LockStaffAccountsRow,
	action, role string,
	now time.Time,
) (AccessChange, error) {
	var result AccessChange
	var details any
	var event string
	target := row.DonorStaff
	switch action {
	case "disable":
		target.Disabled = true
		event = "staff.disabled"
		result.Disabled = true
	case "role":
		if len(staffPermissions(role)) == 0 {
			return result, ErrStaffInput
		}
		if target.Role == role {
			view := staffView(target, row.Active)
			return AccessChange{Staff: &view}, nil
		}
		details = map[string]string{"previous": target.Role, "role": role}
		target.Role = role
		event = "staff.role_changed"
		view := staffView(target, row.Active)
		result.Staff = &view
	case "recovery":
		details = map[string]bool{"previously_disabled": target.Disabled, "previously_active": row.Active}
		target.Disabled = false
		if _, err := q.DeleteStaffPasskeys(ctx, target.ID); err != nil {
			return result, err
		}
		if err := q.InvalidateStaffInvites(ctx, queries.InvalidateStaffInvitesParams{StaffID: target.ID, ConsumedAt: timestamp(now)}); err != nil {
			return result, err
		}
		invitation, err := createStaffInvite(ctx, q, target.ID, now)
		if err != nil {
			return result, err
		}
		result.Token = invitation
		event = "staff.recovery_invited"
	default:
		return result, ErrStaffInput
	}
	if err := q.SetStaffAccess(ctx, queries.SetStaffAccessParams{ID: target.ID, Disabled: target.Disabled, Role: target.Role}); err != nil {
		return result, err
	}
	if err := q.RevokeStaffSessions(ctx, queries.RevokeStaffSessionsParams{StaffID: target.ID, RevokedAt: timestamp(now)}); err != nil {
		return result, err
	}
	return result, auditStaff(ctx, q, actor, event, target.ID.String(), details, now)
}
