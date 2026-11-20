package store

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type LoginEmailWorker struct {
	DB        *pgxpool.Pool
	Settings  mail.Settings
	Origin    string
	Transport mail.Transport
}

// mailSite checks a delivery worker's configuration before it selects work,
// so a misconfigured process cannot spend every queued message's retries.
func mailSite(settings mail.Settings, origin string, transport mail.Transport) (publicSite, error) {
	if err := settings.Validate(); err != nil {
		return "", err
	}
	if transport == nil {
		return "", mail.ErrConfiguration
	}
	return site(origin)
}

// DeliverNext mints and sends the next due sign-in link. It reports whether
// it found work.
func (w LoginEmailWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	site, err := mailSite(w.Settings, w.Origin, w.Transport)
	if err != nil {
		return false, err
	}
	id, worked, err := w.prepare(ctx, now)
	if err != nil || !worked || !id.Valid {
		return worked, err
	}
	message, err := w.claim(ctx, site, id, now)
	if err != nil || message == nil {
		return err == nil, err
	}
	q := queries.New(w.DB)
	if err = w.Transport.Send(ctx, *message); err != nil {
		err = q.FailLoginEmail(
			ctx,
			queries.FailLoginEmailParams{ID: id, LastError: text("DeliveryError"), AvailableAt: timestamp(now.Add(time.Minute))},
		)
	} else {
		err = q.CompleteLoginEmail(ctx, queries.CompleteLoginEmailParams{ID: id, CompletedAt: timestamp(now)})
	}
	return err == nil, err
}

// claim rechecks the job under the donor lock and reserves it for one send,
// which happens after the lock is released. It returns no message when the
// job was finished or failed instead.
func (w LoginEmailWorker) claim(ctx context.Context, site publicSite, id pgtype.UUID, now time.Time) (*mail.Message, error) {
	var message *mail.Message
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		job, err := q.LoginEmailByID(ctx, id)
		if err != nil {
			return err
		}
		if job.CompletedAt.Valid || job.AvailableAt.Time.After(now) || job.Attempts >= MaxDeliveryAttempts {
			return nil
		}
		donor, err := q.LockDonor(ctx, job.DonorID)
		if err != nil {
			return err
		}
		if donor.Disabled || !job.ExpiresAt.Valid || !job.ExpiresAt.Time.After(now) {
			return q.CompleteLoginEmail(ctx, queries.CompleteLoginEmailParams{ID: id, CompletedAt: timestamp(now)})
		}
		if err = q.ClaimLoginEmail(ctx, queries.ClaimLoginEmailParams{ID: id, AvailableAt: timestamp(now.Add(deliveryLease))}); err != nil {
			return err
		}
		token, err := loginToken(job.EncryptedToken, w.Settings.EncryptionKeys)
		if err != nil {
			category := "DeliveryError"
			if errors.Is(err, credentials.ErrInvalid) {
				category = "InvalidToken"
			}
			return q.FailLoginEmail(
				ctx,
				queries.FailLoginEmailParams{ID: id, LastError: text(category), AvailableAt: timestamp(now.Add(time.Minute))},
			)
		}
		messageID, err := mail.MessageID(w.Settings.Sender, "login", id.String())
		if err != nil {
			return err
		}
		message = &mail.Message{
			From:    w.Settings.Sender,
			To:      donor.Email,
			Subject: "Sign in to your DAD sandbox account",
			ID:      messageID,
			Body: "Use this link to claim or sign in to your DAD sandbox donor account.\n\n" + site.claim(
				token,
				job.ReturnPath.String,
			) + "\n\n" +
				"Confirm on the page to continue. This single-use link expires in " + proofMinutes + ".\n" +
				"If you did not request it, ignore this email. No account access is granted until you confirm. This is an account access message, not a tax receipt.\n",
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return message, nil
}

// loginToken decrypts a queued sign-in token. Tokens are 32 random bytes in
// unpadded URL-safe base64, the contract shared with every issued claim link.
func loginToken(ciphertext []byte, keys credentials.Keyring) (string, error) {
	token, err := keys.Decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(string(token))
	if err != nil || len(raw) != 32 {
		return "", credentials.ErrInvalid
	}
	return string(token), nil
}

// prepare selects the next due job and, on its first attempt, mints its
// single-use link. The link commits before any SMTP side effect; claim then
// rechecks the job under its lock, serializing competing workers. It returns
// no job when the one found was finished instead.
func (w LoginEmailWorker) prepare(ctx context.Context, now time.Time) (pgtype.UUID, bool, error) {
	var id pgtype.UUID
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		job, err := q.NextLoginEmail(ctx, queries.NextLoginEmailParams{AvailableAt: timestamp(now), MaxAttempts: MaxDeliveryAttempts})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		donor, err := q.LockDonor(ctx, job.DonorID)
		if err != nil {
			return err
		}
		if donor.Disabled {
			return q.CompleteLoginEmail(ctx, queries.CompleteLoginEmailParams{ID: job.ID, CompletedAt: timestamp(now)})
		}
		id = job.ID
		if job.EncryptedToken != nil {
			return nil
		}
		token, err := randomToken(32)
		if err != nil {
			return err
		}
		ciphertext, err := w.Settings.EncryptionKeys.Encrypt([]byte(token))
		if err != nil {
			return err
		}
		link, err := NewID()
		if err != nil {
			return err
		}
		expiry := timestamp(now.Add(proofLifetime))
		if err = q.InsertLoginLink(ctx, queries.InsertLoginLinkParams{ID: link, DonorID: donor.ID, TokenHash: HashToken(token), ExpiresAt: expiry}); err != nil {
			return err
		}
		return q.PrepareLoginEmail(ctx, queries.PrepareLoginEmailParams{ID: job.ID, EncryptedToken: ciphertext, ExpiresAt: expiry})
	})
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	return id, worked, nil
}
