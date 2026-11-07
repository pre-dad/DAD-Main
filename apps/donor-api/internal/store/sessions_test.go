package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var app = Client{App: true, Device: "Test phone", Platform: "ANDROID"}

func claimSession(t *testing.T, pool *pgxpool.Pool, donor pgtype.UUID, client Client) Tokens {
	t.Helper()
	api := as(t, dbroles.API)
	tokens, err := (AccountService{DB: api}).ClaimAccount(context.Background(), link(t, pool, donor, time.Now()), "", client)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func signedInAs(t *testing.T, pool *pgxpool.Pool, token string) bool {
	t.Helper()
	_, err := authenticate(context.Background(), queries.New(pool), token, time.Now())
	if err != nil && !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	return err == nil
}

func sessionEvents(t *testing.T, pool *pgxpool.Pool, donor pgtype.UUID, action string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_session_events WHERE donor_id=$1 AND action=$2", donor, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func refresh(t *testing.T, token string) Tokens {
	t.Helper()
	api := as(t, dbroles.API)
	tokens, err := (AccountService{DB: api}).Refresh(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestAppSignInIssuesSeparateAccessAndRefreshTokens(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	tokens := claimSession(t, pool, id, Client{App: true, Device: " Alex’s\u202e  phone\a ", Platform: "IOS"})
	family, secret, ok := strings.Cut(tokens.Refresh, ".")
	if len(tokens.Access) != 43 || !ok || len(family) != 43 || len(secret) != 43 || tokens.AccessTTL != accessLifetime ||
		tokens.RefreshTTL != refreshIdle {
		t.Fatalf(
			"app tokens are not a 256-bit access token and family-bound refresh token with their lifetimes: %d %q %v %v",
			len(tokens.Access),
			tokens.Refresh,
			tokens.AccessTTL,
			tokens.RefreshTTL,
		)
	}
	if !signedInAs(t, pool, tokens.Access) || signedInAs(t, pool, tokens.Refresh) {
		t.Fatal("only the access token may authenticate requests")
	}
	if _, err := (AccountService{DB: api}).Refresh(context.Background(), tokens.Access); !errors.Is(err, ErrDenied) {
		t.Fatalf("access token refreshed: %v", err)
	}
	var client, device, platform string
	var stored int
	if err := pool.QueryRow(context.Background(), "SELECT client,device_name,platform FROM donor_sessions WHERE donor_id=$1", id).Scan(&client, &device, &platform); err != nil {
		t.Fatal(err)
	}
	if client != "APP" || device != "Alex’s phone" || platform != "IOS" {
		t.Fatalf("stored %s %q %s, want the app's sanitized description", client, device, platform)
	}
	secrets := []string{tokens.Access, family, tokens.Refresh}
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_sessions WHERE token_hash=ANY($1) OR refresh_family_hash=ANY($1) OR refresh_hash=ANY($1)", secrets).Scan(&stored); err != nil ||
		stored != 0 {
		t.Fatalf("a token is stored unhashed: %d %v", stored, err)
	}
	if sessionEvents(t, pool, id, "SIGNED_IN") != 1 {
		t.Fatal("sign-in not recorded")
	}
	browser := claimSession(t, pool, id, Client{Device: "ignored"})
	if browser.Refresh != "" || browser.AccessTTL != browserLifetime {
		t.Fatalf("browser session got %+v, want a 30-day cookie token and no refresh token", browser)
	}
}

func TestRefreshRotatesAndRetiresTheOldAccessToken(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	first := claimSession(t, pool, id, app)
	second := refresh(t, first.Refresh)
	if signedInAs(t, pool, first.Access) || !signedInAs(t, pool, second.Access) {
		t.Fatal("refresh must replace the access token at once")
	}
	family, _, _ := strings.Cut(first.Refresh, ".")
	if second.Refresh == first.Refresh || !strings.HasPrefix(second.Refresh, family+".") || second.AccessTTL != accessLifetime {
		t.Fatalf("rotation issued %+v", second)
	}
	third := refresh(t, second.Refresh)
	if !signedInAs(t, pool, third.Access) || sessionEvents(t, pool, id, "REFRESH_REUSED") != 0 {
		t.Fatal("a chain of rotations broke the session")
	}
}

func TestReusedRefreshTokenRevokesTheWholeSession(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	ctx := context.Background()
	stolen := claimSession(t, pool, id, app)
	rotated := refresh(t, stolen.Refresh)
	latest := refresh(t, rotated.Refresh)
	other := claimSession(t, pool, id, app)
	if _, err := (AccountService{DB: api}).Refresh(ctx, stolen.Refresh); !errors.Is(err, ErrDenied) {
		t.Fatalf("reused token: %v", err)
	}
	if signedInAs(t, pool, latest.Access) {
		t.Fatal("access descended from the reused token still works")
	}
	if _, err := (AccountService{DB: api}).Refresh(ctx, latest.Refresh); !errors.Is(err, ErrDenied) {
		t.Fatalf("refresh descended from the reused token still works: %v", err)
	}
	if !signedInAs(t, pool, other.Access) || sessionEvents(t, pool, id, "REFRESH_REUSED") != 1 {
		t.Fatal("reuse must revoke only its own session, recorded once")
	}
	forged := strings.SplitN(latest.Refresh, ".", 2)[0] + "." + strings.Repeat("x", 43)
	if _, err := (AccountService{DB: api}).Refresh(ctx, forged); !errors.Is(err, ErrDenied) ||
		sessionEvents(t, pool, id, "REFRESH_REUSED") != 1 {
		t.Fatalf("a revoked session answered or recorded again: %v", err)
	}
}

func TestRefreshGraceAnswersALostResponseOnce(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	ctx := context.Background()
	original := claimSession(t, pool, id, app)
	lost := refresh(t, original.Refresh)
	retried := refresh(t, original.Refresh)
	if signedInAs(t, pool, lost.Access) || !signedInAs(t, pool, retried.Access) {
		t.Fatal("the retry's answer must supersede the lost one")
	}
	if _, err := (AccountService{DB: api}).Refresh(ctx, lost.Refresh); !errors.Is(err, ErrDenied) || signedInAs(t, pool, retried.Access) {
		t.Fatalf("a superseded token must count as reuse: %v", err)
	}

	original = claimSession(t, pool, id, app)
	next := refresh(t, original.Refresh)
	if _, err := pool.Exec(ctx, "UPDATE donor_sessions SET refreshed_at=refreshed_at-$2::interval WHERE token_hash=$1", HashToken(next.Access), (refreshGrace + time.Second).String()); err != nil {
		t.Fatal(err)
	}
	if _, err := (AccountService{DB: api}).Refresh(ctx, original.Refresh); !errors.Is(err, ErrDenied) || signedInAs(t, pool, next.Access) {
		t.Fatalf("the previous token after the grace must count as reuse: %v", err)
	}
	if sessionEvents(t, pool, id, "REFRESH_REUSED") != 2 {
		t.Fatal("reuse not recorded")
	}
}

// Concurrent refreshes with one token are serialized by the session lock. The
// second is inside the grace, so both succeed and the later answer wins; the
// earlier refresh token is then stale.
func TestConcurrentRefreshesLetTheLaterAnswerWin(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	original := claimSession(t, pool, id, app)
	results := make(chan Tokens, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			tokens, err := (AccountService{DB: api}).Refresh(context.Background(), original.Refresh)
			if err != nil {
				t.Error(err)
			}
			results <- tokens
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winners, losers []Tokens
	for tokens := range results {
		if signedInAs(t, pool, tokens.Access) {
			winners = append(winners, tokens)
		} else {
			losers = append(losers, tokens)
		}
	}
	if len(winners) != 1 || len(losers) != 1 {
		t.Fatalf("got %d working and %d superseded answers, want one of each", len(winners), len(losers))
	}
	if _, err := (AccountService{DB: api}).Refresh(context.Background(), losers[0].Refresh); !errors.Is(err, ErrDenied) ||
		signedInAs(t, pool, winners[0].Access) {
		t.Fatalf("the superseded answer must count as reuse: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	ctx := context.Background()
	expire := func(column, token string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE donor_sessions SET "+column+"=now()-interval '1 second' WHERE token_hash=$1", HashToken(token)); err != nil {
			t.Fatal(err)
		}
	}
	tokens := claimSession(t, pool, id, app)
	expire("expires_at", tokens.Access)
	if signedInAs(t, pool, tokens.Access) {
		t.Fatal("expired access token accepted")
	}
	renewed := refresh(t, tokens.Refresh)
	if !signedInAs(t, pool, renewed.Access) {
		t.Fatal("refresh after access expiry failed")
	}
	expire("refresh_expires_at", renewed.Access)
	if _, err := (AccountService{DB: api}).Refresh(ctx, renewed.Refresh); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired refresh token accepted: %v", err)
	}
	if sessionEvents(t, pool, id, "REFRESH_REUSED") != 0 {
		t.Fatal("expiry recorded as reuse")
	}

	tokens = claimSession(t, pool, id, app)
	if _, err := pool.Exec(ctx, "UPDATE donor_sessions SET created_at=now()-$2::interval WHERE token_hash=$1", HashToken(tokens.Access), (refreshLimit - 5*time.Minute).String()); err != nil {
		t.Fatal(err)
	}
	capped := refresh(t, tokens.Refresh)
	if capped.RefreshTTL > 5*time.Minute || capped.AccessTTL > capped.RefreshTTL {
		t.Fatalf("refresh extended past the sign-in limit: %+v", capped)
	}
	if _, err := pool.Exec(ctx, "UPDATE donors SET disabled=true WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := (AccountService{DB: api}).Refresh(ctx, capped.Refresh); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled donor refreshed: %v", err)
	}
}

func TestDonorListsAndSignsOutSessions(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	ctx := context.Background()
	service := DonorService{DB: api}
	browser := claimSession(t, pool, id, Client{})
	phone := claimSession(t, pool, id, app)
	tablet := claimSession(t, pool, id, Client{App: true, Platform: "IOS"})
	ended := claimSession(t, pool, id, Client{})
	if err := (AccountService{DB: api}).SignOut(ctx, ended.Access); err != nil {
		t.Fatal(err)
	}

	list, err := service.Sessions(ctx, browser.Access)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, session := range list.Sessions {
		labels = append(labels, session.Client+" "+session.Label)
	}
	if len(list.Sessions) != 3 || !list.Sessions[0].Current || list.Sessions[1].Current || list.Sessions[2].Current ||
		!strings.Contains(
			strings.Join(labels, ","),
			"APP Test phone",
		) || !strings.Contains(strings.Join(labels, ","), "APP iOS device") || labels[0] != "BROWSER Web browser" {
		t.Fatalf("listed %v, want this browser first and both apps", labels)
	}
	phoneID := list.Sessions[1].ID
	if list.Sessions[1].Label != "Test phone" {
		phoneID = list.Sessions[2].ID
	}

	if _, err = service.SignOutSession(ctx, browser.Access, list.Sessions[0].ID); !errors.Is(err, ErrCurrentSession) {
		t.Fatalf("signed out the current session by ID: %v", err)
	}
	stranger, _ := donor(t, pool)
	strangerSession := claimSession(t, pool, stranger, Client{})
	theirs, err := service.Sessions(ctx, strangerSession.Access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.SignOutSession(ctx, browser.Access, theirs.Sessions[0].ID); !errors.Is(err, pgx.ErrNoRows) ||
		!signedInAs(t, pool, strangerSession.Access) {
		t.Fatalf("signed out another donor's session: %v", err)
	}

	after, err := service.SignOutSession(ctx, browser.Access, phoneID)
	if err != nil || len(after.Sessions) != 2 {
		t.Fatalf("sign out one: %v %+v", err, after)
	}
	if signedInAs(t, pool, phone.Access) {
		t.Fatal("signed-out session's access token still works")
	}
	if _, err = (AccountService{DB: api}).Refresh(ctx, phone.Refresh); !errors.Is(err, ErrDenied) {
		t.Fatalf("signed-out session refreshed: %v", err)
	}
	if _, err = service.SignOutSession(ctx, browser.Access, phoneID); err != nil || sessionEvents(t, pool, id, "SIGNED_OUT_SESSION") != 1 {
		t.Fatalf("signing out an ended session again: %v", err)
	}

	others, err := service.SignOutOthers(ctx, tablet.Access)
	if err != nil || len(others.Sessions) != 1 || !others.Sessions[0].Current || others.Sessions[0].Label != "iOS device" {
		t.Fatalf("sign out others: %v %+v", err, others)
	}
	if signedInAs(t, pool, browser.Access) || !signedInAs(t, pool, tablet.Access) || sessionEvents(t, pool, id, "SIGNED_OUT_OTHERS") != 1 {
		t.Fatal("sign out others must end every other session at once")
	}
	if err = service.SignOutEverywhere(ctx, tablet.Access); err != nil || signedInAs(t, pool, tablet.Access) ||
		sessionEvents(t, pool, id, "SIGNED_OUT_EVERYWHERE") != 1 {
		t.Fatalf("sign out everywhere: %v", err)
	}
}

func TestSessionUseIsRecordedAtMostEveryInterval(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	ctx := context.Background()
	tokens := claimSession(t, pool, id, Client{})
	lastUsed := func(age time.Duration) time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, "UPDATE donor_sessions SET last_used_at=now()-$2::interval WHERE token_hash=$1 RETURNING last_used_at", HashToken(tokens.Access), age.String()).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	stored := func() time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, "SELECT last_used_at FROM donor_sessions WHERE token_hash=$1", HashToken(tokens.Access)).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	service := DonorService{DB: api}
	for _, use := range []struct {
		name string
		call func() error
	}{
		{"read", func() error { _, err := service.Sessions(ctx, tokens.Access); return err }},
		{"write", func() error { _, err := service.SignOutOthers(ctx, tokens.Access); return err }},
	} {
		recent := lastUsed(touchInterval - time.Minute)
		if err := use.call(); err != nil {
			t.Fatal(err)
		}
		if !stored().Equal(recent) {
			t.Fatalf("%s within the interval wrote last use", use.name)
		}
		lastUsed(touchInterval + time.Minute)
		if err := use.call(); err != nil {
			t.Fatal(err)
		}
		if time.Since(stored()) > time.Minute {
			t.Fatalf("%s after the interval did not record use", use.name)
		}
	}
}

func TestDeviceNameIsPlainBoundedText(t *testing.T) {
	long := strings.Repeat("é", MaxDeviceName) + "tail"
	for _, tc := range []struct{ in, want string }{
		{"  Alex’s   iPhone ", "Alex’s iPhone"},
		{"Pixel\u202e\u200b 8\x00\n", "Pixel 8"},
		{"\u2066\u2069\t", ""},
		{long, strings.Repeat("é", MaxDeviceName)},
	} {
		if got := deviceName(tc.in); got.String != tc.want || got.Valid != (tc.want != "") {
			t.Errorf("deviceName(%q) = %+v, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDisabledDonorAndRevokedSessionsAreDeniedAtOnce(t *testing.T) {
	pool := database(t)
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	revoked := claimSession(t, pool, id, app)
	live := claimSession(t, pool, id, Client{})
	if _, err := pool.Exec(context.Background(), "UPDATE donor_sessions SET revoked_at=now() WHERE token_hash=$1", HashToken(revoked.Access)); err != nil {
		t.Fatal(err)
	}
	if signedInAs(t, pool, revoked.Access) || !signedInAs(t, pool, live.Access) {
		t.Fatal("a revoked session must be denied without affecting the donor's others")
	}
	if _, err := pool.Exec(context.Background(), "UPDATE donors SET disabled=true WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if signedInAs(t, pool, live.Access) {
		t.Fatal("a disabled donor's unexpired session still authenticates")
	}
}
