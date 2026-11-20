package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AccountService struct{ DB *pgxpool.Pool }

// ClaimAccount redeems a single-use sign-in proof for a new session for
// client and revokes previousSession, the session the client held before, if
// any.
func (s AccountService) ClaimAccount(ctx context.Context, proof, previousSession string, client Client) (Tokens, error) {
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (Tokens, error) {
		issued, donor, err := claim(ctx, tx, proof, client)
		if err != nil {
			return Tokens{}, err
		}
		now := time.Now().UTC()
		if _, err = refreshProjection(ctx, tx, donor.ID, now); err != nil {
			return Tokens{}, err
		}
		return issued, queries.New(tx).
			RevokeSession(ctx, queries.RevokeSessionParams{TokenHash: HashToken(previousSession), RevokedAt: timestamp(now)})
	})
}

// claim consumes a sign-in proof and every other open link for its donor,
// marks the account claimed and issues a session for client. The session is
// usable only once tx commits.
func claim(ctx context.Context, tx pgx.Tx, proof string, client Client) (Tokens, queries.Donor, error) {
	q := queries.New(tx)
	link, err := q.LoginLink(ctx, HashToken(proof))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, queries.Donor{}, ErrDenied
	}
	if err != nil {
		return Tokens{}, queries.Donor{}, err
	}
	donor, err := q.LockDonor(ctx, link.DonorID)
	if err != nil {
		return Tokens{}, queries.Donor{}, err
	}
	if donor.Disabled {
		return Tokens{}, queries.Donor{}, ErrDenied
	}
	now := time.Now().UTC()
	if _, err = q.RedeemLoginLink(ctx, queries.RedeemLoginLinkParams{ID: link.ID, ConsumedAt: timestamp(now)}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrDenied
		}
		return Tokens{}, queries.Donor{}, err
	}
	if err = q.ConsumeDonorLinks(ctx, queries.ConsumeDonorLinksParams{DonorID: donor.ID, ConsumedAt: timestamp(now)}); err != nil {
		return Tokens{}, queries.Donor{}, err
	}
	if err = q.MarkClaimed(ctx, queries.MarkClaimedParams{ID: donor.ID, ClaimedAt: timestamp(now)}); err != nil {
		return Tokens{}, queries.Donor{}, err
	}
	tokens, err := issueSession(ctx, q, donor.ID, client, now)
	if err == nil {
		// The emailed link proves control of the account, so it stands in for the current password during the
		// reauthentication window and lets a donor with no password set one.
		err = q.MarkPasswordReauthenticated(
			ctx,
			queries.MarkPasswordReauthenticatedParams{TokenHash: HashToken(tokens.Access), PasswordVerifiedAt: timestamp(now)},
		)
	}
	return tokens, donor, err
}

func (s AccountService) SignOut(ctx context.Context, token string) error {
	return queries.New(s.DB).RevokeSession(ctx, queries.RevokeSessionParams{TokenHash: HashToken(token), RevokedAt: timestamp(time.Now())})
}

// QueueLogin queues a sign-in link for email's donor that returns them to
// next when ValidReturnPath accepts it; any other next returns them to the default page.
// An unknown address is not an error, so the response cannot reveal whether
// an account exists.
func (s AccountService) QueueLogin(ctx context.Context, email, next string) error {
	canonical, err := identity.NormalizeEmail(email)
	if err != nil {
		return err
	}
	if !ValidReturnPath(next) {
		next = ""
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		return queueLoginEmail(ctx, queries.New(tx), canonical, next, time.Now().UTC())
	})
}
