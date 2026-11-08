package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"dad/donor-api/internal/credentials"
	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/httpapi"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/oidc"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/sms"
	"dad/donor-api/internal/store"
	"dad/donor-api/internal/wallet"
)

// shutdownTimeout lets in-flight processor calls (bounded at 25s) finish
// instead of being cut off and left for reconciliation. It stays inside the
// ECS stopTimeout of 30s, after which the task is killed regardless.
const shutdownTimeout = 27 * time.Second

// trustedProxies parses DONOR_TRUSTED_PROXY_CIDRS. Ranges broader than a
// private /8 (IPv4) or /32 (IPv6) are refused because trusting them would let
// arbitrary clients choose their own rate-limit identity.
func trustedProxies(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for field := range strings.SplitSeq(value, ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, errors.New("DONOR_TRUSTED_PROXY_CIDRS must contain valid CIDR ranges")
		}
		if (prefix.Addr().Is4() && prefix.Bits() < 8) || (prefix.Addr().Is6() && prefix.Bits() < 32) {
			return nil, fmt.Errorf("DONOR_TRUSTED_PROXY_CIDRS range %s is too broad; list the proxy's own address or network", prefix)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// allowSignInStubs lets a deployment off this machine, such as a shared test
// environment, run the recording SMS stub or the HMAC OIDC stub.
const allowSignInStubs = "DONOR_ALLOW_SIGN_IN_STUBS"

// validateSignInStubs keeps the sign-in stubs, which accept forged tokens and
// send no messages, to local runs unless a deployment opts in explicitly.
func validateSignInStubs(phone *sms.Settings, social *oidc.Settings, origin string, getenv func(string) string) error {
	stubbed := (phone != nil && phone.Provider == "stub") || (social != nil && social.Mode == "stub")
	if !stubbed || payments.LocalOrigin(origin) || getenv(allowSignInStubs) == "true" {
		return nil
	}
	return fmt.Errorf(
		"DONOR_SMS_PROVIDER=stub and DONOR_OIDC_MODE=stub are for local runs; set %s=true only on a test deployment",
		allowSignInStubs,
	)
}

// validateStaffOrigin requires the back office, when enabled, to have its own
// host: host-only cookies and passkeys are scoped by host, so neither site's
// sessions or credentials reach the other.
func validateStaffOrigin(staff, public string, getenv func(string) string) error {
	if staff == "" {
		if getenv("DONOR_STAFF_ENCRYPTION_KEY") != "" {
			return errors.New(
				"set DONOR_STAFF_ORIGIN: staff now sign in with passkeys on their own host, and DONOR_STAFF_ENCRYPTION_KEY is no longer used",
			)
		}
		return nil
	}
	if err := payments.ValidateOrigin(staff); err != nil {
		return fmt.Errorf("DONOR_STAFF_ORIGIN %w", err)
	}
	staffURL, _ := url.Parse(staff)
	publicURL, _ := url.Parse(public)
	// The public site may claim a parent domain as its passkey relying party.
	if staffHost, publicHost := staffURL.Hostname(), publicURL.Hostname(); publicHost == staffHost ||
		strings.HasSuffix(publicHost, "."+staffHost) {
		return errors.New("DONOR_STAFF_ORIGIN must use its own host, neither DONOR_PUBLIC_ORIGIN's host nor a parent domain of it")
	}
	return nil
}

// configure validates the environment and connects the services it enables.
// Every configuration error is reported before the database is contacted.
// The returned function releases the database pool.
func configure(ctx context.Context, getenv func(string) string) (httpapi.Services, func(), error) {
	var services httpapi.Services
	release := func() {}
	var err error
	if services.TrustedProxies, err = trustedProxies(getenv("DONOR_TRUSTED_PROXY_CIDRS")); err != nil {
		return services, release, err
	}
	email, err := mail.FromEnvironment(getenv)
	if err != nil {
		return services, release, err
	}
	services.EmailConfigured = email != nil
	services.Origin = cmp.Or(getenv("DONOR_PUBLIC_ORIGIN"), "http://localhost:3000")
	if err = payments.ValidateOrigin(services.Origin); err != nil {
		return services, release, fmt.Errorf("DONOR_PUBLIC_ORIGIN %w", err)
	}
	nativeReturnOrigin := cmp.Or(getenv("DONOR_NATIVE_RETURN_ORIGIN"), services.Origin)
	if err = payments.ValidateOrigin(nativeReturnOrigin); err != nil {
		return services, release, fmt.Errorf("DONOR_NATIVE_RETURN_ORIGIN %w", err)
	}
	if services.Wallet, err = wallet.FromEnvironment(getenv, services.Origin); err != nil {
		return services, release, err
	}
	smsSettings, err := sms.FromEnvironment(getenv)
	if err != nil {
		return services, release, fmt.Errorf("SMS sign-in: %w", err)
	}
	oidcSettings, err := oidc.FromEnvironment(getenv)
	if err != nil {
		return services, release, fmt.Errorf("OIDC sign-in: %w", err)
	}
	if err = validateSignInStubs(smsSettings, oidcSettings, services.Origin, getenv); err != nil {
		return services, release, err
	}
	stripeKey, webhookSecret := getenv("STRIPE_SECRET_KEY"), getenv("STRIPE_WEBHOOK_SECRET")
	if (stripeKey == "") != (webhookSecret == "") {
		return services, release, errors.New("set both STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET, or neither")
	}
	stripeCeiling, err := payments.RequestsPerSecond(getenv)
	if err != nil {
		return services, release, err
	}
	var stripeGateway *payments.StripeGateway
	var gateway payments.StaffFinanceGateway
	if stripeKey != "" {
		if stripeGateway, err = payments.NewStripeGateway(stripeKey); err != nil {
			return services, release, err
		}
		gateway = stripeGateway
	}
	services.StaffOrigin = getenv("DONOR_STAFF_ORIGIN")
	if err = validateStaffOrigin(services.StaffOrigin, services.Origin, getenv); err != nil {
		return services, release, err
	}
	var foundingClaims store.FoundingClaimKey
	if value := getenv("DONOR_FOUNDING_CLAIM_KEY"); value != "" {
		if foundingClaims, err = store.ParseFoundingClaimKey(value); err != nil {
			return services, release, fmt.Errorf("DONOR_FOUNDING_CLAIM_KEY %w", err)
		}
	}
	var contentKeys credentials.Keyring
	if value := getenv("DONOR_CONTENT_ENCRYPTION_KEY"); value != "" {
		if contentKeys, err = credentials.ParseKeyring(value); err != nil {
			return services, release, fmt.Errorf("DONOR_CONTENT_ENCRYPTION_KEY %w", err)
		}
	}
	platforms, err := push.RegistrationPlatforms(getenv)
	if err != nil {
		return services, release, err
	}
	// A WebAuthn relying party is a domain name, so a public site addressed by
	// IP, as in CI's browser tests, runs without donor passkeys.
	publicHost, _ := url.Parse(services.Origin)
	_, ipErr := netip.ParseAddr(publicHost.Hostname())
	passkeys := ipErr != nil
	for _, feature := range []struct {
		name    string
		enabled bool
	}{
		{"payments (STRIPE_SECRET_KEY)", gateway != nil},
		{"account email (SMTP settings)", email != nil},
		{"staff back office (DONOR_STAFF_ORIGIN)", services.StaffOrigin != ""},
		{"source documents (DONOR_CONTENT_ENCRYPTION_KEY)", contentKeys.Configured()},
		{"website waitlist Founding Donors (DONOR_FOUNDING_CLAIM_KEY)", len(foundingClaims) > 0},
		{"iOS app notifications (DONOR_PUSH_PLATFORMS)", slices.Contains(platforms, push.IOS)},
		{"Android app notifications (DONOR_PUSH_PLATFORMS)", slices.Contains(platforms, push.Android)},
		{"donor passkeys (a DONOR_PUBLIC_ORIGIN host name, not an IP address)", passkeys},
	} {
		if !feature.enabled {
			slog.Info("feature disabled until configured", "feature", feature.name)
		}
	}

	dsn := dbroles.API.URL(getenv)
	if dsn == "" {
		slog.Warn(dbroles.API.Variable() + " is unset; only health and static responses are available")
		return services, release, nil
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := store.Open(startup, dsn, dbroles.API.Pool)
	if err != nil {
		return services, release, errors.New("donor database startup failed")
	}
	if err = migrations.Verify(startup, pool); err != nil {
		pool.Close()
		return services, release, errors.New("donor schema is not current; run migrations before starting the API")
	}
	if err = dbroles.API.Check(startup, pool, getenv(dbroles.AllowOwnerVariable) == "true"); err != nil {
		pool.Close()
		return services, release, err
	}
	if stripeGateway != nil {
		stripeGateway.UseBudget(store.StripeBudget{DB: pool, PerSecond: stripeCeiling})
	}
	services.Ready = func(ctx context.Context) error { return migrations.Verify(ctx, pool) }
	services.InitiativeRead = store.InitiativeReader{DB: pool}
	services.TodayInitiative = store.DonorInitiativeService{DB: pool}
	services.EventRead = store.EventReader{DB: pool, SupportEmail: getenv("DONOR_SUPPORT_EMAIL")}
	services.Reservations = store.ReservationService{DB: pool, CheckInSecret: getenv("DONOR_EVENT_CHECKIN_SECRET")}
	services.EventCheckInSecret = getenv("DONOR_EVENT_CHECKIN_SECRET")
	portal := store.PartnerPortalService{DB: pool}
	services.PartnerPortal = store.PartnerDashboardService{PartnerPortalService: portal}
	services.PartnerApply = store.PartnerApplicationService{DB: pool}
	services.ApplicationReceipts = store.PartnerApplicationService{DB: pool}
	services.Dashboard = store.DashboardReader{DB: pool}
	services.Account = store.AccountService{DB: pool}
	services.Passwords = store.PasswordService{DB: pool, FoundingClaims: foundingClaims}
	services.Donor = store.DonorService{DB: pool, Origin: services.Origin}
	services.Analytics = store.AnalyticsService{DB: pool}
	if passkeys {
		donorPasskeys, err := store.NewDonorPasskeyService(pool, services.Origin)
		if err != nil {
			pool.Close()
			return services, release, fmt.Errorf("donor passkeys: %w", err)
		}
		services.DonorPasskeys = donorPasskeys
	}
	if sender := sms.NewSender(smsSettings); sender != nil {
		services.PhoneSignIn = store.PhoneService{DB: pool, Sender: sender}
	}
	if verifier := oidc.NewVerifier(oidcSettings); verifier != nil {
		services.OIDCSignIn = store.OIDCService{DB: pool, Verifier: verifier, Settings: oidcSettings}
	}
	services.Checkout = store.CheckoutService{
		DB:                 pool,
		Gateway:            gateway,
		Origin:             services.Origin,
		NativeReturnOrigin: nativeReturnOrigin,
		FoundingClaims:     foundingClaims,
	}
	services.Recurring = store.RecurringService{DB: pool, Gateway: gateway, Origin: services.Origin}
	services.Push = store.PushService{DB: pool, Platforms: platforms}
	services.PartnerPush = store.PartnerPushService{DB: pool, Platforms: platforms}
	if webhookSecret != "" {
		services.Webhook = payments.Receiver{Secret: webhookSecret, Inbox: store.StripeInbox{DB: pool}}
	}
	if email != nil {
		services.EmailChange = store.EmailChangeService{DB: pool, EncryptionKeys: email.EncryptionKeys, FoundingClaims: foundingClaims}
	}
	var staff *store.StaffService
	if services.StaffOrigin != "" {
		staff, err = store.NewStaffService(pool, services.StaffOrigin)
		if err != nil {
			pool.Close()
			return services, release, errors.New("staff configuration is invalid")
		}
		services.Staff, services.Admin, services.Exports = staff, staff, staff
		services.StaffFinance = store.StaffFinanceService{Staff: staff, Gateway: gateway, Origin: services.Origin}
		services.ContentAdmin = store.ContentService{Staff: staff, EncryptionKeys: contentKeys}
		services.Wording = store.WordingService{Staff: staff}
		services.FoundingClaims = store.FoundingClaimService{Staff: staff, Key: foundingClaims}
		services.Initiatives = store.InitiativeService{Staff: staff}
		services.Events = store.EventService{Staff: staff}
		services.Reporting = &store.ReportingService{Staff: staff}
		services.StaffPartner = store.PartnerApplicationService{DB: pool, Staff: staff}
	}
	employees := store.EmployeeService{DB: pool, Staff: staff}
	services.EmployeeDonor, services.EmployeePartner = employees, employees
	if staff != nil {
		services.EmployeeStaff = employees
	}
	services.Badges = store.BadgeService{DB: pool, Staff: staff}
	services.Network = store.NetworkService{DB: pool}
	return services, pool.Close, nil
}

// serve runs the server until it fails or ctx ends, then drains in-flight requests.
func serve(ctx context.Context, server *http.Server) error {
	failures := make(chan error, 1)
	go func() { failures <- server.ListenAndServe() }()
	slog.Info("donor API listening", "address", server.Addr)
	select {
	case err := <-failures:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func run(getenv func(string) string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	services, release, err := configure(ctx, getenv)
	if err != nil {
		return err
	}
	defer release()
	return serve(ctx, httpapi.NewServer(cmp.Or(getenv("DONOR_GO_LISTEN"), "127.0.0.1:8002"), services))
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Getenv); err != nil {
		slog.Error("donor service stopped", "error", err)
		os.Exit(1)
	}
}
