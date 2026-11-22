package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/httpapi"
	"dad/donor-api/internal/store"
)

// TestAppSessionLifecycle runs the donor app's sign-in against real session
// storage: a claimed session's access token reads and writes, its refresh
// token rotates, a reused refresh token or a sign-out from another session
// ends it at once along with its push registration, and it ends when it
// expires.
func TestAppSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || dsn == "" ||
		(config.ConnConfig.Database != "donor_go" && !strings.HasPrefix(config.ConnConfig.Database, "donor_go_")) {
		t.Fatal("TEST_DATABASE_URL must name a migrated donor_go test database")
	}
	pool, err := store.Open(ctx, dsn, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	email := "app-session-" + id.String() + "@example.org"
	if _, err = pool.Exec(ctx, "INSERT INTO donors(id,email) VALUES($1,$2)", id, email); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteDonorRows(t, pool, id) })

	const origin = "https://donor.example.org"
	api := dbrolestest.Open(t, dbroles.API)
	server := httpapi.NewServer(
		"",
		httpapi.Services{
			Origin:  origin,
			Account: store.AccountService{DB: api},
			Donor:   store.DonorService{DB: api, Origin: origin},
			Push:    store.PushService{DB: api, Platforms: []string{"IOS"}},
		},
	)
	call := func(method, path, body, session string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-DAD-Client", "native")
		if session != "" {
			r.Header.Set("Authorization", "Bearer "+session)
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response
	}
	type tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	issued := func(step string, response *httptest.ResponseRecorder) tokens {
		t.Helper()
		var body tokens
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK || body.Access == "" ||
			body.Refresh == "" ||
			len(response.Result().Cookies()) != 0 {
			t.Fatalf("%s: got %d %s, want 200 with tokens and no cookie", step, response.Code, response.Body)
		}
		return body
	}
	signIn := func(device string) tokens {
		t.Helper()
		linkID, err := store.NewID()
		if err != nil {
			t.Fatal(err)
		}
		proof := strings.Repeat("p", 11) + linkID.String()
		if _, err = pool.Exec(ctx, "INSERT INTO donor_login_links(id,donor_id,token_hash,expires_at) VALUES($1,$2,$3,$4)", linkID, id, store.HashToken(proof), time.Now().Add(15*time.Minute)); err != nil {
			t.Fatal(err)
		}
		return issued("app claim", call("POST", "/donor/claim", `{"token":"`+proof+`","device_name":"`+device+`","platform":"IOS"}`, ""))
	}
	refresh := func(token string) *httptest.ResponseRecorder {
		return call("POST", "/donor/refresh", `{"refresh_token":"`+token+`"}`, "")
	}
	expect := func(step string, response *httptest.ResponseRecorder, status int) {
		t.Helper()
		if response.Code != status {
			t.Fatalf("%s: got %d %s, want %d", step, response.Code, response.Body, status)
		}
	}
	sessions := func(access string) []store.DeviceSession {
		t.Helper()
		response := call("GET", "/donor/me/sessions", "", access)
		var body store.Sessions
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
			t.Fatalf("list sessions: got %d %s", response.Code, response.Body)
		}
		return body.Sessions
	}

	session := signIn("Alex phone")
	profile := call("GET", "/donor/me/profile", "", session.Access)
	expect("read with bearer", profile, http.StatusOK)
	if !strings.Contains(profile.Body.String(), email) {
		t.Fatalf("profile is not the claimed donor's: %s", profile.Body)
	}
	expect(
		"write with bearer",
		call(
			"POST",
			"/donor/me/preferences",
			`{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":true}`,
			session.Access,
		),
		http.StatusOK,
	)
	expect("refresh token as bearer", call("GET", "/donor/me/profile", "", session.Refresh), http.StatusUnauthorized)
	expect("access token as refresh token", refresh(session.Access), http.StatusUnauthorized)
	browserRefresh := httptest.NewRequest("POST", "/donor/refresh", strings.NewReader(`{"refresh_token":"`+session.Refresh+`"}`))
	browserRefresh.Header.Set("Origin", origin)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, browserRefresh)
	expect("browser refresh", response, http.StatusForbidden)

	first := session
	session = issued("refresh", refresh(session.Refresh))
	expect("read with the replaced access token", call("GET", "/donor/me/profile", "", first.Access), http.StatusUnauthorized)
	expect("read with the new access token", call("GET", "/donor/me/profile", "", session.Access), http.StatusOK)
	session = issued("second refresh", refresh(session.Refresh))
	expect("reuse an exchanged refresh token", refresh(first.Refresh), http.StatusUnauthorized)
	expect("read after reuse", call("GET", "/donor/me/profile", "", session.Access), http.StatusUnauthorized)
	expect("refresh after reuse", refresh(session.Refresh), http.StatusUnauthorized)

	session, other := signIn("Alex phone"), signIn("Alex tablet")
	listed := sessions(session.Access)
	if len(listed) != 2 || !listed[0].Current || listed[0].Label != "Alex phone" || listed[1].Current || listed[1].Label != "Alex tablet" ||
		listed[1].Client != "APP" {
		t.Fatalf("listed %+v, want this phone first and the tablet", listed)
	}
	expect(
		"sign out this session by ID",
		call("POST", "/donor/me/sessions/"+listed[0].ID.String()+"/revoke", "", session.Access),
		http.StatusConflict,
	)
	expect(
		"register the tablet for notifications",
		call(
			"POST",
			"/donor/me/push-device",
			`{"platform":"IOS","token":"`+strings.Repeat("0f", 32)+`","environment":"SANDBOX"}`,
			other.Access,
		),
		http.StatusOK,
	)
	expect("sign out the tablet", call("POST", "/donor/me/sessions/"+listed[1].ID.String()+"/revoke", "", session.Access), http.StatusOK)
	var registered int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_sessions WHERE donor_id=$1 AND push_token IS NOT NULL", id).Scan(&registered); err != nil ||
		registered != 0 {
		t.Fatalf("the signed-out tablet kept its push token: %d %v", registered, err)
	}
	expect("tablet read after sign-out", call("GET", "/donor/me/profile", "", other.Access), http.StatusUnauthorized)
	expect("tablet refresh after sign-out", refresh(other.Refresh), http.StatusUnauthorized)

	other = signIn("Alex tablet")
	expect("sign out other sessions", call("POST", "/donor/me/sessions/revoke-others", "", session.Access), http.StatusOK)
	expect("other session after sign-out of others", call("GET", "/donor/me/profile", "", other.Access), http.StatusUnauthorized)
	expect("this session after sign-out of others", call("GET", "/donor/me/profile", "", session.Access), http.StatusOK)
	expect("sign out", call("POST", "/donor/logout", "", session.Access), http.StatusOK)
	expect("read after sign-out", call("GET", "/donor/me/profile", "", session.Access), http.StatusUnauthorized)
	expect("refresh after sign-out", refresh(session.Refresh), http.StatusUnauthorized)

	session = signIn("Alex phone")
	browser := func(method, path, body, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: cookie})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response
	}
	linkID, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	proof := strings.Repeat("b", 11) + linkID.String()
	if _, err = pool.Exec(ctx, "INSERT INTO donor_login_links(id,donor_id,token_hash,expires_at) VALUES($1,$2,$3,$4)", linkID, id, store.HashToken(proof), time.Now().Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed := browser("POST", "/donor/claim", `{"token":"`+proof+`"}`, "")
	expect("browser claim", claimed, http.StatusOK)
	cookie := claimed.Result().Cookies()[0].Value
	var fromBrowser store.Sessions
	listing := browser("GET", "/donor/me/sessions", "", cookie)
	if err = json.Unmarshal(listing.Body.Bytes(), &fromBrowser); err != nil || len(fromBrowser.Sessions) != 2 ||
		fromBrowser.Sessions[0].Label != "Web browser" ||
		!fromBrowser.Sessions[0].Current {
		t.Fatalf("browser listing: got %d %s, want this browser first and the phone", listing.Code, listing.Body)
	}
	expect(
		"browser signs out the phone",
		browser("POST", "/donor/me/sessions/"+fromBrowser.Sessions[1].ID.String()+"/revoke", "", cookie),
		http.StatusOK,
	)
	expect("phone after the browser signed it out", call("GET", "/donor/me/profile", "", session.Access), http.StatusUnauthorized)
	expect("browser after signing out the phone", browser("GET", "/donor/me/profile", "", cookie), http.StatusOK)

	session, other = signIn("Alex phone"), signIn("Alex tablet")
	expect("sign out everywhere", call("POST", "/donor/logout-all", "", session.Access), http.StatusOK)
	expect("other session after sign-out everywhere", call("GET", "/donor/me/profile", "", other.Access), http.StatusUnauthorized)

	session = signIn("Alex phone")
	if _, err = pool.Exec(ctx, "UPDATE donor_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1", store.HashToken(session.Access)); err != nil {
		t.Fatal(err)
	}
	expect("read with expired access token", call("GET", "/donor/me/profile", "", session.Access), http.StatusUnauthorized)
	session = issued("refresh after access expiry", refresh(session.Refresh))
	if _, err = pool.Exec(ctx, "UPDATE donor_sessions SET expires_at=now()-interval '1 second',refresh_expires_at=now()-interval '1 second' WHERE token_hash=$1", store.HashToken(session.Access)); err != nil {
		t.Fatal(err)
	}
	expect("refresh after the session expired", refresh(session.Refresh), http.StatusUnauthorized)
	if directory := os.Getenv("DONOR_NATIVE_ACCEPTANCE_DIR"); directory != "" {
		serveNativeClients(t, directory, pool, api)
	}
}
