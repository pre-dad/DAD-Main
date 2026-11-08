package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The donors-and-money queues of PRD §8: "see failed payments and accounts in
// grace" and "handle account-deletion requests". Both are read-only and paged
// by keyset, so a deep page costs the same as the first. A staff reader needs
// donor.read, which only a super admin holds, and every page is audited.

// Failed payment states. A failed renewal is in grace for the recognition
// rules' fixed seven days, after which the streak has lapsed.
const (
	FailedAll     = "all"
	FailedInGrace = "in_grace"
	FailedLapsed  = "lapsed"
)

// FailedPayment is one unresolved failed renewal. AmountMinor is null when the
// processor's failure was not recorded.
type FailedPayment struct {
	AmountMinor *int64      `json:"amount_minor"`
	Disabled    bool        `json:"disabled"`
	DonorID     pgtype.UUID `json:"donor_id"`
	Email       string      `json:"email"`
	FailedAt    time.Time   `json:"failed_at"`
	GraceEndsAt time.Time   `json:"grace_ends_at"`
	PeriodID    pgtype.UUID `json:"period_id"`
	PublicName  pgtype.Text `json:"public_name"`
	State       string      `json:"state"`
}

type FailedPaymentPage struct {
	NextCursor *string         `json:"next_cursor"`
	Payments   []FailedPayment `json:"payments"`
}

// AccountDeletion is one completed self-service erasure. The donor's personal
// data is gone by design; the id and the retained gift count remain.
type AccountDeletion struct {
	DeletedAt     time.Time   `json:"deleted_at"`
	DonorID       pgtype.UUID `json:"donor_id"`
	RetainedGifts int64       `json:"retained_gifts"`
}

type AccountDeletionPage struct {
	Deletions  []AccountDeletion `json:"deletions"`
	NextCursor *string           `json:"next_cursor"`
}

// cursor is the keyset position: the last row's time and id. It is opaque to
// callers and carries no data a staff reader could not already see.
type cursor struct {
	At  time.Time   `json:"t,omitzero"`
	Key string      `json:"k,omitempty"`
	ID  pgtype.UUID `json:"i,omitzero"`
}

func encodeCursor(at time.Time, id pgtype.UUID) *string {
	raw, err := json.Marshal(cursor{At: at.UTC(), ID: id})
	if err != nil {
		return nil
	}
	out := base64.RawURLEncoding.EncodeToString(raw)
	return &out
}

// decodeCursor reads a cursor; the empty string is the first page.
func decodeCursor(value string) (cursor, bool, error) {
	if value == "" {
		return cursor{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(value) > 200 {
		return cursor{}, false, ErrStaffInput
	}
	var c cursor
	if err = json.Unmarshal(raw, &c); err != nil || c.At.IsZero() || !c.ID.Valid {
		return cursor{}, false, ErrStaffInput
	}
	return c, true, nil
}

// keyCursor encodes a position in a list ordered by one unique text key.
func keyCursor(key string) *string {
	raw, err := json.Marshal(cursor{Key: key})
	if err != nil {
		return nil
	}
	out := base64.RawURLEncoding.EncodeToString(raw)
	return &out
}

// decodeKeyCursor reads a text-key cursor; the empty string is the first page.
func decodeKeyCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(value) > 600 {
		return "", ErrStaffInput
	}
	var c cursor
	if err = json.Unmarshal(raw, &c); err != nil || c.Key == "" || !c.At.IsZero() || c.ID.Valid {
		return "", ErrStaffInput
	}
	return c.Key, nil
}

var zeroUUID = pgtype.UUID{Valid: true}

// keysetBounds turns a cursor into non-null query bounds. The first page is
// bounded by -infinity (oldest first) or infinity (newest first), so the plan
// is the same for every page and always keeps its index range.
func keysetBounds(value string, newestFirst bool) (pgtype.Timestamptz, pgtype.UUID, error) {
	position, paged, err := decodeCursor(value)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	if paged {
		return timestamp(position.At), position.ID, nil
	}
	infinity := pgtype.Infinity
	if !newestFirst {
		infinity = pgtype.NegativeInfinity
	}
	return pgtype.Timestamptz{InfinityModifier: infinity, Valid: true}, zeroUUID, nil
}

// keysetPage trims a query's look-ahead row and returns the cursor of the last
// row kept when another page follows.
func keysetPage[T any](rows []T, cursorOf func(T) *string) ([]T, *string) {
	if len(rows) <= pageSize {
		return rows, nil
	}
	rows = rows[:pageSize]
	return rows, cursorOf(rows[len(rows)-1])
}

// FailedPayments lists unresolved failed renewals, newest first. state filters
// to accounts still in grace or already lapsed.
func (s *StaffService) FailedPayments(ctx context.Context, token, state, after string) (FailedPaymentPage, error) {
	if state == "" {
		state = FailedAll
	}
	if state != FailedAll && state != FailedInGrace && state != FailedLapsed {
		return FailedPaymentPage{}, ErrStaffInput
	}
	position, paged, err := decodeCursor(after)
	if err != nil {
		return FailedPaymentPage{}, err
	}
	return staffRead(ctx, s, token, "donor.read", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (FailedPaymentPage, error) {
		q := queries.New(tx)
		params := queries.StaffFailedPaymentsParams{State: state, AsOf: timestamp(now)}
		if paged {
			params.CursorDue, params.CursorID = timestamp(position.At), position.ID
		}
		rows, err := q.StaffFailedPayments(ctx, params)
		if err != nil {
			return FailedPaymentPage{}, err
		}
		out := FailedPaymentPage{Payments: make([]FailedPayment, 0, min(len(rows), pageSize))}
		if len(rows) > pageSize {
			rows = rows[:pageSize]
			last := rows[len(rows)-1]
			out.NextCursor = encodeCursor(last.DueAt.Time, last.PeriodID)
		}
		for _, row := range rows {
			item := FailedPayment{
				DonorID:     row.DonorID,
				PeriodID:    row.PeriodID,
				Email:       row.Email,
				PublicName:  row.PublicName,
				Disabled:    row.Disabled,
				FailedAt:    row.DueAt.Time,
				GraceEndsAt: row.GraceEndsAt.Time,
				State:       FailedLapsed,
			}
			if row.GraceEndsAt.Time.After(now) {
				item.State = FailedInGrace
			}
			if row.AmountMinor > 0 {
				item.AmountMinor = &row.AmountMinor
			}
			out.Payments = append(out.Payments, item)
		}
		details := map[string]any{"state": state, "paged": paged, "result_count": len(out.Payments)}
		if err = auditStaff(ctx, q, actor.ID, "payments.failed_list", "donors", details, now); err != nil {
			return FailedPaymentPage{}, err
		}
		return out, nil
	})
}

// AccountDeletions lists completed self-service erasures, newest first.
func (s *StaffService) AccountDeletions(ctx context.Context, token, after string) (AccountDeletionPage, error) {
	position, paged, err := decodeCursor(after)
	if err != nil {
		return AccountDeletionPage{}, err
	}
	return staffRead(ctx, s, token, "donor.read", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (AccountDeletionPage, error) {
		q := queries.New(tx)
		var params queries.StaffAccountDeletionsParams
		if paged {
			params.CursorAt, params.CursorID = timestamp(position.At), position.ID
		}
		rows, err := q.StaffAccountDeletions(ctx, params)
		if err != nil {
			return AccountDeletionPage{}, err
		}
		out := AccountDeletionPage{Deletions: make([]AccountDeletion, 0, min(len(rows), pageSize))}
		if len(rows) > pageSize {
			rows = rows[:pageSize]
			last := rows[len(rows)-1]
			out.NextCursor = encodeCursor(last.DeletedAt.Time, last.DonorID)
		}
		for _, row := range rows {
			out.Deletions = append(
				out.Deletions,
				AccountDeletion{DonorID: row.DonorID, DeletedAt: row.DeletedAt.Time, RetainedGifts: row.RetainedGifts},
			)
		}
		if err = auditStaff(ctx, q, actor.ID, "deletions.list", "donors", map[string]any{"paged": paged, "result_count": len(out.Deletions)}, now); err != nil {
			return AccountDeletionPage{}, err
		}
		return out, nil
	})
}
