package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/identity"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Partner sign-in link kinds: the approval email to an application's signer,
// an invitation from a partner admin, and a link requested from the portal.
const (
	partnerLinkApproved = "APPROVED"
	partnerLinkInvited  = "INVITED"
	partnerLinkSignIn   = "SIGN_IN"
)

func partnerLinkLifetime(kind string) time.Duration {
	if kind == partnerLinkSignIn {
		return proofLifetime
	}
	return partnerInviteLifetime
}

func queuePartnerLogin(ctx context.Context, q *queries.Queries, userID pgtype.UUID, kind string, now time.Time) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.QueuePartnerLoginEmail(ctx, queries.QueuePartnerLoginEmailParams{
		ID: id, PartnerUserID: userID, Kind: kind, AvailableAt: timestamp(now),
	})
}

// RequestSignIn emails a sign-in link to each enabled user of an approved
// partner who signs in with email. Its result never reveals whether any exist.
func (s PartnerPortalService) RequestSignIn(ctx context.Context, email string) error {
	email, err := identity.NormalizeEmail(email)
	if err != nil {
		return ErrPartnerApplication
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		users, err := q.PartnerUsersForSignIn(ctx, email)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, user := range users {
			if err := queuePartnerLogin(ctx, q, user, partnerLinkSignIn, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// PartnerLoginEmailWorker delivers partner sign-in links. Like LoginEmailWorker
// it commits the link before any SMTP side effect and resends the same link on
// retry, but a partner user holds one live link at a time, so a link replaced
// by a newer one is dropped rather than sent.
type PartnerLoginEmailWorker struct {
	DB        *pgxpool.Pool
	Settings  mail.Settings
	Origin    string
	Transport mail.Transport
}

// DeliverNext sends the next due partner sign-in link, reporting whether it
// found work.
func (w PartnerLoginEmailWorker) DeliverNext(ctx context.Context, now time.Time) (bool, error) {
	site, err := mailSite(w.Settings, w.Origin, w.Transport)
	if err != nil {
		return false, err
	}
	id, message, worked, err := w.claim(ctx, site, now)
	if err != nil || message == nil {
		return worked, err
	}
	q := queries.New(w.DB)
	if err = w.Transport.Send(ctx, *message); err != nil {
		return true, q.FailPartnerLoginEmail(ctx, queries.FailPartnerLoginEmailParams{
			ID: id, LastError: text("DeliveryError"), AvailableAt: timestamp(now.Add(time.Minute)),
		})
	}
	return true, q.CompletePartnerLoginEmail(ctx, queries.CompletePartnerLoginEmailParams{ID: id, CompletedAt: timestamp(now)})
}

// claim selects the next due link, mints it on first use, and reserves it for
// one send, which happens after the transaction commits. It returns no message
// when the job was finished instead.
func (w PartnerLoginEmailWorker) claim(ctx context.Context, site publicSite, now time.Time) (pgtype.UUID, *mail.Message, bool, error) {
	var id pgtype.UUID
	var message *mail.Message
	worked := false
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		job, err := q.NextPartnerLoginEmail(
			ctx,
			queries.NextPartnerLoginEmailParams{AvailableAt: timestamp(now), MaxAttempts: MaxDeliveryAttempts},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		complete := func() error {
			return q.CompletePartnerLoginEmail(ctx, queries.CompletePartnerLoginEmailParams{ID: job.ID, CompletedAt: timestamp(now)})
		}
		user, err := q.LockPartnerLoginRecipient(ctx, job.PartnerUserID)
		if err != nil {
			return err
		}
		if user.Disabled || !user.ApprovedAt.Valid || (job.ExpiresAt.Valid && !job.ExpiresAt.Time.After(now)) {
			return complete()
		}
		var token string
		if job.EncryptedToken == nil {
			if token, err = randomToken(32); err != nil {
				return err
			}
			ciphertext, err := w.Settings.EncryptionKeys.Encrypt([]byte(token))
			if err != nil {
				return err
			}
			job.ExpiresAt = timestamp(now.Add(partnerLinkLifetime(job.Kind)))
			if err = q.SetPartnerUserLink(ctx, queries.SetPartnerUserLinkParams{
				ID: user.ID, InviteTokenHash: text(HashToken(token)), InviteExpiresAt: job.ExpiresAt,
			}); err != nil {
				return err
			}
			if err = q.PreparePartnerLoginEmail(ctx, queries.PreparePartnerLoginEmailParams{
				ID: job.ID, EncryptedToken: ciphertext, ExpiresAt: job.ExpiresAt,
			}); err != nil {
				return err
			}
		} else {
			if token, err = loginToken(job.EncryptedToken, w.Settings.EncryptionKeys); err != nil {
				category := "DeliveryError"
				if errors.Is(err, credentials.ErrInvalid) {
					category = "InvalidToken"
				}
				return q.FailUnreadablePartnerLoginEmail(ctx, queries.FailUnreadablePartnerLoginEmailParams{
					ID: job.ID, LastError: text(category), AvailableAt: timestamp(now.Add(time.Minute)),
				})
			}
			if user.InviteTokenHash.String != HashToken(token) {
				return complete()
			}
		}
		if err = q.ClaimPartnerLoginEmail(ctx, queries.ClaimPartnerLoginEmailParams{
			ID: job.ID, AvailableAt: timestamp(now.Add(deliveryLease)),
		}); err != nil {
			return err
		}
		messageID, err := mail.MessageID(w.Settings.Sender, "partner-login", job.ID.String())
		if err != nil {
			return err
		}
		subject, body := partnerLoginEmail(job.Kind, user, site.partnerSignIn(token))
		id = job.ID
		message = &mail.Message{From: w.Settings.Sender, To: user.Email, Subject: subject, Body: body, ID: messageID}
		return nil
	})
	if err != nil {
		return pgtype.UUID{}, nil, false, err
	}
	return id, message, worked, nil
}

func partnerLinkLifetimeText(kind string) string {
	d := partnerLinkLifetime(kind)
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return fmt.Sprintf("%d minutes", d/time.Minute)
}

func partnerLoginEmail(kind string, user queries.LockPartnerLoginRecipientRow, link string) (string, string) {
	org := user.Organization
	lifetime := "This single-use link expires in " + partnerLinkLifetimeText(kind) + ".\n"
	const unexpected = "If you were not expecting this email, you can ignore it.\n"
	switch kind {
	case partnerLinkApproved:
		return "Your DAD partnership is approved",
			org + " has been approved as a DAD partner, and you are its partner administrator.\n\n" +
				"Sign in to your partner portal to see your verified dashboard and invite your colleagues:\n" + link + "\n\n" +
				lifetime + unexpected
	case partnerLinkInvited:
		role := "viewer"
		if user.Role == "ADMIN" {
			role = "administrator"
		}
		return "You're invited to " + org + "'s DAD partner portal",
			"You have been invited to " + org + "'s DAD partner portal as a " + role + ".\n\n" +
				"Accept your invitation and sign in:\n" + link + "\n\n" +
				lifetime + unexpected
	default:
		return "Sign in to the DAD partner portal",
			"Use this link to sign in to " + org + "'s DAD partner portal:\n" + link + "\n\n" +
				lifetime +
				"If you did not request it, ignore this email. No access is granted until you confirm on the page.\n"
	}
}
