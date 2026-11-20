package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func partnerPushSession(t *testing.T, pool *pgxpool.Pool, partner pgtype.UUID, app bool) (pgtype.UUID, string) {
	t.Helper()
	user := testID(t)
	mustExec(t, pool, `INSERT INTO donor_partner_users(id,partner_id,email,name,role,accepted_at)
        VALUES($1,$2,$3,'Partner','ADMIN',now())`, user, partner, user.String()+"@partner.test")
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_partner_users WHERE id=$1", user) })
	portal := PartnerPortalService{DB: as(t, dbroles.API)}
	token, err := portal.issueSession(context.Background(), queries.New(portal.DB), user, time.Now().UTC(), app)
	if err != nil {
		t.Fatal(err)
	}
	return user, token
}

func TestPartnerPushRegistrationAndAccountTransfer(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api := as(t, dbroles.API)
	partner := approvedPartner(t, pool, "Push partner", "PUSHPARTNER", 0, 0)
	_, browser := partnerPushSession(t, pool, partner, false)
	_, native := partnerPushSession(t, pool, partner, true)
	service := PartnerPushService{DB: api, Platforms: []string{push.IOS, push.Android}}
	device := PushDevice{Platform: push.IOS, Token: iosToken, Environment: "SANDBOX"}
	for _, token := range []string{browser, "invalid"} {
		if _, err := service.RegisterDevice(ctx, token, device); !errors.Is(err, ErrPartnerDenied) {
			t.Fatalf("registration accepted: %v", err)
		}
	}
	donorID, _ := donor(t, pool)
	accountCleanup(t, pool, donorID)
	donorSession := claimSession(t, pool, donorID, Client{App: true})
	donorPush := PushService{DB: api, Platforms: []string{push.IOS}}
	if _, err := donorPush.RegisterDevice(ctx, donorSession.Access, device); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterDevice(ctx, donorSession.Access, device); !errors.Is(err, ErrPartnerDenied) {
		t.Fatalf("donor authenticated as partner: %v", err)
	}
	if _, err := service.RegisterDevice(ctx, native, device); err != nil {
		t.Fatal(err)
	}
	if _, _, registered := pushTokenOf(t, pool, donorSession.Access); registered {
		t.Fatal("installation remained attached to donor")
	}
	if _, err := donorPush.RegisterDevice(ctx, donorSession.Access, device); err != nil {
		t.Fatal(err)
	}
	var registered bool
	if err := pool.QueryRow(ctx, "SELECT push_token IS NOT NULL FROM donor_partner_sessions WHERE token_hash=$1", HashToken(native)).Scan(&registered); err != nil ||
		registered {
		t.Fatalf("installation remained attached to partner: %v %v", registered, err)
	}
	if _, err := service.RegisterDevice(ctx, native, device); err != nil {
		t.Fatal(err)
	}
	if err := (PartnerPortalService{DB: api}).SignOut(ctx, native); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterDevice(ctx, native, device); !errors.Is(err, ErrPartnerDenied) {
		t.Fatalf("revoked session registered: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT push_token IS NOT NULL FROM donor_partner_sessions WHERE token_hash=$1", HashToken(native)).Scan(&registered); err != nil ||
		registered {
		t.Fatalf("signed-out session kept registration: %v %v", registered, err)
	}
}

func TestPartnerPushDeliveryRetrySuppressionAndTokenRotation(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api := as(t, dbroles.API)
	partner := approvedPartner(t, pool, "Push delivery partner", "PUSHDELIVERY", 0, 0)
	user, native := partnerPushSession(t, pool, partner, true)
	service := PartnerPushService{DB: api, Platforms: []string{push.IOS}}
	if _, err := service.RegisterDevice(ctx, native, PushDevice{Platform: push.IOS, Token: iosToken}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	worker := PartnerPushWorker{DB: as(t, dbroles.PushWorker), Providers: map[string]push.Provider{push.IOS: provider}}
	now := time.Now().UTC().Add(time.Second)
	step := func(at time.Time) bool {
		t.Helper()
		worked, err := worker.DeliverNext(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return worked
	}
	if !step(now) || len(provider.notes) != 1 || provider.notes[0].Category != "PARTNER_APPROVED" || provider.notes[0].Path != "/partner" {
		t.Fatalf("approval push: %+v", provider.notes)
	}
	queue := func(kind, key string) {
		t.Helper()
		if err := queuePartnerMessage(ctx, queries.New(api), partner, kind, key, map[string]string{}, now); err != nil {
			t.Fatal(err)
		}
	}
	queue("METRICS_VERIFIED", "partner-metrics-fixture")
	queue("METRICS_VERIFIED", "partner-metrics-fixture")
	provider.result = func(push.Device) error { return push.Unavailable{After: time.Minute} }
	if !step(now) {
		t.Fatal("no metric push")
	}
	if step(now.Add(time.Second)) {
		t.Fatal("retry ignored provider delay")
	}
	provider.result = nil
	if !step(now.Add(time.Hour)) || len(provider.notes) != 3 || provider.notes[1].ID != provider.notes[2].ID {
		t.Fatalf("retry lost collapse identifier: %+v", provider.notes)
	}
	if step(now.Add(time.Hour)) {
		t.Fatal("duplicate event queued")
	}
	queue("INITIATIVE_COMPLETE", "partner-complete-disabled")
	mustExec(t, pool, "UPDATE donor_partner_users SET disabled=true WHERE id=$1", user)
	if !step(now) || len(provider.notes) != 3 {
		t.Fatal("disabled user received notification")
	}
	mustExec(t, pool, "UPDATE donor_partner_users SET disabled=false WHERE id=$1", user)
	queue("INITIATIVE_COMPLETE", "partner-complete-rotated")
	replacement := strings.Repeat("b2", 32)
	provider.result = func(push.Device) error {
		if _, err := service.RegisterDevice(ctx, native, PushDevice{Platform: push.IOS, Token: replacement}); err != nil {
			t.Fatal(err)
		}
		return push.ErrUnregistered
	}
	if !step(now) {
		t.Fatal("no completion push")
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT push_token FROM donor_partner_sessions WHERE token_hash=$1", HashToken(native)).Scan(&stored); err != nil ||
		stored != replacement {
		t.Fatalf("late rejection removed replacement: %q %v", stored, err)
	}
	provider.result = nil
	queue("METRICS_VERIFIED", "partner-expired")
	if !step(now.Add(25*time.Hour)) || len(provider.notes) != 4 {
		t.Fatal("expired push was sent")
	}
	queue("METRICS_VERIFIED", "partner-unregistered")
	if _, err := service.UnregisterDevice(ctx, native); err != nil {
		t.Fatal(err)
	}
	if !step(now) || len(provider.notes) != 4 {
		t.Fatal("unregistered installation was reached")
	}
}

func TestPartnerPushContentCoversEveryKindAndRejectsOthers(t *testing.T) {
	for _, kind := range []string{"APPROVED", "METRICS_VERIFIED", "INITIATIVE_COMPLETE"} {
		n, ok := partnerPushContent(queries.DonorPartnerPushNotification{Kind: kind})
		if !ok || n.Title == "" || n.Body == "" || n.Category != "PARTNER_"+kind || n.Path != "/partner" {
			t.Errorf("%s: %+v ok=%v", kind, n, ok)
		}
	}
	if _, ok := partnerPushContent(queries.DonorPartnerPushNotification{Kind: "UNKNOWN"}); ok {
		t.Error("an unknown kind rendered")
	}
}
