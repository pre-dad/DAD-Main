package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
)

func TestStaffResendIsIdempotentLimitedAndAudited(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	staff, staffToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, adminToken := staffFixture(t, pool, "SUPER_ADMIN")
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	s := &StaffService{DB: api}
	message := func(kind string, completed bool, suppressed string) pgtype.UUID {
		t.Helper()
		job := testID(t)
		if _, err := pool.Exec(ctx, `INSERT INTO donor_messages(id,donor_id,kind,dedup_key,context,available_at,completed_at,suppressed_reason,attempts,created_at)
VALUES($1,$2,$3,$4,'{"gift_id":"g"}',now()-interval '2 minutes',CASE WHEN $5 THEN now()-interval '2 minutes' END,NULLIF($6,''),1,now()-interval '2 minutes')`, job, id, kind, "test:"+job.String(), completed, suppressed); err != nil {
			t.Fatal(err)
		}
		return job
	}
	source := message("RECEIPT", true, "")
	for name, candidate := range map[string]pgtype.UUID{
		"pending":    message("RECEIPT", false, ""),
		"suppressed": message("RECEIPT", true, "donor_disabled"),
		"optional":   message("REFERRAL", true, ""),
	} {
		if _, err := s.ResendMessage(ctx, staffToken, candidate, testID(t)); !errors.Is(err, ErrStaffConflict) {
			t.Fatalf("%s message resent: %v", name, err)
		}
	}
	invalid := testID(t)
	invalid.Bytes[6] = 0x10
	if _, err := s.ResendMessage(ctx, staffToken, source, invalid); !errors.Is(err, ErrStaffInput) {
		t.Fatal("non-v4 request id accepted", err)
	}
	if _, err := s.ResendMessage(ctx, "not-a-session", source, testID(t)); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("anonymous resend", err)
	}
	request := testID(t)
	first, err := s.ResendMessage(ctx, staffToken, source, request)
	if err != nil {
		t.Fatal(err)
	}
	view := jsonView(t, first)
	assertKeys(t, view, "queued", "id")
	again, err := s.ResendMessage(ctx, staffToken, source, request)
	if err != nil || jsonView(t, again)["id"] != view["id"] || view["queued"] != true {
		t.Fatal("retry queued another message", again, err)
	}
	if _, err = s.ResendMessage(ctx, adminToken, source, request); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("request id reused by another actor", err)
	}
	if _, err = s.ResendMessage(ctx, staffToken, source, testID(t)); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("resend not rate limited", err)
	}
	var raw string
	var resends int
	if err = pool.QueryRow(ctx, "SELECT context::text,(SELECT count(*) FROM donor_messages WHERE donor_id=$1 AND dedup_key LIKE 'resend:%') FROM donor_messages WHERE dedup_key=$2", id, "resend:"+request.String()).Scan(&raw, &resends); err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	if err = json.Unmarshal([]byte(raw), &saved); err != nil || saved["gift_id"] != "g" || saved["resend_source"] != source.String() ||
		saved["resend_actor"] != staff.String() ||
		resends != 1 {
		t.Fatal("resend context", raw, resends, err)
	}
	var audited int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE actor_id=$1 AND action='message.resend_requested' AND target=$2 AND details->>'source_id'=$3", staff, view["id"], source.String()).Scan(&audited); err != nil ||
		audited != 1 {
		t.Fatal("resend not audited", audited, err)
	}
}

func TestStaffClaimRequestQueuesLoginEmail(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	staff, token := staffFixture(t, pool, "SUPER_ADMIN")
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	s := &StaffService{DB: api}
	if err := s.RequestClaim(ctx, token, testID(t)); err == nil {
		t.Fatal("claim requested for unknown donor")
	}
	if err := s.RequestClaim(ctx, token, id); err != nil {
		t.Fatal(err)
	}
	var queued, audited int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM donor_login_emails WHERE donor_id=$1 AND created_at>$3),(SELECT count(*) FROM donor_staff_audit WHERE actor_id=$2 AND action='message.claim_requested' AND target=$4)", id, staff, time.Now().Add(-time.Minute), id.String()).Scan(&queued, &audited); err != nil ||
		queued != 1 ||
		audited != 1 {
		t.Fatal("claim request", queued, audited, err)
	}
}

func TestStaffDonorDetailShowsUncomputedStreakAsUpdating(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	id, _ := donor(t, pool)
	q := queries.New(pool)
	now := time.Now().UTC()
	if err := q.SeedProjection(ctx, queries.SeedProjectionParams{ID: testID(t), DonorID: id, ComputedAt: timestamp(now)}); err != nil {
		t.Fatal(err)
	}
	s := &StaffService{DB: api}
	detail, err := s.DonorDetail(ctx, token, id, 0, 0)
	if err != nil || detail.Streak != nil {
		t.Fatalf("seeded projection reported a streak: %+v, %v", detail.Streak, err)
	}
	tx, err := as(t, dbroles.ProjectionWorker).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = refreshProjection(ctx, tx, id, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err = s.DonorDetail(ctx, token, id, 0, 0)
	if err != nil || detail.Streak == nil || detail.Streak.Best != 0 {
		t.Fatalf("computed projection missing from detail: %+v, %v", detail.Streak, err)
	}
}

func TestPendingActionIsScopedAndIndependentOfHistoryPages(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	otherActor, otherToken := staffFixture(t, pool, "SUPER_ADMIN")
	_, supportToken := staffFixture(t, pool, "OP_PARTNERSHIPS")
	donorID, _ := donor(t, pool)
	otherDonor, _ := donor(t, pool)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM donor_staff_actions WHERE donor_id=$1 OR donor_id=$2", donorID, otherDonor); err != nil {
			t.Error(err)
		}
	})
	target := testID(t)
	base := time.Now().UTC().Add(-time.Hour)
	insert := func(owner, donor, target pgtype.UUID, kind, status string, at time.Time) pgtype.UUID {
		t.Helper()
		id := testID(t)
		var amount any = int64(1000)
		var period any
		if kind == "CANCEL" {
			amount, period = nil, base.AddDate(0, 1, 0)
		}
		_, err := pool.Exec(
			ctx,
			"INSERT INTO donor_staff_actions(id,actor_id,donor_id,kind,target_id,amount_minor,period_end,reason,status,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'Donor requested change',$8,$9)",
			id,
			owner,
			donor,
			kind,
			target,
			amount,
			period,
			status,
			at,
		)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	saved := insert(actor, donorID, target, "REFUND", "AUTHORIZED", base)
	for i := range 55 {
		insert(actor, donorID, target, "REFUND", "OBSERVED", base.Add(time.Duration(i+1)*time.Second))
	}
	other := insert(otherActor, donorID, target, "REFUND", "AUTHORIZED", base.Add(time.Minute))
	insert(actor, otherDonor, target, "REFUND", "AUTHORIZED", base.Add(time.Minute))
	insert(actor, donorID, testID(t), "REFUND", "AUTHORIZED", base.Add(time.Minute))
	cancellation := insert(actor, donorID, target, "CANCEL", "AUTHORIZED", base.Add(time.Minute))
	s := &StaffService{DB: api}
	for _, tc := range []struct {
		token, kind string
		want        pgtype.UUID
	}{{token, "REFUND", saved}, {otherToken, "REFUND", other}, {token, "CANCEL", cancellation}} {
		got, err := s.PendingAction(ctx, tc.token, donorID, target, tc.kind)
		if err != nil || got.Action == nil || got.Action.ID != tc.want {
			t.Fatalf("%s lookup: %+v %v", tc.kind, got, err)
		}
	}
	empty, err := s.PendingAction(ctx, token, donorID, testID(t), "REFUND")
	if err != nil || empty.Action != nil || jsonView(t, empty)["action"] != nil {
		t.Fatalf("missing action: %+v %v", empty, err)
	}
	if _, err := s.PendingAction(ctx, supportToken, donorID, target, "REFUND"); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("support lookup permitted: %v", err)
	}
	if _, err := s.PendingAction(ctx, "", donorID, target, "CANCEL"); err == nil {
		t.Fatal("anonymous lookup permitted")
	}
	if _, err := s.PendingAction(ctx, token, donorID, target, "UNKNOWN"); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("invalid kind accepted: %v", err)
	}
}

func TestStaffMessageToolsRequireThePermissionAndResendParkedReceipts(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	id, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	s := &StaffService{DB: as(t, dbroles.API)}
	parked := testID(t)
	mustExec(t, pool, `INSERT INTO donor_messages(id,donor_id,kind,dedup_key,context,available_at,attempts,created_at)
VALUES($1,$2,'RECEIPT',$3,'{"gift_id":"g"}',now()-interval '2 minutes',5,now()-interval '2 minutes')`, parked, id, "test:"+parked.String())
	if _, err := s.ResendMessage(ctx, growth, parked, testID(t)); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("operator resent a message: %v", err)
	}
	if err := s.RequestClaim(ctx, growth, id); !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("operator requested a claim link: %v", err)
	}
	var queued int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_login_emails WHERE donor_id=$1", id).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("denied claim request queued an email: %d %v", queued, err)
	}
	got, err := s.ResendMessage(ctx, token, parked, testID(t))
	if err != nil || !got.Queued {
		t.Fatalf("parked receipt not resendable: %+v %v", got, err)
	}
	if _, err = s.ResendMessage(ctx, token, testID(t), testID(t)); err == nil {
		t.Fatal("resent a message that does not exist")
	}
}
