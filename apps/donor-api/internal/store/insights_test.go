package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	specA = `[{"type":"Heading","title":"Your impact"},{"type":"Metric","bind":"/impact/initiatives_funded","format":"integer"}]`
	specB = `[{"type":"Callout","tone":"success","title":"Thank you","body":"Every gift counts."}]`
)

type insightHarness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    InsightService
	ctx    context.Context
	author pgtype.UUID
	authorToken, otherToken,
	deniedToken string
	other pgtype.UUID
}

// insightFixture clears the table, because at most one spec is live across the
// whole database, and registers a cleanup that runs before the staff fixtures'.
func insightFixture(t *testing.T) insightHarness {
	t.Helper()
	pool := database(t)
	api := as(t, dbroles.API)
	h := insightHarness{t: t, pool: pool, svc: InsightService{Staff: &StaffService{DB: api}}, ctx: context.Background()}
	h.author, h.authorToken = staffFixture(t, pool, "SUPER_ADMIN")
	h.other, h.otherToken = staffFixture(t, pool, "SUPER_ADMIN")
	_, h.deniedToken = staffFixture(t, pool, "OP_GROWTH")
	clear := func() {
		if _, err := pool.Exec(h.ctx, "DELETE FROM donor_insight_specs"); err != nil {
			t.Error(err)
		}
	}
	clear()
	t.Cleanup(clear)
	return h
}

func (h insightHarness) draft(spec string) InsightSpec {
	h.t.Helper()
	created, err := h.svc.Create(h.ctx, h.authorToken, []byte(spec))
	if err != nil {
		h.t.Fatal(err)
	}
	return created
}

func (h insightHarness) pending(spec string) InsightSpec {
	h.t.Helper()
	created := h.draft(spec)
	submitted, err := h.svc.Submit(h.ctx, h.authorToken, created.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return submitted
}

func (h insightHarness) live(spec string) InsightSpec {
	h.t.Helper()
	approved, err := h.svc.Approve(h.ctx, h.otherToken, h.pending(spec).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return approved
}

func (h insightHarness) audit(target pgtype.UUID) []string {
	h.t.Helper()
	rows, err := h.pool.Query(h.ctx, "SELECT action FROM donor_staff_audit WHERE target=$1 ORDER BY created_at,id", target.String())
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err = rows.Scan(&action); err != nil {
			h.t.Fatal(err)
		}
		actions = append(actions, action)
	}
	return actions
}

func (h insightHarness) liveCount() int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.ctx, "SELECT count(*) FROM donor_insight_specs WHERE state='live'").Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestInsightSpecLifecycleAndAudit(t *testing.T) {
	h := insightFixture(t)
	created := h.draft(specA)
	if created.State != InsightDraft || created.AuthorID != h.author || created.CatalogVersion != 1 || created.ApproverID.Valid {
		t.Fatalf("created = %+v", created)
	}
	updated, err := h.svc.Update(h.ctx, h.authorToken, created.ID, []byte(specB))
	if err != nil || !strings.Contains(string(updated.Spec), "Thank you") || updated.State != InsightDraft {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	submitted, err := h.svc.Submit(h.ctx, h.authorToken, created.ID)
	if err != nil || submitted.State != InsightPending || submitted.SubmittedAt == nil {
		t.Fatalf("submit = %+v, %v", submitted, err)
	}
	approved, err := h.svc.Approve(h.ctx, h.otherToken, created.ID)
	if err != nil || approved.State != InsightLive || approved.ApproverID != h.other || approved.DecidedAt == nil ||
		approved.ApproverName == nil {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	retired, err := h.svc.Retire(h.ctx, h.authorToken, created.ID)
	if err != nil || retired.State != InsightRetired || retired.RetiredAt == nil {
		t.Fatalf("retire = %+v, %v", retired, err)
	}
	want := []string{
		"content.insight_created",
		"content.insight_updated",
		"content.insight_submitted",
		"content.insight_approved",
		"content.insight_retired",
	}
	if got := h.audit(created.ID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

func TestInsightSpecRejection(t *testing.T) {
	h := insightFixture(t)
	spec := h.pending(specA)
	if _, err := h.svc.Reject(h.ctx, h.otherToken, spec.ID, "  "); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("blank reason: %v", err)
	}
	if _, err := h.svc.Reject(h.ctx, h.otherToken, spec.ID, strings.Repeat("x", maxRejectReason+1)); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("long reason: %v", err)
	}
	if _, err := h.svc.Reject(h.ctx, h.authorToken, spec.ID, "No"); !errors.Is(err, ErrInsightTwoPerson) {
		t.Fatalf("author rejecting: %v", err)
	}
	rejected, err := h.svc.Reject(h.ctx, h.otherToken, spec.ID, "  Wording needs counsel review  ")
	if err != nil || rejected.State != InsightRejected || rejected.RejectReason == nil ||
		*rejected.RejectReason != "Wording needs counsel review" {
		t.Fatalf("reject = %+v, %v", rejected, err)
	}
	if got := h.audit(spec.ID); got[len(got)-1] != "content.insight_rejected" {
		t.Fatalf("audit = %v", got)
	}
	for name, run := range map[string]func() error{
		"approve": func() error { _, err := h.svc.Approve(h.ctx, h.otherToken, spec.ID); return err },
		"submit":  func() error { _, err := h.svc.Submit(h.ctx, h.authorToken, spec.ID); return err },
		"retire":  func() error { _, err := h.svc.Retire(h.ctx, h.otherToken, spec.ID); return err },
		"update":  func() error { _, err := h.svc.Update(h.ctx, h.authorToken, spec.ID, []byte(specB)); return err },
	} {
		if err := run(); !errors.Is(err, ErrInsightState) {
			t.Errorf("%s after rejection: %v", name, err)
		}
	}
}

func TestInsightApprovalNeedsAnotherPerson(t *testing.T) {
	h := insightFixture(t)
	spec := h.pending(specA)
	if _, err := h.svc.Approve(h.ctx, h.authorToken, spec.ID); !errors.Is(err, ErrInsightTwoPerson) || !errors.Is(err, ErrStaffForbidden) {
		t.Fatalf("self approval: %v", err)
	}
	if h.liveCount() != 0 {
		t.Fatal("a spec went live without a second person")
	}
	got, err := h.svc.Get(h.ctx, h.authorToken, spec.ID)
	if err != nil || got.State != InsightPending || got.ApproverID.Valid {
		t.Fatalf("after refused approval: %+v, %v", got, err)
	}
	if _, err = h.pool.Exec(h.ctx, "UPDATE donor_insight_specs SET state='live',approver_id=author_id,decided_at=now() WHERE id=$1", spec.ID); err == nil {
		t.Fatal("the database accepted an approver who is the author")
	}
}

func TestInsightApprovalRetiresThePreviousLiveSpec(t *testing.T) {
	h := insightFixture(t)
	first := h.live(specA)
	second := h.pending(specB)
	approved, err := h.svc.Approve(h.ctx, h.otherToken, second.ID)
	if err != nil || approved.State != InsightLive {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	previous, err := h.svc.Get(h.ctx, h.authorToken, first.ID)
	if err != nil || previous.State != InsightRetired || previous.RetiredAt == nil {
		t.Fatalf("previous = %+v, %v", previous, err)
	}
	if h.liveCount() != 1 {
		t.Fatalf("%d live specs", h.liveCount())
	}
	if got := h.audit(first.ID); got[len(got)-1] != "content.insight_retired" {
		t.Fatalf("previous spec audit = %v", got)
	}
	var replaced string
	if err = h.pool.QueryRow(h.ctx, "SELECT details->>'replaced' FROM donor_staff_audit WHERE target=$1 AND action='content.insight_approved'", second.ID.String()).Scan(&replaced); err != nil ||
		replaced != first.ID.String() {
		t.Fatalf("approval audit replaced = %q, %v", replaced, err)
	}
}

func TestInsightSingleLiveIsEnforcedByTheDatabase(t *testing.T) {
	h := insightFixture(t)
	h.live(specA)
	other := h.draft(specB)
	if _, err := h.pool.Exec(h.ctx, "UPDATE donor_insight_specs SET state='live',submitted_at=now(),approver_id=$2,decided_at=now() WHERE id=$1", other.ID, h.other); err == nil {
		t.Fatal("two specs live at once")
	}
}

func TestInsightConcurrentApprovalsLeaveOneLive(t *testing.T) {
	h := insightFixture(t)
	a, b := h.pending(specA), h.pending(specB)
	results := make(chan error, 2)
	for _, id := range []pgtype.UUID{a.ID, b.ID} {
		go func() {
			_, err := h.svc.Approve(h.ctx, h.otherToken, id)
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, ErrInsightState) {
			t.Fatal(err)
		}
	}
	if h.liveCount() != 1 {
		t.Fatalf("%d live specs", h.liveCount())
	}
}

func TestInsightDraftsBelongToTheirAuthor(t *testing.T) {
	h := insightFixture(t)
	draft := h.draft(specA)
	if _, err := h.svc.Update(h.ctx, h.otherToken, draft.ID, []byte(specB)); !errors.Is(err, ErrInsightNotAuthor) {
		t.Fatalf("update by another: %v", err)
	}
	if _, err := h.svc.Submit(h.ctx, h.otherToken, draft.ID); !errors.Is(err, ErrInsightNotAuthor) {
		t.Fatalf("submit by another: %v", err)
	}
	if _, err := h.svc.Approve(h.ctx, h.otherToken, draft.ID); !errors.Is(err, ErrInsightState) {
		t.Fatalf("approving a draft: %v", err)
	}
	pending := h.pending(specB)
	if _, err := h.svc.Update(h.ctx, h.authorToken, pending.ID, []byte(specA)); !errors.Is(err, ErrInsightState) {
		t.Fatalf("editing a pending spec: %v", err)
	}
	if _, err := h.svc.Retire(h.ctx, h.otherToken, pending.ID); !errors.Is(err, ErrInsightState) {
		t.Fatalf("retiring a pending spec: %v", err)
	}
}

func TestInsightRetireNeedsNoSecondPerson(t *testing.T) {
	h := insightFixture(t)
	live := h.live(specA)
	retired, err := h.svc.Retire(h.ctx, h.authorToken, live.ID)
	if err != nil || retired.State != InsightRetired {
		t.Fatalf("retire = %+v, %v", retired, err)
	}
	if _, err = h.svc.Retire(h.ctx, h.authorToken, live.ID); !errors.Is(err, ErrInsightState) {
		t.Fatalf("second retire: %v", err)
	}
}

func TestInsightSpecsAreValidatedOnEveryWrite(t *testing.T) {
	h := insightFixture(t)
	bad := `[{"type":"LinkButton","navigate":"/partner"}]`
	for name, spec := range map[string]string{
		"catalog":   bad,
		"not json":  `[{`,
		"not array": `{"type":"Heading"}`,
		"empty":     `[]`,
		"oversize":  `[{"type":"Future","note":"` + strings.Repeat("x", maxInsightSpecBytes) + `"}]`,
	} {
		_, err := h.svc.Create(h.ctx, h.authorToken, []byte(spec))
		var invalid *InsightInvalidError
		if !errors.As(err, &invalid) || !errors.Is(err, ErrStaffInput) || len(invalid.Errors) == 0 {
			t.Errorf("create %s: %v", name, err)
		}
	}
	_, err := h.svc.Create(h.ctx, h.authorToken, []byte(bad))
	if invalid, _ := errors.AsType[*InsightInvalidError](err); invalid == nil || invalid.Errors[0].Code != "NAV_FORBIDDEN" ||
		invalid.Errors[0].Path != "/0/navigate" {
		t.Fatalf("errors = %+v", invalid)
	}
	draft := h.draft(specA)
	if _, err = h.svc.Update(h.ctx, h.authorToken, draft.ID, []byte(bad)); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("update with an invalid spec: %v", err)
	}
	if got, _ := h.svc.Get(h.ctx, h.authorToken, draft.ID); !strings.Contains(string(got.Spec), "Your impact") {
		t.Fatalf("an invalid update changed the draft: %s", got.Spec)
	}
	for _, spec := range []string{bad, `[{"type":"Future","anything":[1,2,3]}]`} {
		stored := h.draft(specA)
		if _, err = h.pool.Exec(h.ctx, "UPDATE donor_insight_specs SET spec=$2 WHERE id=$1", stored.ID, spec); err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.Submit(h.ctx, h.authorToken, stored.ID)
		if (spec == bad) != errors.Is(err, ErrStaffInput) {
			t.Errorf("submit %s: %v", spec, err)
		}
	}
	var count int
	if err = h.pool.QueryRow(h.ctx, "SELECT count(*) FROM donor_insight_specs").Scan(&count); err != nil || count != 3 {
		t.Fatalf("%d specs stored, %v", count, err)
	}
}

func TestInsightApprovalValidatesAgain(t *testing.T) {
	h := insightFixture(t)
	spec := h.pending(specA)
	if _, err := h.pool.Exec(h.ctx, `UPDATE donor_insight_specs SET spec='[{"type":"LinkButton","navigate":"/partner"}]' WHERE id=$1`, spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Approve(h.ctx, h.otherToken, spec.ID); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("approving an invalid spec: %v", err)
	}
	if h.liveCount() != 0 {
		t.Fatal("an invalid spec went live")
	}
}

func TestInsightPermissions(t *testing.T) {
	h := insightFixture(t)
	spec := h.pending(specA)
	for name, run := range map[string]func() error{
		"list":    func() error { _, err := h.svc.List(h.ctx, h.deniedToken, "", ""); return err },
		"get":     func() error { _, err := h.svc.Get(h.ctx, h.deniedToken, spec.ID); return err },
		"create":  func() error { _, err := h.svc.Create(h.ctx, h.deniedToken, []byte(specA)); return err },
		"update":  func() error { _, err := h.svc.Update(h.ctx, h.deniedToken, spec.ID, []byte(specA)); return err },
		"submit":  func() error { _, err := h.svc.Submit(h.ctx, h.deniedToken, spec.ID); return err },
		"approve": func() error { _, err := h.svc.Approve(h.ctx, h.deniedToken, spec.ID); return err },
		"reject":  func() error { _, err := h.svc.Reject(h.ctx, h.deniedToken, spec.ID, "No"); return err },
		"retire":  func() error { _, err := h.svc.Retire(h.ctx, h.deniedToken, spec.ID); return err },
	} {
		if err := run(); !errors.Is(err, ErrStaffForbidden) {
			t.Errorf("%s without content.publish: %v", name, err)
		}
		if err := func() error { _, err := h.svc.Get(h.ctx, "no-such-session", spec.ID); return err }(); !errors.Is(err, ErrStaffDenied) {
			t.Errorf("signed-out get: %v", err)
		}
	}
	if got, _ := h.svc.Get(h.ctx, h.authorToken, spec.ID); got.State != InsightPending {
		t.Fatalf("a refused call changed the spec: %s", got.State)
	}
}

func TestInsightUnknownIDIsNotFound(t *testing.T) {
	h := insightFixture(t)
	missing := testID(t)
	if _, err := h.svc.Get(h.ctx, h.authorToken, missing); err == nil || !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("get: %v", err)
	}
	if _, err := h.svc.Approve(h.ctx, h.otherToken, missing); err == nil || !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("approve: %v", err)
	}
}

func TestInsightListPagesNewestFirstAndFilters(t *testing.T) {
	h := insightFixture(t)
	const total = pageSize + 3
	for i := range total {
		id := testID(t)
		if _, err := h.pool.Exec(h.ctx,
			"INSERT INTO donor_insight_specs(id,spec,catalog_version,state,author_id,created_at,updated_at) VALUES($1,$2,1,'draft',$3,$4,$4)",
			id, specA, h.author, time.Now().Add(time.Duration(i-total)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	h.live(specB)
	first, err := h.svc.List(h.ctx, h.authorToken, "", "")
	if err != nil || len(first.Specs) != pageSize || first.NextCursor == nil {
		t.Fatalf("first page: %d rows, cursor %v, %v", len(first.Specs), first.NextCursor, err)
	}
	if first.Specs[0].State != InsightLive || first.Specs[0].AuthorName == "" {
		t.Fatalf("newest first: %+v", first.Specs[0])
	}
	second, err := h.svc.List(h.ctx, h.authorToken, "", *first.NextCursor)
	if err != nil || len(second.Specs) != total+1-pageSize || second.NextCursor != nil {
		t.Fatalf("second page: %d rows, %v", len(second.Specs), err)
	}
	seen := map[pgtype.UUID]bool{}
	for _, row := range append(first.Specs, second.Specs...) {
		if seen[row.ID] {
			t.Fatalf("%s listed twice", row.ID)
		}
		seen[row.ID] = true
	}
	live, err := h.svc.List(h.ctx, h.authorToken, InsightLive, "")
	if err != nil || len(live.Specs) != 1 {
		t.Fatalf("live filter: %d rows, %v", len(live.Specs), err)
	}
	if _, err = h.svc.List(h.ctx, h.authorToken, "bogus", ""); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("bad state filter: %v", err)
	}
	if _, err = h.svc.List(h.ctx, h.authorToken, "", "not-a-cursor"); !errors.Is(err, ErrStaffInput) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestLiveInsightsSkipsASpecThatNoLongerValidates(t *testing.T) {
	h := insightFixture(t)
	q := queries.New(h.pool)
	if panel, err := liveInsights(h.ctx, q); err != nil || panel != nil {
		t.Fatalf("no live spec: %+v, %v", panel, err)
	}
	live := h.live(specA)
	panel, err := liveInsights(h.ctx, q)
	if err != nil || panel == nil || panel.SpecID != live.ID || panel.CatalogVersion != 1 ||
		!strings.Contains(string(panel.Spec), "Your impact") {
		t.Fatalf("panel = %+v, %v", panel, err)
	}
	if _, err = h.pool.Exec(h.ctx, `UPDATE donor_insight_specs SET spec='[{"type":"Text","bind":"/nowhere"}]' WHERE id=$1`, live.ID); err != nil {
		t.Fatal(err)
	}
	if panel, err = liveInsights(h.ctx, q); err != nil || panel != nil {
		t.Fatalf("invalid live spec served: %+v, %v", panel, err)
	}
	if _, err = h.pool.Exec(h.ctx, `UPDATE donor_insight_specs SET spec=$2,catalog_version=99 WHERE id=$1`, live.ID, specA); err != nil {
		t.Fatal(err)
	}
	if panel, err = liveInsights(h.ctx, q); err != nil || panel != nil {
		t.Fatalf("spec for a newer catalog served: %+v, %v", panel, err)
	}
}

func TestDashboardCarriesTheLiveInsightOnlyWhenEnabled(t *testing.T) {
	h := insightFixture(t)
	api := as(t, dbroles.API)
	donorID, token := signedInDonor(t, h.pool)
	if _, err := h.pool.Exec(h.ctx,
		"INSERT INTO donor_metric_projections(id,donor_id,net_minor,computed_at,next_refresh_at) VALUES($1,$1,0,now(),now()+interval '1 hour')", donorID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.pool.Exec(h.ctx, "DELETE FROM donor_metric_projections WHERE donor_id=$1", donorID) })
	live := h.live(specA)
	off, err := DashboardReader{DB: api}.DonorDashboard(h.ctx, token)
	if err != nil || off.State != "ready" || off.Insights != nil {
		t.Fatalf("flag off: %+v, %v", off.Insights, err)
	}
	on, err := DashboardReader{DB: api, Insights: true}.DonorDashboard(h.ctx, token)
	if err != nil || on.Insights == nil || on.Insights.SpecID != live.ID {
		t.Fatalf("flag on: %+v, %v", on.Insights, err)
	}
	if _, err = h.svc.Retire(h.ctx, h.authorToken, live.ID); err != nil {
		t.Fatal(err)
	}
	if none, err := (DashboardReader{DB: api, Insights: true}).DonorDashboard(h.ctx, token); err != nil || none.Insights != nil {
		t.Fatalf("nothing live: %+v, %v", none.Insights, err)
	}
}
