package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	staffSessionLifetime = 8 * time.Hour
	ceremonyLifetime     = 5 * time.Minute
	maxInviteAttempts    = 5
)

// Ceremony is one WebAuthn challenge. The browser passes Options to the
// WebAuthn API and answers with the resulting credential and the ceremony ID.
type Ceremony struct {
	ID      string `json:"ceremony"`
	Options any    `json:"options"`
}

// Invitation is the passkey registration for an invited account, naming the
// person it was issued to so they can recognise it as theirs.
type Invitation struct {
	Ceremony
	Email string `json:"email"`
	Name  string `json:"name"`
}

// staffUser presents an account to WebAuthn. Its user handle is the account
// ID, which is how a usernameless sign-in names its owner.
type staffUser struct {
	staff    queries.DonorStaff
	passkeys []webauthn.Credential
}

func (u staffUser) WebAuthnID() []byte                         { return u.staff.ID.Bytes[:] }
func (u staffUser) WebAuthnName() string                       { return u.staff.Email }
func (u staffUser) WebAuthnDisplayName() string                { return u.staff.Name }
func (u staffUser) WebAuthnCredentials() []webauthn.Credential { return u.passkeys }

// lockStaffUser locks an account, then its passkeys.
func lockStaffUser(ctx context.Context, q *queries.Queries, id pgtype.UUID) (staffUser, error) {
	staff, err := q.StaffByID(ctx, id)
	if err != nil {
		return staffUser{}, err
	}
	rows, err := q.StaffPasskeys(ctx, id)
	if err != nil {
		return staffUser{}, err
	}
	user := staffUser{staff: staff}
	for _, row := range rows {
		flags := protocol.FlagUserPresent
		if row.UserVerified {
			flags |= protocol.FlagUserVerified
		}
		if row.BackupEligible {
			flags |= protocol.FlagBackupEligible
		}
		if row.BackupState {
			flags |= protocol.FlagBackupState
		}
		transports := make([]protocol.AuthenticatorTransport, len(row.Transports))
		for i, transport := range row.Transports {
			transports[i] = protocol.AuthenticatorTransport(transport)
		}
		user.passkeys = append(user.passkeys, webauthn.Credential{
			ID: row.CredentialID, PublicKey: row.PublicKey, AttestationFormat: row.AttestationFormat, Transport: transports,
			Flags:         webauthn.NewCredentialFlags(flags),
			Authenticator: webauthn.Authenticator{AAGUID: row.Aaguid, SignCount: uint32(row.SignCount)},
		})
	}
	return user, nil
}

// saveCeremony keeps a challenge until it expires and returns its ID.
func saveCeremony(ctx context.Context, q *queries.Queries, kind string, invite pgtype.UUID, session *webauthn.SessionData) (string, error) {
	raw, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	id, err := randomToken(32)
	if err != nil {
		return "", err
	}
	return id, q.InsertStaffCeremony(
		ctx,
		queries.InsertStaffCeremonyParams{
			TokenHash: HashToken(id),
			Kind:      kind,
			InviteID:  invite,
			Session:   raw,
			ExpiresAt: timestamp(session.Expires),
		},
	)
}

// takeCeremony deletes a ceremony and returns its challenge, so each one can
// be answered once.
func takeCeremony(ctx context.Context, q *queries.Queries, id, kind string, now time.Time) (webauthn.SessionData, pgtype.UUID, error) {
	var session webauthn.SessionData
	row, err := q.TakeStaffCeremony(ctx, queries.TakeStaffCeremonyParams{TokenHash: HashToken(id), Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.ExpiresAt.Time.After(now)) {
		return session, row.InviteID, ErrStaffDenied
	}
	if err != nil {
		return session, row.InviteID, err
	}
	return session, row.InviteID, json.Unmarshal(row.Session, &session)
}

func startStaffSession(ctx context.Context, q *queries.Queries, staff pgtype.UUID, now time.Time) (string, error) {
	token, err := randomToken(48)
	if err != nil {
		return "", err
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	return token, q.InsertStaffSession(
		ctx,
		queries.InsertStaffSessionParams{
			ID:        id,
			StaffID:   staff,
			TokenHash: HashToken(token),
			ExpiresAt: timestamp(now.Add(staffSessionLifetime)),
		},
	)
}

// BeginSignIn issues a usernameless challenge: any of the browser's passkeys
// for the staff host may answer it, so nothing reveals which accounts exist.
func (s *StaffService) BeginSignIn(ctx context.Context) (Ceremony, error) {
	assertion, session, err := s.WebAuthn.BeginDiscoverableLogin()
	if err != nil {
		return Ceremony{}, err
	}
	id, err := saveCeremony(ctx, queries.New(s.DB), "SIGN_IN", pgtype.UUID{}, session)
	return Ceremony{ID: id, Options: assertion.Response}, err
}

// SignIn verifies a passkey assertion against its ceremony and returns a new
// session token. A failure with a known passkey is audited against its owner
// with its reason. A signature counter that fails to advance is treated as a
// cloned passkey; synced passkeys always report zero, which is accepted.
func (s *StaffService) SignIn(ctx context.Context, ceremony string, response []byte) (string, error) {
	assertion, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return "", ErrStaffInput
	}
	var token string
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		now := time.Now().UTC()
		session, _, err := takeCeremony(ctx, q, ceremony, "SIGN_IN", now)
		if err != nil {
			return err
		}
		owner, err := q.StaffPasskeyOwner(ctx, assertion.RawID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The consumed ceremony is committed, then the denial reported.
			return nil
		}
		if err != nil {
			return err
		}
		user, err := lockStaffUser(ctx, q, owner)
		if err != nil {
			return err
		}
		_, passkey, err := s.WebAuthn.ValidatePasskeyLogin(
			func([]byte, []byte) (webauthn.User, error) { return user, nil },
			session,
			assertion,
		)
		var reason string
		switch {
		case err != nil:
			reason = "invalid"
		case passkey.Authenticator.CloneWarning:
			reason = "counter"
		case user.staff.Disabled:
			reason = "disabled"
		}
		if reason != "" {
			return auditStaff(ctx, q, pgtype.UUID{}, "staff.sign_in_failed", owner.String(), map[string]string{"reason": reason}, now)
		}
		err = q.RecordStaffPasskeyUse(ctx, queries.RecordStaffPasskeyUseParams{
			CredentialID: passkey.ID,
			SignCount:    int64(passkey.Authenticator.SignCount),
			UserVerified: passkey.Flags.UserVerified,
			BackupState:  passkey.Flags.BackupState,
			LastUsedAt:   timestamp(now),
		})
		if err != nil {
			return err
		}
		if token, err = startStaffSession(ctx, q, owner, now); err != nil {
			return err
		}
		return auditStaff(ctx, q, owner, "staff.signed_in", owner.String(), nil, now)
	})
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrStaffDenied
	}
	return token, nil
}

// lockInvitation locks an account and then its invitation, the order every
// staff write takes, and checks that the invitation is still open.
func lockInvitation(
	ctx context.Context,
	q *queries.Queries,
	staffID pgtype.UUID,
	invitation func() (queries.DonorStaffInvite, error),
) (queries.DonorStaffInvite, staffUser, error) {
	user, err := lockStaffUser(ctx, q, staffID)
	if err != nil {
		return queries.DonorStaffInvite{}, user, err
	}
	invite, err := invitation()
	if err != nil {
		return invite, user, err
	}
	now, err := q.DatabaseNow(ctx)
	if err != nil {
		return invite, user, err
	}
	if invite.StaffID != user.staff.ID || user.staff.Disabled || invite.ConsumedAt.Valid || !invite.ExpiresAt.Time.After(now.Time) ||
		invite.Attempts >= maxInviteAttempts {
		return invite, user, ErrStaffDenied
	}
	return invite, user, nil
}

func staffInvitation(ctx context.Context, q *queries.Queries, token string) (queries.DonorStaffInvite, staffUser, error) {
	owner, err := q.StaffInviteOwner(ctx, HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return queries.DonorStaffInvite{}, staffUser{}, ErrStaffDenied
	}
	if err != nil {
		return queries.DonorStaffInvite{}, staffUser{}, err
	}
	return lockInvitation(ctx, q, owner, func() (queries.DonorStaffInvite, error) { return q.StaffInviteByToken(ctx, HashToken(token)) })
}

// BeginActivation returns the passkey registration for an invitation. The
// passkey must be discoverable and verify its user, so it can later sign in
// without an email address.
func (s *StaffService) BeginActivation(ctx context.Context, token string) (Invitation, error) {
	var result Invitation
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		invite, user, err := staffInvitation(ctx, q, token)
		if err != nil {
			return err
		}
		creation, session, err := s.WebAuthn.BeginRegistration(user)
		if err != nil {
			return err
		}
		id, err := saveCeremony(ctx, q, "ACTIVATION", invite.ID, session)
		result = Invitation{Ceremony: Ceremony{ID: id, Options: creation.Response}, Email: user.staff.Email, Name: user.staff.Name}
		return err
	})
	return result, err
}

// Activate stores the passkey answering an activation ceremony, consumes the
// invitation, and returns a session for its owner. Every answer to an open
// ceremony counts against the invitation, rejected ones too; a malformed
// answer, or one to an unknown or expired ceremony, does not.
func (s *StaffService) Activate(ctx context.Context, ceremony string, response []byte) (string, error) {
	attestation, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return "", ErrStaffInput
	}
	var token string
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		now := time.Now().UTC()
		session, inviteID, err := takeCeremony(ctx, q, ceremony, "ACTIVATION", now)
		if err != nil {
			return err
		}
		if len(session.UserID) != 16 {
			return ErrStaffDenied
		}
		owner := pgtype.UUID{Bytes: [16]byte(session.UserID), Valid: true}
		invite, user, err := lockInvitation(
			ctx,
			q,
			owner,
			func() (queries.DonorStaffInvite, error) { return q.StaffInviteByID(ctx, inviteID) },
		)
		if err != nil {
			return err
		}
		invite.Attempts++
		if token, err = s.register(ctx, q, user, session, attestation, now); err != nil {
			return err
		}
		if token != "" {
			invite.ConsumedAt = timestamp(now)
		}
		return q.SaveStaffInvite(
			ctx,
			queries.SaveStaffInviteParams{ID: invite.ID, Attempts: invite.Attempts, ConsumedAt: invite.ConsumedAt},
		)
	})
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrStaffDenied
	}
	return token, nil
}

// knownTransports are the transports a passkey may be stored with. The
// browser reports them unchecked, and the column holds only short names.
var knownTransports = []protocol.AuthenticatorTransport{
	protocol.USB,
	protocol.NFC,
	protocol.BLE,
	protocol.SmartCard,
	protocol.Hybrid,
	protocol.Internal,
}

// register stores a verified new passkey and starts a session, or audits the
// failure and returns no token when the attestation is rejected or its
// credential is registered already.
func (s *StaffService) register(
	ctx context.Context,
	q *queries.Queries,
	user staffUser,
	session webauthn.SessionData,
	attestation *protocol.ParsedCredentialCreationData,
	now time.Time,
) (string, error) {
	failed := func(reason string) (string, error) {
		return "", auditStaff(
			ctx,
			q,
			pgtype.UUID{},
			"staff.activation_failed",
			user.staff.ID.String(),
			map[string]string{"reason": reason},
			now,
		)
	}
	passkey, err := s.WebAuthn.CreateCredential(user, session, attestation)
	if err != nil {
		return failed("invalid")
	}
	// A credential registered to anyone already is rejected, not reassigned.
	switch _, err = q.StaffPasskeyOwner(ctx, passkey.ID); {
	case err == nil:
		return failed("credential_in_use")
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	transports := []string{}
	for _, transport := range passkey.Transport {
		if slices.Contains(knownTransports, transport) {
			transports = append(transports, string(transport))
		}
	}
	err = q.InsertStaffPasskey(ctx, queries.InsertStaffPasskeyParams{
		ID:                id,
		StaffID:           user.staff.ID,
		CredentialID:      passkey.ID,
		PublicKey:         passkey.PublicKey,
		SignCount:         int64(passkey.Authenticator.SignCount),
		Transports:        transports,
		Aaguid:            passkey.Authenticator.AAGUID,
		AttestationFormat: passkey.AttestationFormat,
		UserVerified:      passkey.Flags.UserVerified,
		BackupEligible:    passkey.Flags.BackupEligible,
		BackupState:       passkey.Flags.BackupState,
	})
	if err != nil {
		return "", err
	}
	if err = auditStaff(ctx, q, user.staff.ID, "staff.activated", user.staff.ID.String(), nil, now); err != nil {
		return "", err
	}
	token, err := startStaffSession(ctx, q, user.staff.ID, now)
	if err != nil {
		return "", err
	}
	return token, auditStaff(ctx, q, user.staff.ID, "staff.signed_in", user.staff.ID.String(), nil, now)
}
