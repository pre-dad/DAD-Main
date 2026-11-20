package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session lifetimes. Every request checks its session in the database, so
// revocation is immediate whatever the access lifetime; the app's access
// token is short-lived only to bound the use of one that leaks from a log,
// proxy, or crash report. Its refresh token lasts while the app is used and
// expires after refreshIdle without a refresh, or refreshLimit after sign-in,
// when the donor proves their email again.
const (
	browserLifetime = 30 * 24 * time.Hour
	accessLifetime  = 15 * time.Minute
	refreshIdle     = 60 * 24 * time.Hour
	refreshLimit    = 365 * 24 * time.Hour
	// refreshGrace lets the app retry a refresh whose answer it lost to a
	// dropped connection, within one client timeout, instead of revoking
	// the session as a reused token.
	refreshGrace  = time.Minute
	touchInterval = 5 * time.Minute
)

// MaxDeviceName bounds the app's name for its device, in characters.
const MaxDeviceName = 64

// ErrCurrentSession refuses to sign out the caller's own session by ID; the
// caller signs out instead, which also clears a browser's cookie.
var ErrCurrentSession = errors.New("use sign-out for the current session")

// Client is what a session is issued to: a browser, or the donor app with the
// device name and platform it reports.
type Client struct {
	App      bool
	Device   string
	Platform string // IOS, ANDROID, or empty
	// CompanyToken is a company touch from the browser completing sign-in.
	CompanyToken string
	JourneyID    string
}

// Tokens are a new session's credentials and how long each lasts. A browser
// session has only Access, kept as its cookie for the whole session.
type Tokens struct {
	Access     string
	AccessTTL  time.Duration
	Refresh    string
	RefreshTTL time.Duration
}

// DeviceSession is one place the donor is signed in.
type DeviceSession struct {
	ID         pgtype.UUID `json:"id"`
	Client     string      `json:"client"`
	Platform   *string     `json:"platform"`
	Label      string      `json:"label"`
	CreatedAt  time.Time   `json:"created_at"`
	LastUsedAt time.Time   `json:"last_used_at"`
	Current    bool        `json:"current"`
}

// Sessions is the donor's signed-in devices.
type Sessions struct {
	Sessions []DeviceSession `json:"sessions"`
}

// issueSession signs donor in on client. The app's refresh token is
// "<family>.<secret>"; only hashes of either part are stored.
func issueSession(ctx context.Context, q *queries.Queries, donor pgtype.UUID, client Client, now time.Time) (Tokens, error) {
	id, err := NewID()
	if err != nil {
		return Tokens{}, err
	}
	access, err := randomToken(32)
	if err != nil {
		return Tokens{}, err
	}
	session := queries.InsertSessionParams{
		ID:        id,
		DonorID:   donor,
		TokenHash: HashToken(access),
		ExpiresAt: timestamp(now.Add(browserLifetime)),
		Client:    "BROWSER",
	}
	tokens := Tokens{Access: access, AccessTTL: browserLifetime}
	if client.App {
		family, err := randomToken(32)
		if err != nil {
			return Tokens{}, err
		}
		refresh, err := refreshToken(family)
		if err != nil {
			return Tokens{}, err
		}
		session.Client, session.DeviceName, session.ExpiresAt = "APP", deviceName(client.Device), timestamp(now.Add(accessLifetime))
		if client.Platform != "" {
			session.Platform = text(client.Platform)
		}
		session.RefreshFamilyHash, session.RefreshHash = text(HashToken(family)), text(HashToken(refresh))
		session.RefreshExpiresAt = timestamp(now.Add(refreshIdle))
		tokens = Tokens{Access: access, AccessTTL: accessLifetime, Refresh: refresh, RefreshTTL: refreshIdle}
	}
	if err = q.InsertSession(ctx, session); err != nil {
		return Tokens{}, err
	}
	if _, err = bindCompanyCode(ctx, q, donor, client.CompanyToken, now); err != nil {
		return Tokens{}, err
	}
	var journey pgtype.UUID
	if journey.Scan(client.JourneyID) == nil && journey.Valid {
		if err = q.BindAnalyticsJourney(ctx, queries.BindAnalyticsJourneyParams{JourneyID: journey, DonorID: donor}); err != nil {
			return Tokens{}, err
		}
	}
	return tokens, sessionEvent(ctx, q, donor, id, "SIGNED_IN")
}

func refreshToken(family string) (string, error) {
	secret, err := randomToken(32)
	return family + "." + secret, err
}

// deviceName stores the app's name for its device as plain text: control and
// format characters, which could hide or reorder what the donor reads, are
// dropped and runs of space collapsed.
func deviceName(name string) pgtype.Text {
	name = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, name)), " ")
	if runes := []rune(name); len(runes) > MaxDeviceName {
		name = strings.TrimSpace(string(runes[:MaxDeviceName]))
	}
	if name == "" {
		return pgtype.Text{}
	}
	return text(name)
}

// Refresh exchanges an app session's current refresh token for new tokens,
// replacing its access token. Presenting a token the session has already
// exchanged revokes the session, because either the app or someone who copied
// the token is using a stale one; the exception is the token exchanged last,
// within refreshGrace, which is answered with yet another rotation so that
// whichever answer arrives last is the one that works.
func (s AccountService) Refresh(ctx context.Context, token string) (Tokens, error) {
	family, _, ok := strings.Cut(token, ".")
	if !ok {
		return Tokens{}, ErrDenied
	}
	var tokens Tokens
	reused := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		row, err := q.RefreshingSession(ctx, text(HashToken(family)))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDenied
		}
		if err != nil {
			return err
		}
		clock, err := q.DatabaseNow(ctx)
		if err != nil {
			return err
		}
		session, now := row.DonorSession, clock.Time
		if row.Disabled || session.RevokedAt.Valid || !session.RefreshExpiresAt.Time.After(now) {
			return ErrDenied
		}
		previous, refreshedAt := session.RefreshPreviousHash, session.RefreshedAt
		switch presented := HashToken(token); {
		case presented == session.RefreshHash.String:
			previous, refreshedAt = session.RefreshHash, timestamp(now)
		case presented == previous.String && now.Before(refreshedAt.Time.Add(refreshGrace)):
		default:
			reused = true
			if _, err = q.RevokeSessionByID(ctx, queries.RevokeSessionByIDParams{ID: session.ID, RevokedAt: timestamp(now)}); err != nil {
				return err
			}
			return sessionEvent(ctx, q, session.DonorID, session.ID, "REFRESH_REUSED")
		}
		tokens, err = rotate(ctx, q, session, family, previous, refreshedAt, now)
		return err
	})
	if err == nil && reused {
		err = ErrDenied
	}
	return tokens, err
}

// rotate issues session new tokens in family, extending its refresh lifetime
// up to refreshLimit after sign-in.
func rotate(
	ctx context.Context,
	q *queries.Queries,
	session queries.DonorSession,
	family string,
	previous pgtype.Text,
	refreshedAt pgtype.Timestamptz,
	now time.Time,
) (Tokens, error) {
	access, err := randomToken(32)
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := refreshToken(family)
	if err != nil {
		return Tokens{}, err
	}
	expires, accessExpires := now.Add(refreshIdle), now.Add(accessLifetime)
	if limit := session.CreatedAt.Time.Add(refreshLimit); limit.Before(expires) {
		expires = limit
	}
	if expires.Before(accessExpires) {
		accessExpires = expires
	}
	err = q.RotateSession(ctx, queries.RotateSessionParams{
		ID: session.ID, TokenHash: HashToken(access), ExpiresAt: timestamp(accessExpires),
		RefreshHash: text(HashToken(refresh)), RefreshPreviousHash: previous, RefreshedAt: refreshedAt,
		RefreshExpiresAt: timestamp(expires), LastUsedAt: timestamp(now),
	})
	return Tokens{Access: access, AccessTTL: accessExpires.Sub(now), Refresh: refresh, RefreshTTL: expires.Sub(now)}, err
}

func sessionEvent(ctx context.Context, q *queries.Queries, donor, session pgtype.UUID, action string) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return q.RecordSessionEvent(ctx, queries.RecordSessionEventParams{ID: id, DonorID: donor, SessionID: session, Action: action})
}

// Sessions lists where the donor signed in with token is signed in, this
// session first.
func (s DonorService) Sessions(ctx context.Context, token string) (Sessions, error) {
	return donorRead(ctx, s.DB, token, func(q *queries.Queries, d queries.Donor, now time.Time) (Sessions, error) {
		return listSessions(ctx, q, d.ID, token, now)
	})
}

// SignOutSession revokes another of the donor's sessions by ID. A session
// that already ended is signed out already; an ID that is not the donor's
// is not found.
func (s DonorService) SignOutSession(ctx context.Context, token string, id pgtype.UUID) (Sessions, error) {
	return sessionWrite(ctx, s.DB, token, func(q *queries.Queries, user signedIn, now time.Time) (Sessions, error) {
		credential, err := q.DonorSessionCredential(ctx, queries.DonorSessionCredentialParams{ID: id, DonorID: user.ID})
		if err != nil {
			return Sessions{}, err
		}
		if credential == HashToken(token) {
			return Sessions{}, ErrCurrentSession
		}
		revoked, err := q.RevokeSessionByID(ctx, queries.RevokeSessionByIDParams{ID: id, RevokedAt: timestamp(now)})
		if err != nil {
			return Sessions{}, err
		}
		if revoked > 0 {
			if err = sessionEvent(ctx, q, user.ID, id, "SIGNED_OUT_SESSION"); err != nil {
				return Sessions{}, err
			}
		}
		return listSessions(ctx, q, user.ID, token, now)
	})
}

// SignOutOthers revokes every session of the donor but the caller's.
func (s DonorService) SignOutOthers(ctx context.Context, token string) (Sessions, error) {
	return sessionWrite(ctx, s.DB, token, func(q *queries.Queries, user signedIn, now time.Time) (Sessions, error) {
		if err := q.RevokeOtherSessions(ctx, queries.RevokeOtherSessionsParams{DonorID: user.ID, TokenHash: HashToken(token), RevokedAt: timestamp(now)}); err != nil {
			return Sessions{}, err
		}
		if err := sessionEvent(ctx, q, user.ID, user.session, "SIGNED_OUT_OTHERS"); err != nil {
			return Sessions{}, err
		}
		return listSessions(ctx, q, user.ID, token, now)
	})
}

// SignOutEverywhere revokes every session of the donor, the caller's included.
func (s DonorService) SignOutEverywhere(ctx context.Context, token string) error {
	_, err := sessionWrite(ctx, s.DB, token, func(q *queries.Queries, user signedIn, now time.Time) (struct{}, error) {
		if err := q.RevokeAllSessions(ctx, queries.RevokeAllSessionsParams{DonorID: user.ID, RevokedAt: timestamp(now)}); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, sessionEvent(ctx, q, user.ID, user.session, "SIGNED_OUT_EVERYWHERE")
	})
	return err
}

// sessionWrite is donorWrite for commands on the donor's sessions, which need
// to know which session is the caller's.
func sessionWrite[T any](
	ctx context.Context,
	db *pgxpool.Pool,
	token string,
	fn func(*queries.Queries, signedIn, time.Time) (T, error),
) (T, error) {
	var user signedIn
	owner := func(ctx context.Context, tx pgx.Tx) (queries.Donor, error) {
		var err error
		user, err = lockSignedIn(ctx, queries.New(tx), token)
		return user.Donor, err
	}
	return donorWrite(ctx, db, owner, func(tx pgx.Tx, _ queries.Donor, now time.Time) (T, error) {
		return fn(queries.New(tx), user, now)
	})
}

func listSessions(ctx context.Context, q *queries.Queries, donor pgtype.UUID, token string, now time.Time) (Sessions, error) {
	rows, err := q.DonorSessions(ctx, queries.DonorSessionsParams{DonorID: donor, CurrentHash: HashToken(token), AsOf: timestamp(now)})
	if err != nil {
		return Sessions{}, err
	}
	out := Sessions{Sessions: make([]DeviceSession, 0, len(rows))}
	for _, row := range rows {
		out.Sessions = append(out.Sessions, DeviceSession{
			ID: row.ID, Client: row.Client, Platform: textPointer(row.Platform), Label: sessionLabel(row),
			CreatedAt: row.CreatedAt.Time, LastUsedAt: row.LastUsedAt.Time, Current: row.Current,
		})
	}
	return out, nil
}

// sessionLabel names a session as its donor would recognize it. Browsers are
// not fingerprinted, so every browser is a web browser.
func sessionLabel(row queries.DonorSessionsRow) string {
	switch {
	case row.Client == "BROWSER":
		return "Web browser"
	case row.DeviceName.Valid:
		return row.DeviceName.String
	case row.Platform.String == "IOS":
		return "iOS device"
	case row.Platform.String == "ANDROID":
		return "Android device"
	}
	return "Mobile device"
}
