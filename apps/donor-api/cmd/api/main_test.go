package main

import (
	"cmp"
	"context"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/dbroles/dbrolestest"
	"dad/donor-api/internal/oidc"
	"dad/donor-api/internal/sms"
)

func TestTrustedProxiesAcceptsProxyNetworksAndRefusesOpenRanges(t *testing.T) {
	got, err := trustedProxies(" 172.30.42.10/32, ,10.0.0.0/8,fd00::/48")
	want := []netip.Prefix{
		netip.MustParsePrefix("172.30.42.10/32"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("fd00::/48"),
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, invalid := range []string{"0.0.0.0/0", "10.0.0.0/7", "::/0", "2001::/16", "not-a-cidr", "10.0.0.1"} {
		if _, err := trustedProxies(invalid); err == nil {
			t.Errorf("%q accepted", invalid)
		}
	}
}

func TestConfigureRejectsInvalidEnvironmentBeforeTheDatabase(t *testing.T) {
	// The database URL is unreachable: each case must fail on configuration first.
	base := map[string]string{"DONOR_API_DATABASE_URL": "postgresql://unreachable.invalid/donor"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"proxy range", map[string]string{"DONOR_TRUSTED_PROXY_CIDRS": "0.0.0.0/0"}, "too broad"},
		{"plain HTTP origin", map[string]string{"DONOR_PUBLIC_ORIGIN": "http://dad.example.org"}, "DONOR_PUBLIC_ORIGIN must be an HTTPS"},
		{"origin with path", map[string]string{"DONOR_PUBLIC_ORIGIN": "https://dad.example.org/app"}, "DONOR_PUBLIC_ORIGIN must be an HTTPS"},
		{"half of Stripe", map[string]string{"STRIPE_SECRET_KEY": "sk_test_123"}, "set both STRIPE_SECRET_KEY"},
		{"Stripe ceiling", map[string]string{"DONOR_STRIPE_REQUESTS_PER_SECOND": "0"}, "DONOR_STRIPE_REQUESTS_PER_SECOND"},
		{"retired staff key without a staff origin", map[string]string{"DONOR_STAFF_ENCRYPTION_KEY": "MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA="}, "set DONOR_STAFF_ORIGIN"},
		{"plain HTTP staff origin", map[string]string{"DONOR_STAFF_ORIGIN": "http://staff.dad.example.org"}, "DONOR_STAFF_ORIGIN must be an HTTPS"},
		{"staff origin with path", map[string]string{"DONOR_STAFF_ORIGIN": "https://staff.dad.example.org/staff"}, "DONOR_STAFF_ORIGIN must be an HTTPS"},
		{"public origin in capitals", map[string]string{"DONOR_PUBLIC_ORIGIN": "https://DAD.example.org"}, "DONOR_PUBLIC_ORIGIN must be written as browsers send it"},
		{"public origin with its default port", map[string]string{"DONOR_PUBLIC_ORIGIN": "https://dad.example.org:443"}, "DONOR_PUBLIC_ORIGIN must be written as browsers send it"},
		{"staff origin in capitals", map[string]string{"DONOR_STAFF_ORIGIN": "https://Staff.dad.example.org"}, "DONOR_STAFF_ORIGIN must be written as browsers send it"},
		{"staff origin with its default port", map[string]string{"DONOR_STAFF_ORIGIN": "https://staff.dad.example.org:443"}, "DONOR_STAFF_ORIGIN must be written as browsers send it"},
		{"staff origin with a trailing slash", map[string]string{"DONOR_STAFF_ORIGIN": "https://staff.dad.example.org/"}, "DONOR_STAFF_ORIGIN must be an HTTPS"},
		{"staff on the public host", map[string]string{"DONOR_PUBLIC_ORIGIN": "https://dad.example.org", "DONOR_STAFF_ORIGIN": "https://dad.example.org:8443"}, "must use its own host"},
		{"staff on a parent of the public host", map[string]string{"DONOR_PUBLIC_ORIGIN": "https://dad.example.org", "DONOR_STAFF_ORIGIN": "https://example.org"}, "must use its own host"},
		{"local staff on the public host", map[string]string{"DONOR_STAFF_ORIGIN": "http://localhost:3001"}, "must use its own host"},
		{"content key", map[string]string{"DONOR_CONTENT_ENCRYPTION_KEY": "short"}, "DONOR_CONTENT_ENCRYPTION_KEY key 1 of 1"},
		{"retired content key", map[string]string{"DONOR_CONTENT_ENCRYPTION_KEY": "MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=,short"}, "DONOR_CONTENT_ENCRYPTION_KEY key 2 of 2"},
		{"short event check-in secret", map[string]string{"DONOR_EVENT_CHECKIN_SECRET": "too-short"}, "DONOR_EVENT_CHECKIN_SECRET must be at least 32 bytes"},
		{"short waitlist key", map[string]string{"DONOR_FOUNDING_CLAIM_KEY": "c2hvcnQ="}, "DONOR_FOUNDING_CLAIM_KEY"},
		{"partial SMTP", map[string]string{"DONOR_SMTP_HOST": "smtp.example.org"}, "invalid email configuration"},
		{"partial APNs", map[string]string{"DONOR_PUSH_APNS_TEAM_ID": "TEAM123456"}, "DONOR_PUSH_APNS_KEY_FILE"},
		{"unreadable FCM key", map[string]string{"DONOR_PUSH_FCM_KEY_FILE": "/nonexistent/firebase.json"}, "DONOR_PUSH_FCM_KEY_FILE"},
		{"SMS stub off this machine", map[string]string{
			"DONOR_PUBLIC_ORIGIN": "https://dad.example.org", "DONOR_SMS_PROVIDER": "stub", "DONOR_SMS_FROM": "DAD",
		}, "DONOR_ALLOW_SIGN_IN_STUBS"},
		{"OIDC stub off this machine", map[string]string{
			"DONOR_PUBLIC_ORIGIN": "https://dad.example.org", "DONOR_OIDC_MODE": "stub", "DONOR_OIDC_STUB_KEY": "k",
			"DONOR_OIDC_GOOGLE_CLIENT_ID": "g.apps",
		}, "DONOR_ALLOW_SIGN_IN_STUBS"},
		{"native return origin with a path", map[string]string{"DONOR_NATIVE_RETURN_ORIGIN": "https://dad.example.org/app"}, "DONOR_NATIVE_RETURN_ORIGIN must be an HTTPS"},
		{"native return origin over plain HTTP", map[string]string{"DONOR_NATIVE_RETURN_ORIGIN": "http://dad.example.org"}, "DONOR_NATIVE_RETURN_ORIGIN must be an HTTPS"},
		{"invalid SMS settings", map[string]string{"DONOR_SMS_PROVIDER": "twilio"}, "invalid SMS configuration"},
		{"invalid OIDC settings", map[string]string{"DONOR_OIDC_MODE": "live"}, "invalid OIDC configuration"},
	} {
		getenv := func(name string) string { return cmp.Or(tc.env[name], base[name]) }
		_, release, err := configure(context.Background(), getenv)
		release()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestConfigureWithoutDatabaseServesHealthOnly(t *testing.T) {
	services, release, err := configure(context.Background(), func(string) string { return "" })
	defer release()
	if err != nil || services.Origin != "http://localhost:3000" || services.Ready != nil || services.Donor != nil || services.Staff != nil {
		t.Fatalf("got %+v, %v; want the local origin and no data services", services, err)
	}
}

func TestConfigureAcceptsAStaffHostBesideThePublicOne(t *testing.T) {
	for _, env := range []map[string]string{
		{"DONOR_STAFF_ORIGIN": "http://staff.localhost:3000"},
		{"DONOR_PUBLIC_ORIGIN": "https://dad.example.org", "DONOR_STAFF_ORIGIN": "https://staff.dad.example.org"},
		{"DONOR_PUBLIC_ORIGIN": "https://dad.example.org", "DONOR_STAFF_ORIGIN": "https://staff.example.org:8443"},
	} {
		services, release, err := configure(context.Background(), func(name string) string { return env[name] })
		release()
		if err != nil || services.StaffOrigin != env["DONOR_STAFF_ORIGIN"] {
			t.Fatalf("%v: got %q, %v", env, services.StaffOrigin, err)
		}
	}
}

func TestConfigureConnectsAsTheAPIRole(t *testing.T) {
	owner := os.Getenv("TEST_DATABASE_URL")
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"its role", map[string]string{"DONOR_API_DATABASE_URL": dbrolestest.URL(t, dbroles.API), "DONOR_ALLOW_OWNER_CONNECTION": "true"}, ""},
		{"schema owner", map[string]string{"DONOR_API_DATABASE_URL": owner}, "can change the schema"},
		{"its role assumed by the owner", map[string]string{"DONOR_API_DATABASE_URL": dbrolestest.URL(t, dbroles.API)}, "can change the schema"},
		{"another role", map[string]string{"DONOR_API_DATABASE_URL": dbrolestest.URL(t, dbroles.Monitor), "DONOR_ALLOW_OWNER_CONNECTION": "true"}, "not a member of dad_api"},
		{"local owner", map[string]string{"DONOR_GO_DATABASE_URL": owner, "DONOR_ALLOW_OWNER_CONNECTION": "true"}, ""},
	} {
		services, release, err := configure(context.Background(), func(name string) string { return tc.env[name] })
		release()
		if tc.want == "" && (err != nil || services.Ready == nil) {
			t.Errorf("%s: got %v, want the database services", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestConfigureOffersDonorPasskeysOnlyOnAHostName(t *testing.T) {
	for origin, want := range map[string]bool{"http://localhost:3000": true, "https://dad.example.org": true, "http://127.0.0.1:3000": false} {
		env := map[string]string{
			"DONOR_API_DATABASE_URL":       dbrolestest.URL(t, dbroles.API),
			"DONOR_ALLOW_OWNER_CONNECTION": "true",
			"DONOR_PUBLIC_ORIGIN":          origin,
		}
		services, release, err := configure(context.Background(), func(name string) string { return env[name] })
		release()
		if err != nil || (services.DonorPasskeys != nil) != want {
			t.Errorf("%s: got passkeys %v, err %v; want passkeys %v", origin, services.DonorPasskeys != nil, err, want)
		}
	}
}

func TestSignInStubsNeedALocalOriginOrAnOptIn(t *testing.T) {
	smsStub := &sms.Settings{Provider: "stub", Sender: "DAD"}
	oidcStub := &oidc.Settings{Mode: "stub", StubKey: "k", ClientIDs: map[oidc.Provider][]string{oidc.Google: {"g"}}}
	none := func(string) string { return "" }
	optIn := func(name string) string { return map[string]string{allowSignInStubs: "true"}[name] }
	for _, tc := range []struct {
		name   string
		sms    *sms.Settings
		oidc   *oidc.Settings
		origin string
		getenv func(string) string
		ok     bool
	}{
		{"SMS stub locally", smsStub, nil, "http://localhost:3000", none, true},
		{"OIDC stub on a local name", nil, oidcStub, "http://dad.localhost:3000", none, true},
		{"SMS stub off this machine", smsStub, nil, "https://dad.example.org", none, false},
		{"OIDC stub off this machine", nil, oidcStub, "https://dad.example.org", none, false},
		{"stubs opted in", smsStub, oidcStub, "https://test.dad.example.org", optIn, true},
		{"real providers", &sms.Settings{Provider: "twilio"}, &oidc.Settings{Mode: "live"}, "https://dad.example.org", none, true},
		{"nothing configured", nil, nil, "https://dad.example.org", none, true},
	} {
		if err := validateSignInStubs(tc.sms, tc.oidc, tc.origin, tc.getenv); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
