package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
)

// walk follows next cursors from the first page to the last, returning every
// page's keys. It fails the test on a loop or a page larger than pageSize.
func walk[T any](t *testing.T, fetch func(after string) ([]T, *string, error), key func(T) string) [][]string {
	t.Helper()
	var pages [][]string
	after := ""
	for range 20 {
		rows, next, err := fetch(after)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > pageSize {
			t.Fatalf("page of %d rows exceeds the cap of %d", len(rows), pageSize)
		}
		keys := make([]string, 0, len(rows))
		for _, row := range rows {
			keys = append(keys, key(row))
		}
		pages = append(pages, keys)
		if next == nil {
			return pages
		}
		after = *next
	}
	t.Fatal("cursor never ended")
	return nil
}

func flatten(pages [][]string) []string { return slices.Concat(pages...) }

func tamperedCursors(t *testing.T) []string {
	t.Helper()
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return []string{
		"!!not-base64!!",
		encode("not json"),
		encode(`{}`),
		encode(`{"t":"2026-01-01T00:00:00Z"}`),
		encode(`{"t":"2026-01-01T00:00:00Z","i":"not-a-uuid"}`),
		strings.Repeat("A", 700),
	}
}

func TestStaffSearchPagesByEmailWithoutSkippingOrRepeating(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	_, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	s := &StaffService{DB: as(t, dbroles.API)}
	prefix := "keyset-" + strings.ToLower(testID(t).String()[:8]) + "-"
	insert := func(n int) string {
		t.Helper()
		id, email := testID(t), fmt.Sprintf("%s%03d@example.org", prefix, n)
		if err := queries.New(pool).InsertGuest(ctx, queries.InsertGuestParams{ID: id, Email: email}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cleanExec(t, "DELETE FROM donors WHERE id=$1", id) })
		return email
	}
	var want []string
	for i := 10; i < 130; i++ {
		want = append(want, insert(i*2))
	}
	fetch := func(after string) ([]DonorSummary, *string, error) {
		page, err := s.SearchDonors(ctx, token, prefix, after)
		return page.Donors, page.NextCursor, err
	}
	key := func(d DonorSummary) string { return d.Email }

	first, next, err := fetch("")
	if err != nil || next == nil || len(first) != pageSize {
		t.Fatalf("first page: %d rows, next %v, %v", len(first), next, err)
	}
	// Rows inserted while a reader is between pages must neither shift the rest
	// of the list nor repeat: one sorts before the cursor, one after the list.
	insert(1)
	later := insert(999)
	got := []string{}
	for _, row := range first {
		got = append(got, row.Email)
	}
	for cursor := next; cursor != nil; {
		rows, nextCursor, err := fetch(*cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			got = append(got, row.Email)
		}
		cursor = nextCursor
	}
	if !slices.Equal(got, append(slices.Clone(want), later)) {
		t.Fatalf("paged %d rows across concurrent inserts, want the original %d in order plus the later one", len(got), len(want))
	}
	if pages := walk(t, fetch, key); len(pages) != 3 || len(pages[2]) != len(want)+2-2*pageSize {
		t.Fatalf("last page is not the remainder: %d pages", len(pages))
	}

	for _, bad := range tamperedCursors(t) {
		if _, _, err := fetch(bad); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("cursor %.20q accepted: %v", bad, err)
		}
	}
	timeCursor := encodeCursor(time.Now(), testID(t))
	if _, _, err := fetch(*timeCursor); !errors.Is(err, ErrStaffInput) {
		t.Fatal("a timestamp cursor was accepted by the email search", err)
	}
	if _, err := s.SearchDonors(ctx, growth, prefix, ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("search without donor.read", err)
	}
	if _, err := s.SearchDonors(ctx, "no-session", prefix, ""); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("anonymous search", err)
	}
}

func TestStaffAuditLogPagesNewestFirstWithStableTies(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	s := &StaffService{DB: as(t, dbroles.API)}
	tied := time.Date(2200, 1, 1, 0, 0, 0, 123456000, time.UTC)
	ids := map[string]time.Time{}
	insert := func(at time.Time) pgtype.UUID {
		id := testID(t)
		mustExec(
			t,
			pool,
			`INSERT INTO donor_staff_audit(id,actor_id,action,target,details,created_at) VALUES($1,$2,'test.keyset','t','{}',$3)`,
			id,
			actor,
			at,
		)
		ids[id.String()] = at
		return id
	}
	for range 120 {
		insert(tied)
	}
	newest := insert(tied.Add(time.Hour))
	oldest := insert(tied.Add(-time.Hour))

	fetch := func(after string) ([]AuditEvent, *string, error) {
		page, err := s.AuditEvents(ctx, token, after)
		return page.Events, page.NextCursor, err
	}
	pages := walk(t, fetch, func(e AuditEvent) string { return e.ID.String() })
	got := flatten(pages)
	var mine []string
	for _, id := range got {
		if _, ok := ids[id]; ok {
			mine = append(mine, id)
		}
	}
	if len(mine) != len(ids) || len(got) != len(slices.Compact(slices.Sorted(slices.Values(got)))) {
		t.Fatalf("paged %d of %d rows, with duplicates or gaps", len(mine), len(ids))
	}
	if mine[0] != newest.String() || mine[len(mine)-1] != oldest.String() {
		t.Fatal("audit log is not newest first")
	}
	tiedIDs := mine[1 : len(mine)-1]
	if !slices.IsSorted(tiedIDs) {
		t.Fatal("rows sharing a timestamp are not in id order")
	}
	if len(pages) < 2 {
		t.Fatalf("expected several pages, got %d", len(pages))
	}

	// A row inserted between pages is not repeated and does not shift the rest.
	firstPage, next, err := fetch("")
	if err != nil || next == nil {
		t.Fatal(err)
	}
	insert(tied.Add(2 * time.Hour))
	second, _, err := fetch(*next)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range slices.Concat(firstPage, second) {
		if seen[e.ID.String()] {
			t.Fatal("row repeated after a concurrent insert", e.ID)
		}
		seen[e.ID.String()] = true
	}
	for _, bad := range tamperedCursors(t) {
		if _, _, err := fetch(bad); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("cursor %.20q accepted: %v", bad, err)
		}
	}
	if _, err := s.AuditEvents(ctx, growth, ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("audit log without audit.read", err)
	}
}

func TestStaffMessageAndActionHistoryPageNewestFirst(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	id, _ := donor(t, pool)
	other, _ := donor(t, pool)
	accountCleanup(t, pool, id)
	t.Cleanup(func() {
		cleanExec(t, "DELETE FROM donor_staff_actions WHERE donor_id=$1", id)
		cleanExec(t, "DELETE FROM donor_messages WHERE donor_id=$1", id)
	})
	s := &StaffService{DB: as(t, dbroles.API)}
	tied := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	var messages, actions []string
	for i := range 120 {
		at := tied
		if i == 0 {
			at = tied.Add(time.Minute)
		}
		m, a := testID(t), testID(t)
		mustExec(t, pool, `INSERT INTO donor_messages(id,donor_id,kind,dedup_key,context,available_at,attempts,created_at)
VALUES($1,$2,'RECEIPT',$3,'{}',$4,0,$4)`, m, id, "keyset:"+m.String(), at)
		mustExec(t, pool, `INSERT INTO donor_staff_actions(id,actor_id,donor_id,kind,target_id,amount_minor,reason,created_at)
VALUES($1,$2,$3,'REFUND',$4,100,'Duplicate gift reported',$5)`, a, actor, id, testID(t), at)
		messages, actions = append(messages, m.String()), append(actions, a.String())
	}
	newestFirst := func(ids []string) []string {
		head, tail := ids[0], slices.Sorted(slices.Values(ids[1:]))
		return append([]string{head}, tail...)
	}

	messagePages := walk(t, func(after string) ([]queries.StaffMessagePageRow, *string, error) {
		page, err := s.DonorMessages(ctx, token, id, after)
		return page.Messages, page.NextCursor, err
	}, func(r queries.StaffMessagePageRow) string { return r.ID.String() })
	if !slices.Equal(flatten(messagePages), newestFirst(messages)) || len(messagePages) != 3 || len(messagePages[2]) != 20 {
		t.Fatalf("message pages %v do not list newest first with id tiebreak", messagePages)
	}
	actionPages := walk(t, func(after string) ([]queries.StaffActionPageRow, *string, error) {
		page, err := s.DonorActions(ctx, token, id, after)
		return page.Actions, page.NextCursor, err
	}, func(r queries.StaffActionPageRow) string { return r.ID.String() })
	if !slices.Equal(flatten(actionPages), newestFirst(actions)) || len(actionPages) != 3 {
		t.Fatalf("action pages %v do not list newest first with id tiebreak", actionPages)
	}

	// Exactly one full page has no next cursor, and another donor's rows never appear.
	exact, err := s.DonorActions(ctx, token, id, "")
	if err != nil || len(exact.Actions) != pageSize || exact.NextCursor == nil {
		t.Fatal("first page", err)
	}
	empty, err := s.DonorActions(ctx, token, other, "")
	if err != nil || empty.Actions == nil || len(empty.Actions) != 0 || empty.NextCursor != nil {
		t.Fatalf("another donor's history is not an empty list: %+v %v", empty, err)
	}
	mustExec(t, pool, "DELETE FROM donor_staff_actions WHERE id=$1", mustUUID(t, actions[1]))
	for range 69 {
		mustExec(
			t,
			pool,
			"DELETE FROM donor_staff_actions WHERE id=(SELECT id FROM donor_staff_actions WHERE donor_id=$1 ORDER BY created_at LIMIT 1)",
			id,
		)
	}
	one, err := s.DonorActions(ctx, token, id, "")
	if err != nil || len(one.Actions) != pageSize || one.NextCursor != nil {
		t.Fatalf("exactly one page reported more: %d rows, next %v, %v", len(one.Actions), one.NextCursor, err)
	}

	for _, bad := range tamperedCursors(t) {
		if _, err := s.DonorMessages(ctx, token, id, bad); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("message cursor %.20q accepted: %v", bad, err)
		}
		if _, err := s.DonorActions(ctx, token, id, bad); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("action cursor %.20q accepted: %v", bad, err)
		}
	}
	if _, err := s.DonorMessages(ctx, growth, id, ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("messages without donor.read", err)
	}
	if _, err := s.DonorActions(ctx, growth, id, ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("actions without donor.read", err)
	}
}

func TestPendingPartnerApplicationsPageOldestFirstPastFifty(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	svc := PartnerApplicationService{DB: pool, Staff: &StaffService{DB: as(t, dbroles.API)}}
	_, token := staffFixture(t, pool, "OP_PARTNERSHIPS")
	_, growth := staffFixture(t, pool, "OP_GROWTH")
	cleanPartners(t, pool)
	tied := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 120 {
		id := testID(t)
		at := tied
		if i == 0 {
			at = tied.Add(-time.Hour)
		}
		mustExec(t, pool, `INSERT INTO donor_partner_applications(id,organization,contact_name,work_email,created_at,updated_at)
VALUES($1,$2,'Dana',$3,$4,$4)`, id, fmt.Sprintf("Org %03d", i), fmt.Sprintf("%s@pending.test", id), at)
		ids = append(ids, id.String())
	}
	want := append([]string{ids[0]}, slices.Sorted(slices.Values(ids[1:]))...)
	pages := walk(t, func(after string) ([]PartnerApplication, *string, error) {
		page, err := svc.PendingApplications(ctx, token, after)
		return page.Applications, page.NextCursor, err
	}, func(a PartnerApplication) string { return a.ID })
	got := flatten(pages)
	if !slices.Equal(got[:len(want)], want) || len(pages[0]) != pageSize || len(pages) < 3 {
		t.Fatalf("pending queue is not paged oldest first past %d: %d pages", pageSize, len(pages))
	}

	// A decision removes a row without moving the others: the cursor still resumes after it.
	_, next, err := func() ([]PartnerApplication, *string, error) {
		page, err := svc.PendingApplications(ctx, token, "")
		return page.Applications, page.NextCursor, err
	}()
	if err != nil || next == nil {
		t.Fatal(err)
	}
	if err = svc.Reject(ctx, token, mustUUID(t, want[3]), "Not a fit"); err != nil {
		t.Fatal(err)
	}
	second, err := svc.PendingApplications(ctx, token, *next)
	if err != nil || second.Applications[0].ID != want[pageSize] {
		t.Fatalf("resuming after a rejection skipped or repeated a row: %v", err)
	}
	for _, bad := range tamperedCursors(t) {
		if _, err := svc.PendingApplications(ctx, token, bad); !errors.Is(err, ErrStaffInput) {
			t.Fatalf("cursor %.20q accepted: %v", bad, err)
		}
	}
	if _, err := svc.PendingApplications(ctx, growth, ""); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("queue without partner.manage", err)
	}
}
