package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/geo"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	passwordCost = 12
	// unknownAccountHash is a valid bcrypt hash of a password no one holds, so a
	// sign-in for an unknown or ineligible account spends the same time as a
	// wrong password.
	unknownAccountHash = "$2a$12$LQv3c1yqBWVHxkd0LHAkCOYz6Ttxq7vdR5rJdJzNV6YjZ8T2eW8tK"
)

var ErrPassword = errors.New("use a password of 12 to 72 bytes")

// ErrCurrentPassword means the signed-in donor's proof for a password change was missing or wrong. It
// wraps ErrDenied so existing callers still match, while the API can tell it from a bad session.
var ErrCurrentPassword = fmt.Errorf("current password incorrect: %w", ErrDenied)

type PasswordService struct {
	DB             *pgxpool.Pool
	FoundingClaims FoundingClaimKey
}

type SignupInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Zip   string `json:"zip"`
	City  string `json:"city"`
}

// Signup sends proof to the address; it never overwrites an existing donor's profile.
func (s PasswordService) Signup(ctx context.Context, in SignupInput) error {
	email, err := identity.NormalizeEmail(in.Email)
	if err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	in.City = strings.TrimSpace(in.City)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 150 || !plainText(in.Name, false) || in.City == "" ||
		utf8.RuneCountInString(in.City) > 80 ||
		!plainText(in.City, false) {
		return ErrStaffInput
	}
	zone, err := geo.Timezone(in.Zip)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		// Decide whether the profile is new only after concurrent identity creation finishes.
		if err := reserveEmailIdentity(ctx, q, email, identityDonor, pgtype.UUID{}); err != nil {
			return err
		}
		_, lookup := q.DonorByEmail(ctx, email)
		if lookup != nil && !errors.Is(lookup, pgx.ErrNoRows) {
			return lookup
		}
		now := time.Now().UTC()
		d, err := lockGuest(ctx, q, s.FoundingClaims, email, now)
		if err != nil {
			return err
		}
		if errors.Is(lookup, pgx.ErrNoRows) {
			if err = q.SaveSignupProfile(ctx, queries.SaveSignupProfileParams{ID: d.ID, FullName: text(in.Name), Zip: text(in.Zip), City: text(in.City), Timezone: text(zone)}); err != nil {
				return err
			}
		}
		return queueLoginEmail(ctx, q, email, dashboardPath, now)
	})
}

func (s PasswordService) Set(ctx context.Context, token, current, password string) error {
	if len(password) < 12 || len(password) > 72 {
		return ErrPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordCost)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		u, err := lockSignedIn(ctx, q, token)
		if err != nil {
			return err
		}
		old, err := q.DonorPassword(ctx, u.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		verified, e := q.PasswordReauthentication(
			ctx,
			queries.PasswordReauthenticationParams{TokenHash: HashToken(token), AsOf: timestamp(time.Now().UTC())},
		)
		if e != nil {
			return e
		}
		if !verified && (err != nil || bcrypt.CompareHashAndPassword([]byte(old), []byte(current)) != nil) {
			return ErrCurrentPassword
		}
		if !u.ClaimedAt.Valid {
			return ErrDenied
		}
		return q.SetDonorPassword(
			ctx,
			queries.SetDonorPasswordParams{DonorID: u.ID, PasswordHash: string(hash), UpdatedAt: timestamp(time.Now().UTC())},
		)
	})
}

func (s PasswordService) SignIn(ctx context.Context, email, password string, client Client) (Tokens, error) {
	canonical, err := identity.NormalizeEmail(email)
	if err != nil {
		return Tokens{}, ErrSignInCode
	}
	if len(password) > 72 {
		return Tokens{}, ErrSignInCode
	}
	q := queries.New(s.DB)
	d, err := q.DonorByEmail(ctx, canonical)
	hash := unknownAccountHash
	eligible := err == nil && !d.Disabled && d.ClaimedAt.Valid
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, err
	}
	if eligible {
		stored, e := q.DonorPassword(ctx, d.ID)
		if e == nil {
			hash = stored
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return Tokens{}, e
		} else {
			eligible = false
		}
	}
	if err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil || !eligible {
		return Tokens{}, ErrSignInCode
	}
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (Tokens, error) {
		q := queries.New(tx)
		locked, err := q.LockDonor(ctx, d.ID)
		if err != nil {
			return Tokens{}, err
		}
		if locked.Disabled || locked.Email != canonical {
			return Tokens{}, ErrSignInCode
		}
		current, err := q.DonorPassword(ctx, d.ID)
		if err != nil {
			return Tokens{}, err
		}
		if current != hash {
			return Tokens{}, ErrSignInCode
		}
		return issueSession(ctx, q, d.ID, client, time.Now().UTC())
	})
}
