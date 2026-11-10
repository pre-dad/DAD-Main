package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"sync"
	"time"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/oidc"
	"dad/donor-api/internal/sms"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSignInUnavailable is returned when a sign-in method is not configured on
// this deployment (no SMS provider, or no OIDC provider).
var ErrSignInUnavailable = errors.New("this sign-in method is not available")

// ErrSignInCode is returned for an expired, missing, or wrong SMS code, and for
// a verified identity with no matching account. It is deliberately one error so
// a caller cannot tell "no account" from "wrong code" — neither reveals whether
// an account exists.
var ErrSignInCode = errors.New("that code is incorrect or has expired")

// phonePatternStore mirrors the ck_donor_phone constraint (E.164).
var phonePatternStore = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)

const (
	phoneCodeTTL      = 10 * time.Minute
	phoneCodeMaxTries = 5
	// phoneWindowMaxTries bounds wrong guesses for a phone across resends in a
	// rolling day, keeping a sustained guess rate near 20 in a million a day.
	phoneWindowMaxTries = 20
	phoneGuessWindow    = 24 * time.Hour
	phoneSendTimeout    = 15 * time.Second
)

// windowSpent reports whether the phone has used its day's wrong guesses.
func windowSpent(code queries.DonorPhoneCode, now time.Time) bool {
	return code.WindowAttempts >= phoneWindowMaxTries && now.Before(code.WindowStartedAt.Time.Add(phoneGuessWindow))
}

// PhoneService runs phone/SMS sign-in. Sender is nil when SMS is unconfigured.
type PhoneService struct {
	DB     *pgxpool.Pool
	Sender sms.Sender
	// sends, when set, tracks background sends so tests can wait for them.
	sends *sync.WaitGroup
}

// BeginSignIn sends a fresh one-time code to phone. An unknown or malformed
// phone is not an error (the response never reveals whether an account exists);
// a code is only stored and sent for a phone that matches an account, and a
// failed send is reported to operators, not the caller.
func (s PhoneService) BeginSignIn(ctx context.Context, phone string) error {
	if s.Sender == nil {
		return ErrSignInUnavailable
	}
	if !phonePatternStore.MatchString(phone) {
		return nil
	}
	code, err := numericCode(6)
	if err != nil {
		return err
	}
	sent := false
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		donor, err := q.DonorByPhone(ctx, text(phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no account: do not store or send, but report success
		} else if err != nil {
			return err
		}
		donor, err = q.LockDonor(ctx, donor.ID)
		if err != nil {
			return err
		}
		if donor.Disabled || donor.Phone.String != phone {
			return nil
		}
		pending, err := q.PhoneCode(ctx, phone)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && (time.Since(pending.CreatedAt.Time) < time.Minute || windowSpent(pending, time.Now())) {
			return nil
		}
		sent = true
		return q.UpsertPhoneCode(ctx, queries.UpsertPhoneCodeParams{
			Phone: phone, CodeHash: HashToken(code), ExpiresAt: timestamp(time.Now().UTC().Add(phoneCodeTTL)),
		})
	}); err != nil {
		return err
	}
	if !sent {
		return nil
	}
	// The send runs after the response so that neither its outcome nor its
	// latency tells the caller whether the number has an account.
	message := fmt.Sprintf("Your DAD sign-in code is %s. It expires in 10 minutes.", code)
	send, cancel := context.WithTimeout(context.WithoutCancel(ctx), phoneSendTimeout)
	if s.sends != nil {
		s.sends.Add(1)
	}
	go func() {
		defer cancel()
		if s.sends != nil {
			defer s.sends.Done()
		}
		if err := s.Sender.Send(send, phone, message); err != nil {
			slog.Warn("sign-in code not sent", "cause", fmt.Sprintf("%T", err))
		}
	}()
	return nil
}

// FinishSignIn verifies the code for phone and issues a session for client.
// Wrong, expired, over-tried, or no-account all collapse to ErrSignInCode.
func (s PhoneService) FinishSignIn(ctx context.Context, phone, code string, client Client) (Tokens, error) {
	if s.Sender == nil {
		return Tokens{}, ErrSignInUnavailable
	}
	if !phonePatternStore.MatchString(phone) {
		return Tokens{}, ErrSignInCode
	}
	rejected := false
	tokens, err := transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (Tokens, error) {
		q := queries.New(tx)
		donor, err := q.DonorByPhone(ctx, text(phone))
		if errors.Is(err, pgx.ErrNoRows) {
			return Tokens{}, ErrSignInCode
		}
		if err != nil {
			return Tokens{}, err
		}
		donor, err = q.LockDonor(ctx, donor.ID)
		if err != nil {
			return Tokens{}, err
		}
		if donor.Disabled || donor.Phone.String != phone {
			return Tokens{}, ErrSignInCode
		}
		pending, err := q.PhoneCode(ctx, phone)
		if errors.Is(err, pgx.ErrNoRows) {
			return Tokens{}, ErrSignInCode
		}
		if err != nil {
			return Tokens{}, err
		}
		now := time.Now().UTC()
		expired := !now.Before(pending.ExpiresAt.Time)
		overTried := pending.Attempts >= phoneCodeMaxTries || windowSpent(pending, now)
		matched := HashToken(code) == pending.CodeHash
		if expired || overTried || !matched {
			if !expired && !overTried {
				// Commit the guess while holding the code lock, then reject it.
				rejected = true
				return Tokens{}, q.RecordPhoneCodeAttempt(ctx, phone)
			}
			return Tokens{}, ErrSignInCode
		}
		if err := q.DeletePhoneCode(ctx, phone); err != nil {
			return Tokens{}, err
		}
		return issueSession(ctx, q, donor.ID, client, now)
	})
	if err == nil && rejected {
		return Tokens{}, ErrSignInCode
	}
	return tokens, err
}

// OIDCService runs Apple/Google sign-in. Verifier/Settings are nil when OIDC is
// unconfigured.
type OIDCService struct {
	DB       *pgxpool.Pool
	Verifier oidc.Verifier
	Settings *oidc.Settings
}

// SignIn verifies a provider id_token, checks its nonce binding, and issues a session for the donor it
// belongs to. A verified identity is matched first by a prior link, then by the
// verified email (which also records the link). A first-time verified identity
// creates a donor account, subject to the same email-role checks as signup.
func (s OIDCService) SignIn(ctx context.Context, provider oidc.Provider, idToken, nonce string, client Client) (Tokens, error) {
	if s.Verifier == nil || !s.Settings.Configured(provider) {
		return Tokens{}, ErrSignInUnavailable
	}
	ident, err := s.Verifier.Verify(ctx, provider, idToken)
	if errors.Is(err, oidc.ErrToken) {
		return Tokens{}, ErrSignInCode
	}
	if err != nil {
		return Tokens{}, err
	}
	if !s.Settings.NonceAccepted(ident, nonce) {
		return Tokens{}, ErrSignInCode
	}
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (Tokens, error) {
		q := queries.New(tx)
		now := time.Now().UTC()
		issue := func(id pgtype.UUID) (Tokens, error) {
			if err := q.MarkClaimed(ctx, queries.MarkClaimedParams{ID: id, ClaimedAt: timestamp(now)}); err != nil {
				return Tokens{}, err
			}
			return issueSession(ctx, q, id, client, now)
		}

		// Already linked: sign that donor in.
		if link, err := q.OIDCIdentity(ctx, queries.OIDCIdentityParams{
			Provider: string(ident.Provider), Subject: ident.Subject,
		}); err == nil {
			donor, err := q.LockDonor(ctx, link.DonorID)
			if err != nil {
				return Tokens{}, err
			}
			if donor.Disabled {
				return Tokens{}, ErrSignInCode
			}
			if err := q.TouchOIDCIdentity(ctx, queries.TouchOIDCIdentityParams{ID: link.ID, LastUsedAt: timestamp(now)}); err != nil {
				return Tokens{}, err
			}
			return issue(donor.ID)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Tokens{}, err
		}

		// Not linked: find or create the donor using the verified email.
		if ident.Email == "" {
			return Tokens{}, ErrSignInCode
		}
		canonical, err := identity.NormalizeEmail(ident.Email)
		if err != nil {
			return Tokens{}, ErrSignInCode
		}
		donor, err := q.DonorByEmail(ctx, canonical)
		if errors.Is(err, pgx.ErrNoRows) {
			donor, err = lockGuest(ctx, q, nil, canonical, now)
		}
		if err != nil {
			return Tokens{}, err
		}
		locked, err := q.LockDonor(ctx, donor.ID)
		if err != nil {
			return Tokens{}, err
		}
		if locked.Disabled || locked.Email != canonical {
			return Tokens{}, ErrSignInCode
		}
		id, err := NewID()
		if err != nil {
			return Tokens{}, err
		}
		if err := q.InsertOIDCIdentity(ctx, queries.InsertOIDCIdentityParams{
			ID: id, DonorID: locked.ID, Provider: string(ident.Provider), Subject: ident.Subject,
			LastUsedAt: timestamp(now),
		}); err != nil {
			return Tokens{}, err
		}
		return issue(locked.ID)
	})
}

// numericCode returns an n-digit base-10 code with uniform digits.
func numericCode(n int) (string, error) {
	const digits = "0123456789"
	buf := make([]byte, n)
	for i := range buf {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			return "", err
		}
		buf[i] = digits[idx.Int64()]
	}
	return string(buf), nil
}
