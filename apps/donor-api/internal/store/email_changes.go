package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrEmailChangeConflict = errors.New("this address cannot replace the account email")

// proofLifetime is how long an emailed sign-in or confirmation link works.
const proofLifetime = 15 * time.Minute

// proofMinutes is proofLifetime as it reads in an email.
var proofMinutes = fmt.Sprintf("%d minutes", int(proofLifetime/time.Minute))

type EmailChangeService struct {
	DB             *pgxpool.Pool
	EncryptionKeys credentials.Keyring
	// FoundingClaims matches the new address to the website waitlist (D-6).
	FoundingClaims FoundingClaimKey
}

func emailChangeProof(keys credentials.Keyring) (string, []byte, error) {
	proof, err := randomToken(32)
	if err != nil {
		return "", nil, err
	}
	encrypted, err := keys.Encrypt([]byte(proof))
	return HashToken(proof), encrypted, err
}

func identityEvent(ctx context.Context, q *queries.Queries, donor, change pgtype.UUID, action string) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.RecordIdentityEvent(ctx, queries.RecordIdentityEventParams{ID: id, DonorID: donor, EmailChangeID: change, Action: action})
}

// Request starts changing the signed-in donor's email to email, replacing any
// change in progress. The current address must confirm first.
func (s EmailChangeService) Request(ctx context.Context, session, email string) error {
	email, err := identity.NormalizeEmail(email)
	if err != nil {
		return err
	}
	_, err = donorWrite(ctx, s.DB, donorOwner(session), func(tx pgx.Tx, donor queries.Donor, now time.Time) (struct{}, error) {
		if donor.Email == email {
			return struct{}{}, ErrEmailChangeConflict
		}
		hash, encrypted, err := emailChangeProof(s.EncryptionKeys)
		if err != nil {
			return struct{}{}, err
		}
		id, err := NewID()
		if err != nil {
			return struct{}{}, err
		}
		q := queries.New(tx)
		if err = reserveEmailIdentity(ctx, q, email, identityDonor, pgtype.UUID{}); errors.Is(err, EmailRoleError{}) {
			// Answer like any other address, so the response does not reveal that a staff or partner holds it.
			return struct{}{}, nil
		} else if err != nil {
			return struct{}{}, err
		}
		if err = q.CancelEmailChanges(ctx, donor.ID); err != nil {
			return struct{}{}, err
		}
		if err = q.InsertEmailChange(ctx, queries.InsertEmailChangeParams{ID: id, DonorID: donor.ID, OldEmail: donor.Email, NewEmail: email, TokenHash: hash, EncryptedToken: encrypted, ExpiresAt: timestamp(now.Add(proofLifetime))}); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, identityEvent(ctx, q, donor.ID, id, "REQUESTED")
	})
	return err
}

// Confirm redeems an email-change proof and returns the stage reached. Proof
// from the current address issues the new address's confirmation ("NEW");
// proof from the new address completes the change ("COMPLETED"), revoking
// every session, sign-in link, and passkey. The donor ID never changes.
func (s EmailChangeService) Confirm(ctx context.Context, proof string) (string, error) {
	var stage string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		owner, err := q.EmailChangeOwner(ctx, HashToken(proof))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDenied
		}
		if err != nil {
			return err
		}
		donor, err := q.LockDonor(ctx, owner)
		if err != nil {
			return err
		}
		change, err := q.EmailChangeByToken(ctx, HashToken(proof))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDenied
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if donor.Disabled || donor.Email != change.OldEmail || !change.ExpiresAt.Time.After(now) {
			return ErrDenied
		}
		switch change.Stage {
		case "CURRENT":
			stage = "NEW"
			return s.advanceEmailChange(ctx, q, change, now)
		case "NEW":
			stage = "COMPLETED"
			return completeEmailChange(ctx, q, s.FoundingClaims, change, now)
		}
		return ErrDenied
	})
	if err != nil {
		return "", err
	}
	return stage, nil
}

// advanceEmailChange replaces the current address's proof with a fresh one
// for the new address.
func (s EmailChangeService) advanceEmailChange(
	ctx context.Context,
	q *queries.Queries,
	change queries.DonorEmailChange,
	now time.Time,
) error {
	hash, encrypted, err := emailChangeProof(s.EncryptionKeys)
	if err != nil {
		return err
	}
	if err = q.AdvanceEmailChange(ctx, queries.AdvanceEmailChangeParams{ID: change.ID, TokenHash: hash, EncryptedToken: encrypted, ExpiresAt: timestamp(now.Add(proofLifetime))}); err != nil {
		return err
	}
	return identityEvent(ctx, q, change.DonorID, change.ID, "CURRENT_VERIFIED")
}

// completeEmailChange moves the account to the new address, which must not
// belong to another donor: accounts are never merged.
func completeEmailChange(
	ctx context.Context,
	q *queries.Queries,
	claims FoundingClaimKey,
	change queries.DonorEmailChange,
	now time.Time,
) error {
	if err := reserveEmailIdentity(ctx, q, change.NewEmail, identityDonor, pgtype.UUID{}); errors.Is(err, EmailRoleError{}) {
		return ErrEmailChangeConflict
	} else if err != nil {
		return err
	}
	if err := q.SetDonorEmail(ctx, queries.SetDonorEmailParams{ID: change.DonorID, Email: change.NewEmail}); err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
			return ErrEmailChangeConflict
		}
		return err
	}
	if err := claims.grant(ctx, q, change.DonorID, change.NewEmail, now); err != nil {
		return err
	}
	if err := q.RevokeAllSessions(ctx, queries.RevokeAllSessionsParams{DonorID: change.DonorID, RevokedAt: timestamp(now)}); err != nil {
		return err
	}
	// The email is the account's root credential, so changing it resets every
	// other way in; passkeys also name the old address to their authenticators.
	if _, err := q.DeleteAllDonorPasskeys(ctx, change.DonorID); err != nil {
		return err
	}
	if err := q.ConsumeDonorLinks(ctx, queries.ConsumeDonorLinksParams{DonorID: change.DonorID, ConsumedAt: timestamp(now)}); err != nil {
		return err
	}
	if err := q.CompleteEmailChange(ctx, queries.CompleteEmailChangeParams{ID: change.ID, CompletedAt: timestamp(now)}); err != nil {
		return err
	}
	return identityEvent(ctx, q, change.DonorID, change.ID, "CHANGED")
}
