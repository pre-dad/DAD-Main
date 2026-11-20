package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"time"
	"unicode/utf8"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store/queries"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPasskeyDenied is returned when a passkey ceremony or credential check fails.
var ErrPasskeyDenied = errors.New("passkey ceremony invalid or expired")

// MaxPasskeyNameRunes bounds the optional human-readable name a donor gives a passkey.
const MaxPasskeyNameRunes = 64

type PasskeyInfo struct {
	ID         pgtype.UUID `json:"id"`
	Name       *string     `json:"name"`
	CreatedAt  time.Time   `json:"created_at"`
	LastUsedAt *time.Time  `json:"last_used_at"`
}

type PasskeyList struct {
	Passkeys []PasskeyInfo `json:"passkeys"`
}

type DonorPasskeyService struct {
	DB       *pgxpool.Pool
	WebAuthn *webauthn.WebAuthn
}

// NewDonorPasskeyService constructs the service, deriving the WebAuthn
// relying-party ID from the public origin's hostname. This must match the
// domain in apple-app-site-association's webcredentials section.
func NewDonorPasskeyService(db *pgxpool.Pool, publicOrigin string) (*DonorPasskeyService, error) {
	if err := payments.ValidateOrigin(publicOrigin); err != nil {
		return nil, err
	}
	u, err := url.Parse(publicOrigin)
	if err != nil {
		return nil, err
	}
	ceremony := webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyLifetime, TimeoutUVD: ceremonyLifetime}
	// Sign-in is usernameless, so only a discoverable credential can answer it.
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName:         "Dynamic Aid Delivery",
		RPID:                  u.Hostname(),
		RPOrigins:             []string{publicOrigin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: ceremony, Registration: ceremony},
	})
	if err != nil {
		return nil, err
	}
	return &DonorPasskeyService{DB: db, WebAuthn: wa}, nil
}

// donorUser presents a donor account to WebAuthn. Its user handle is the
// donor ID so a usernameless sign-in names its owner.
type donorUser struct {
	donor    queries.Donor
	passkeys []webauthn.Credential
	session  pgtype.UUID
}

func (u donorUser) WebAuthnID() []byte                         { return u.donor.ID.Bytes[:] }
func (u donorUser) WebAuthnName() string                       { return u.donor.Email }
func (u donorUser) WebAuthnDisplayName() string                { return u.donor.Email }
func (u donorUser) WebAuthnCredentials() []webauthn.Credential { return u.passkeys }

// loadDonorUser locks a donor and loads their passkeys, for use inside a
// transaction that holds the donor lock.
func loadDonorUser(ctx context.Context, q *queries.Queries, id pgtype.UUID) (donorUser, error) {
	d, err := q.LockDonor(ctx, id)
	if err != nil {
		return donorUser{}, err
	}
	rows, err := q.DonorPasskeys(ctx, id)
	if err != nil {
		return donorUser{}, err
	}
	user := donorUser{donor: d}
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
		for i, t := range row.Transports {
			transports[i] = protocol.AuthenticatorTransport(t)
		}
		user.passkeys = append(user.passkeys, webauthn.Credential{
			ID: row.CredentialID, PublicKey: row.PublicKey, AttestationFormat: row.AttestationFormat,
			Transport: transports, Flags: webauthn.NewCredentialFlags(flags),
			Authenticator: webauthn.Authenticator{AAGUID: row.Aaguid, SignCount: uint32(row.SignCount)},
		})
	}
	return user, nil
}

func (s *DonorPasskeyService) sweepCeremonies(ctx context.Context) error {
	return queries.New(s.DB).DeleteExpiredDonorCeremonies(ctx, timestamp(time.Now()))
}

type donorCeremony struct {
	webauthn.SessionData
	SessionID pgtype.UUID `json:"session_id"`
}

func saveDonorCeremony(
	ctx context.Context,
	q *queries.Queries,
	kind string,
	donorID, sessionID pgtype.UUID,
	session *webauthn.SessionData,
) (string, error) {
	raw, err := json.Marshal(donorCeremony{SessionData: *session, SessionID: sessionID})
	if err != nil {
		return "", err
	}
	id, err := randomToken(32)
	if err != nil {
		return "", err
	}
	return id, q.InsertDonorCeremony(ctx, queries.InsertDonorCeremonyParams{
		TokenHash: HashToken(id), Kind: kind, DonorID: donorID,
		Session: raw, ExpiresAt: timestamp(session.Expires),
	})
}

func takeDonorCeremony(ctx context.Context, q *queries.Queries, id, kind string, now time.Time) (donorCeremony, pgtype.UUID, error) {
	var session donorCeremony
	row, err := q.TakeDonorCeremony(ctx, queries.TakeDonorCeremonyParams{TokenHash: HashToken(id), Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.ExpiresAt.Time.After(now)) {
		return session, row.DonorID, ErrPasskeyDenied
	}
	if err != nil {
		return session, row.DonorID, err
	}
	return session, row.DonorID, json.Unmarshal(row.Session, &session)
}

// answerCeremony runs answer in a transaction that commits even when the
// answer is refused, so any answer spends its ceremony; the refusal is
// reported once the commit has made that permanent.
func answerCeremony(ctx context.Context, db *pgxpool.Pool, answer func(*queries.Queries) error) error {
	denied := false
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		err := answer(queries.New(tx))
		if errors.Is(err, ErrPasskeyDenied) {
			denied = true
			return nil
		}
		return err
	})
	if err == nil && denied {
		return ErrPasskeyDenied
	}
	return err
}

// BeginSignIn issues a usernameless discoverable-login challenge. Any of the
// browser's passkeys for the public origin may answer it.
func (s *DonorPasskeyService) BeginSignIn(ctx context.Context) (Ceremony, error) {
	if err := s.sweepCeremonies(ctx); err != nil {
		return Ceremony{}, err
	}
	assertion, session, err := s.WebAuthn.BeginDiscoverableLogin()
	if err != nil {
		return Ceremony{}, err
	}
	id, err := saveDonorCeremony(ctx, queries.New(s.DB), "SIGN_IN", pgtype.UUID{}, pgtype.UUID{}, session)
	return Ceremony{ID: id, Options: assertion.Response}, err
}

// FinishSignIn validates a passkey assertion and issues a donor session.
// A cloned-counter warning is treated as a compromised key and denied.
func (s *DonorPasskeyService) FinishSignIn(ctx context.Context, ceremony string, response []byte, client Client) (Tokens, error) {
	var tokens Tokens
	err := answerCeremony(ctx, s.DB, func(q *queries.Queries) error {
		now := time.Now().UTC()
		session, _, err := takeDonorCeremony(ctx, q, ceremony, "SIGN_IN", now)
		if err != nil {
			return err
		}
		assertion, err := protocol.ParseCredentialRequestResponseBytes(response)
		if err != nil {
			return ErrPasskeyDenied
		}
		ownerID, err := q.DonorPasskeyOwner(ctx, assertion.RawID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPasskeyDenied
		}
		if err != nil {
			return err
		}
		user, err := loadDonorUser(ctx, q, ownerID)
		if err != nil {
			return err
		}
		if user.donor.Disabled {
			return ErrPasskeyDenied
		}
		now = time.Now().UTC()
		_, passkey, err := s.WebAuthn.ValidatePasskeyLogin(
			func([]byte, []byte) (webauthn.User, error) { return user, nil },
			session.SessionData, assertion,
		)
		if err != nil || passkey.Authenticator.CloneWarning {
			return ErrPasskeyDenied
		}
		if err = q.RecordDonorPasskeyUse(ctx, queries.RecordDonorPasskeyUseParams{
			CredentialID: passkey.ID, SignCount: int64(passkey.Authenticator.SignCount),
			UserVerified: passkey.Flags.UserVerified, BackupState: passkey.Flags.BackupState,
			LastUsedAt: timestamp(now),
		}); err != nil {
			return err
		}
		tokens, err = issueSession(ctx, q, ownerID, client, now)
		return err
	})
	return tokens, err
}

// BeginRegister issues a passkey creation challenge for a signed-in donor.
// The donor must already be authenticated; this is not an anonymous route.
func (s *DonorPasskeyService) BeginRegister(ctx context.Context, token string) (Ceremony, error) {
	if err := s.sweepCeremonies(ctx); err != nil {
		return Ceremony{}, err
	}
	return transact(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) (Ceremony, error) {
		q := queries.New(tx)
		user, err := loadDonorUserByToken(ctx, q, token)
		if err != nil {
			return Ceremony{}, err
		}
		creation, session, err := s.WebAuthn.BeginRegistration(user,
			webauthn.WithExclusions(webauthn.Credentials(user.passkeys).CredentialDescriptors()))
		if err != nil {
			return Ceremony{}, err
		}
		id, err := saveDonorCeremony(ctx, q, "REGISTER", user.donor.ID, user.session, session)
		return Ceremony{ID: id, Options: creation.Response}, err
	})
}

// FinishRegister validates and stores a new passkey for a signed-in donor and
// queues the essential notice telling them so, returning every passkey the
// donor now holds.
func (s *DonorPasskeyService) FinishRegister(
	ctx context.Context,
	token, ceremony string,
	response []byte,
	name string,
) (PasskeyList, error) {
	if !ValidPasskeyName(name) {
		return PasskeyList{}, ErrPasskeyDenied
	}
	var list PasskeyList
	err := answerCeremony(ctx, s.DB, func(q *queries.Queries) error {
		now := time.Now().UTC()
		session, donorID, err := takeDonorCeremony(ctx, q, ceremony, "REGISTER", now)
		if err != nil {
			return err
		}
		attestation, err := protocol.ParseCredentialCreationResponseBytes(response)
		if err != nil {
			return ErrPasskeyDenied
		}
		user, err := loadDonorUserByToken(ctx, q, token)
		if err != nil {
			return err
		}
		if user.donor.ID != donorID || !session.SessionID.Valid || session.SessionID != user.session {
			return ErrPasskeyDenied
		}
		now = time.Now().UTC()
		passkey, err := s.WebAuthn.CreateCredential(user, session.SessionData, attestation)
		if err != nil {
			return ErrPasskeyDenied
		}
		switch _, err = q.DonorPasskeyOwner(ctx, passkey.ID); {
		case err == nil:
			return ErrPasskeyDenied
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		id, err := NewID()
		if err != nil {
			return err
		}
		transports := []string{}
		for _, t := range passkey.Transport {
			if slices.Contains(knownTransports, t) {
				transports = append(transports, string(t))
			}
		}
		var nameText pgtype.Text
		if name != "" {
			nameText = text(name)
		}
		if err = q.InsertDonorPasskey(ctx, queries.InsertDonorPasskeyParams{
			ID: id, DonorID: user.donor.ID, CredentialID: passkey.ID, PublicKey: passkey.PublicKey,
			SignCount: int64(passkey.Authenticator.SignCount), Transports: transports,
			Aaguid: passkey.Authenticator.AAGUID, AttestationFormat: passkey.AttestationFormat,
			UserVerified: passkey.Flags.UserVerified, BackupEligible: passkey.Flags.BackupEligible,
			BackupState: passkey.Flags.BackupState, Name: nameText,
		}); err != nil {
			return duplicateCredential(err)
		}
		// The notice carries its facts, so removing the passkey cannot withdraw it.
		if err = queueMessage(ctx, q, user.donor.ID, "PASSKEY_ADDED", "passkey-added:"+id.String(),
			map[string]string{"name": name, "added_at": iso(now)}, now); err != nil {
			return err
		}
		list, err = listPasskeys(ctx, q, user.donor.ID)
		return err
	})
	return list, err
}

// duplicateCredential reports a concurrent registration of the same credential
// id as a denial, like the ownership check that precedes the insert.
func duplicateCredential(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		return ErrPasskeyDenied
	}
	return err
}

func (s *DonorPasskeyService) ListPasskeys(ctx context.Context, token string) (PasskeyList, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, _ time.Time) (PasskeyList, error) {
		return listPasskeys(ctx, q, d.ID)
	})
}

// DeletePasskey removes one of the donor's passkeys and returns those left.
// A passkey the donor does not hold is not found, whoever holds it.
func (s *DonorPasskeyService) DeletePasskey(ctx context.Context, token string, id pgtype.UUID) (PasskeyList, error) {
	return donorWrite(ctx, s.DB, donorOwner(token), func(tx pgx.Tx, d queries.Donor, _ time.Time) (PasskeyList, error) {
		q := queries.New(tx)
		n, err := q.DeleteDonorPasskey(ctx, queries.DeleteDonorPasskeyParams{ID: id, DonorID: d.ID})
		if err != nil {
			return PasskeyList{}, err
		}
		if n == 0 {
			return PasskeyList{}, pgx.ErrNoRows
		}
		return listPasskeys(ctx, q, d.ID)
	})
}

func listPasskeys(ctx context.Context, q *queries.Queries, donorID pgtype.UUID) (PasskeyList, error) {
	rows, err := q.DonorPasskeys(ctx, donorID)
	if err != nil {
		return PasskeyList{}, err
	}
	out := PasskeyList{Passkeys: make([]PasskeyInfo, 0, len(rows))}
	for _, row := range rows {
		info := PasskeyInfo{ID: row.ID, CreatedAt: row.CreatedAt.Time}
		if row.Name.Valid {
			info.Name = &row.Name.String
		}
		if row.LastUsedAt.Valid {
			info.LastUsedAt = &row.LastUsedAt.Time
		}
		out.Passkeys = append(out.Passkeys, info)
	}
	return out, nil
}

func loadDonorUserByToken(ctx context.Context, q *queries.Queries, token string) (donorUser, error) {
	si, err := lockSignedIn(ctx, q, token)
	if err != nil {
		return donorUser{}, err
	}
	user, err := loadDonorUser(ctx, q, si.ID)
	user.session = si.session
	return user, err
}

func ValidPasskeyName(name string) bool {
	n := utf8.RuneCountInString(name)
	return n <= MaxPasskeyNameRunes && plainText(name, false)
}
