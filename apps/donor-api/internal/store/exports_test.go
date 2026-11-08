package store

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type failedExportWriter struct{}

func (failedExportWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// newExportRig returns the services as the processes run them: the API
// requests and serves exports, the projections worker builds them.
func newExportRig(t *testing.T) (*StaffService, *ExportWorker) {
	t.Helper()
	database(t)
	return &StaffService{DB: as(t, dbroles.API)}, &ExportWorker{DB: as(t, dbroles.ProjectionWorker)}
}

func requestExport(t *testing.T, s *StaffService, token, name string) pgtype.UUID {
	t.Helper()
	job, err := s.RequestExport(context.Background(), token, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var id pgtype.UUID
	if err = id.Scan(job.ID); err != nil {
		t.Fatal(err)
	}
	cleanup(t, "DELETE FROM donor_export_jobs WHERE id=$1", id)
	return id
}

// drain runs worker steps until nothing is left and returns how many it took.
func drain(t *testing.T, w *ExportWorker) int {
	t.Helper()
	steps := 0
	for {
		worked, err := w.ProcessNext(context.Background(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return steps
		}
		if steps++; steps > 1000 {
			t.Fatal("export never finished")
		}
	}
}

func download(t *testing.T, s *StaffService, token string, id pgtype.UUID) ([]byte, error) {
	t.Helper()
	file, err := s.OpenExport(context.Background(), token, id)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	err = file.WriteTo(context.Background(), &body)
	return body.Bytes(), err
}

// exportCSV requests, builds and downloads one export.
func exportCSV(t *testing.T, s *StaffService, w *ExportWorker, token, name string) string {
	t.Helper()
	id := requestExport(t, s, token, name)
	drain(t, w)
	body, err := download(t, s, token, id)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// legacyCSV is what the former synchronous export wrote: the dataset in one
// query ordered by id, CRLF rows, formula-neutralized cells.
func legacyCSV(t *testing.T, name string) string {
	t.Helper()
	dataset := exportDatasets[name]
	fields := strings.Fields(dataset.fields)
	query := "SELECT " + strings.Join(fields, ",") + " FROM " + dataset.table
	if name == "referrals" {
		query += " WHERE referrer_id IS NOT NULL"
	}
	rows, err := database(t).Query(context.Background(), query+" ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var body bytes.Buffer
	writer := csv.NewWriter(&body)
	writer.UseCRLF = true
	if err = writer.Write(fields); err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		cells := make([]string, len(values))
		for i, value := range values {
			if cells[i], err = safeCSVCell(value); err != nil {
				t.Fatal(err)
			}
		}
		if err = writer.Write(cells); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	if err = errors.Join(rows.Err(), writer.Error()); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

func exportAudits(t *testing.T, actor pgtype.UUID, action, where string) int {
	t.Helper()
	var count int
	query := "SELECT count(*) FROM donor_staff_audit WHERE actor_id=$1 AND action=$2 " + where
	if err := database(t).QueryRow(context.Background(), query, actor, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestExportsEnforcePermissionsEscapeFormulasAndMatchTheSynchronousOutput(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	id, _ := donor(t, pool)
	if _, err := pool.Exec(ctx, "UPDATE donors SET public_name=$2 WHERE id=$1", id, " \t=HYPERLINK(\"https://example.invalid\")"); err != nil {
		t.Fatal(err)
	}
	// data.export (CSV of donor/gift/staff data) is a §8 "donors & money" power:
	// super-admin only. An operator is denied every dataset.
	for _, name := range []string{"donors", "staff", "audit"} {
		if _, err := api.RequestExport(ctx, support, name); !errors.Is(err, ErrStaffForbidden) {
			t.Fatalf("operator requested %s: %v", name, err)
		}
	}
	if _, err := api.RequestExport(ctx, admin, "no-such-dataset"); !errors.Is(err, ErrStaffInput) {
		t.Fatal("unknown dataset accepted", err)
	}
	if _, err := api.RequestExport(ctx, "not-a-session", "donors"); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("anonymous request accepted", err)
	}
	for name, dataset := range exportDatasets {
		job := requestExport(t, api, admin, name)
		want := legacyCSV(t, name)
		drain(t, worker)
		file, err := api.OpenExport(ctx, admin, job)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var body bytes.Buffer
		if err = file.WriteTo(ctx, &body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if body.String() != want {
			t.Fatalf("%s: export differs from the synchronous output\n got %.300q\nwant %.300q", name, body.String(), want)
		}
		rows, err := csv.NewReader(strings.NewReader(want)).ReadAll()
		if err != nil || len(rows) == 0 || !slices.Equal(rows[0], strings.Fields(dataset.fields)) {
			t.Fatalf("%s: invalid CSV %v", name, err)
		}
		if name == "donors" {
			found := false
			for _, row := range rows[1:] {
				if row[0] == id.String() {
					found = true
					if !strings.HasPrefix(row[len(row)-1], "' \t=") {
						t.Fatal("formula not escaped")
					}
				}
			}
			if !found {
				t.Fatal("donor missing from export")
			}
		}
		for _, field := range rows[0] {
			if slices.Contains([]string{"token_hash", "credential_id", "public_key", "session", "encrypted_data"}, field) {
				t.Fatal("credential field exported")
			}
		}
	}
}

func TestExportBuildsInBoundedStepsAndMatchesAtAnyBatchSize(t *testing.T) {
	pool := database(t)
	api, worker := newExportRig(t)
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	for range 7 {
		donor(t, pool)
	}
	want := legacyCSV(t, "donors")
	rows := strings.Count(want, "\r\n") - 1
	worker.BatchRows = 3
	job := requestExport(t, api, admin, "donors")
	steps := drain(t, worker)
	if want := rows/3 + 1; steps != want {
		t.Fatalf("took %d steps for %d rows in batches of 3, want %d", steps, rows, want)
	}
	body, err := download(t, api, admin, job)
	if err != nil || string(body) != want {
		t.Fatalf("stepwise export differs from the single read: %v", err)
	}
	var chunks, count int
	if err = pool.QueryRow(context.Background(), "SELECT count(*) FROM donor_export_chunks WHERE job_id=$1", job).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(context.Background(), "SELECT chunk_count FROM donor_export_jobs WHERE id=$1", job).Scan(&count); err != nil ||
		chunks != steps ||
		count != steps {
		t.Fatalf("stored %d chunks, job says %d, took %d steps: %v", chunks, count, steps, err)
	}
}

func TestExportResumesAfterAWorkerDiesWithoutRepeatingOrLosingRows(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	for range 6 {
		donor(t, pool)
	}
	want := legacyCSV(t, "donors")
	worker.BatchRows = 2
	job := requestExport(t, api, admin, "donors")
	for range 2 {
		if worked, err := worker.ProcessNext(ctx, time.Now().UTC()); err != nil || !worked {
			t.Fatal(worked, err)
		}
	}
	progress := func() (string, int, int64) {
		var state string
		var chunks int
		var rows int64
		if err := pool.QueryRow(ctx, "SELECT state,chunk_count,row_count FROM donor_export_jobs WHERE id=$1", job).Scan(&state, &chunks, &rows); err != nil {
			t.Fatal(err)
		}
		return state, chunks, rows
	}
	state, chunks, rows := progress()
	if state != exportRunning || chunks != 2 || rows != 4 {
		t.Fatalf("after two steps: %s %d %d", state, chunks, rows)
	}
	// A step that dies partway rolls back whole: no chunk, no cursor move.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := worker.ProcessNext(dead, time.Now().UTC()); err == nil {
		t.Fatal("a cancelled step reported success")
	}
	if s, c, r := progress(); s != state || c != chunks || r != rows {
		t.Fatalf("a dead step changed the job: %s %d %d", s, c, r)
	}
	// Another process picks the job up where it stopped.
	replacement := &ExportWorker{DB: as(t, dbroles.ProjectionWorker), BatchRows: 2}
	drain(t, replacement)
	if body, err := download(t, api, admin, job); err != nil || string(body) != want {
		t.Fatalf("resumed export differs from the single read: %v", err)
	}
	// Finished work is not redone, and its completion is audited once.
	if worked, err := replacement.ProcessNext(ctx, time.Now().UTC()); err != nil || worked {
		t.Fatal("finished export was claimed again", worked, err)
	}
	if n := exportAudits(t, actor, "data.export_finished", "AND details->>'completed'='true'"); n != 1 {
		t.Fatalf("%d completion audit rows, want 1", n)
	}
}

func TestExportIsConsistentAsOfItsRequest(t *testing.T) {
	pool := database(t)
	api, worker := newExportRig(t)
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	before, _ := donor(t, pool)
	job := requestExport(t, api, admin, "donors")
	after, _ := donor(t, pool)
	drain(t, worker)
	body, err := download(t, api, admin, job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), before.String()) || strings.Contains(string(body), after.String()) {
		t.Fatal("export must hold the donor from before the request and not the one created after it")
	}
}

func TestExportDownloadIsBoundToItsRequesterAndCurrentPermission(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	_, other := staffFixture(t, pool, "SUPER_ADMIN")
	job := requestExport(t, api, admin, "gifts")
	if _, err := download(t, api, admin, job); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("an unfinished export was served", err)
	}
	drain(t, worker)
	for name, call := range map[string]func() error{
		"status":   func() error { _, err := api.ExportJob(ctx, other, job); return err },
		"download": func() error { _, err := download(t, api, other, job); return err },
	} {
		if err := call(); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("another staff member's %s: %v", name, err)
		}
	}
	if jobs, err := api.ExportJobs(ctx, other); err != nil || len(jobs.Jobs) != 0 {
		t.Fatal("another member's exports were listed", jobs, err)
	}
	if _, err := download(t, api, "not-a-session", job); !errors.Is(err, ErrStaffDenied) {
		t.Fatal("anonymous download", err)
	}
	if jobs, err := api.ExportJobs(ctx, admin); err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].State != exportComplete {
		t.Fatal("requester's own export not listed", jobs, err)
	}
	if _, err := download(t, api, admin, job); err != nil {
		t.Fatal(err)
	}
	if n := exportAudits(t, actor, "data.export_downloaded", "AND target='gifts'"); n != 1 {
		t.Fatalf("%d download audit rows, want 1", n)
	}
	if n := exportAudits(t, actor, "data.export_download_finished", "AND details->>'completed'='true'"); n != 1 {
		t.Fatalf("%d completed download audit rows, want 1", n)
	}
	file, err := api.OpenExport(ctx, admin, job)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.WriteTo(ctx, failedExportWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("writer failure swallowed", err)
	}
	if n := exportAudits(t, actor, "data.export_download_finished", "AND details->>'completed'='false'"); n != 1 {
		t.Fatalf("%d incomplete download audit rows, want 1", n)
	}
	// A demotion takes the file away at once, though the member still owns it.
	if _, err = pool.Exec(ctx, "UPDATE donor_staff SET role='OP_PARTNERSHIPS' WHERE id=$1", actor); err != nil {
		t.Fatal(err)
	}
	if _, err = api.OpenExport(ctx, admin, job); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("a demoted member downloaded", err)
	}
	if _, err = api.ExportJob(ctx, admin, job); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("a demoted member read status", err)
	}
}

func TestExportFailsWhenItsRequesterLosesAccessBeforeItIsBuilt(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	job := requestExport(t, api, admin, "donors")
	if _, err := pool.Exec(ctx, "UPDATE donor_staff SET disabled=true WHERE id=$1", actor); err != nil {
		t.Fatal(err)
	}
	drain(t, worker)
	var state, failure string
	var chunks int
	if err := pool.QueryRow(ctx, "SELECT state,failure,(SELECT count(*) FROM donor_export_chunks WHERE job_id=id) FROM donor_export_jobs WHERE id=$1", job).
		Scan(&state, &failure, &chunks); err != nil ||
		state != exportFailed ||
		failure != exportForbidden ||
		chunks != 0 {
		t.Fatalf("got %s %s with %d chunks: %v", state, failure, chunks, err)
	}
	if n := exportAudits(t, actor, "data.export_finished", "AND details->>'completed'='false' AND details->>'failure'='forbidden'"); n != 1 {
		t.Fatalf("%d failure audit rows, want 1", n)
	}
}

func TestExportStopsAtTheSizeCapAndDiscardsWhatItStored(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	for range 5 {
		donor(t, pool)
	}
	worker.BatchRows, worker.MaxBytes = 2, 400
	job := requestExport(t, api, admin, "donors")
	drain(t, worker)
	view, err := api.ExportJob(ctx, admin, job)
	if err != nil || view.State != exportFailed || view.Failure != exportTooLarge || view.CompletedAt == nil {
		t.Fatalf("got %+v: %v", view, err)
	}
	var chunks int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_export_chunks WHERE job_id=$1", job).Scan(&chunks); err != nil || chunks != 0 {
		t.Fatalf("%d chunks of a failed export kept: %v", chunks, err)
	}
	if _, err = api.OpenExport(ctx, admin, job); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("a failed export was served", err)
	}
	if n := exportAudits(t, actor, "data.export_finished", "AND details->>'failure'='too_large'"); n != 1 {
		t.Fatalf("%d size-cap audit rows, want 1", n)
	}
}

func TestExportThatKeepsFailingIsFailedAfterItsAttempts(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	job := requestExport(t, api, admin, "donors")
	for attempt := 1; attempt <= exportAttempts; attempt++ {
		if err := worker.recordFailure(ctx, job, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		view, err := api.ExportJob(ctx, admin, job)
		if err != nil {
			t.Fatal(err)
		}
		if failed := view.State == exportFailed; failed != (attempt == exportAttempts) {
			t.Fatalf("attempt %d: state %s", attempt, view.State)
		}
	}
	if n := exportAudits(t, actor, "data.export_finished", "AND details->>'failure'='error'"); n != 1 {
		t.Fatalf("%d error audit rows, want 1", n)
	}
	if worked, err := worker.ProcessNext(ctx, time.Now().UTC()); err != nil || worked {
		t.Fatal("a failed export was worked again", worked, err)
	}
}

func TestExportsExpireAndAreOnlyPurgedAfterRetention(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	_, admin := staffFixture(t, pool, "SUPER_ADMIN")
	job := requestExport(t, api, admin, "donors")
	drain(t, worker)
	now := time.Now().UTC()
	if purged, err := worker.PurgeNext(ctx, now); err != nil || purged {
		t.Fatal("a fresh export was purged", purged, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE donor_export_jobs SET expires_at=$2 WHERE id=$1", job, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := download(t, api, admin, job); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("an expired export was served before it was purged", err)
	}
	if purged, err := (&ExportWorker{DB: worker.DB}).PurgeNext(ctx, now); err != nil || !purged {
		t.Fatal("an expired export was kept", purged, err)
	}
	var jobs, chunks int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM donor_export_jobs WHERE id=$1),(SELECT count(*) FROM donor_export_chunks WHERE job_id=$1)", job).
		Scan(&jobs, &chunks); err != nil ||
		jobs != 0 ||
		chunks != 0 {
		t.Fatalf("%d jobs and %d chunks survived the purge: %v", jobs, chunks, err)
	}
}

func TestExportRequestsAreDeduplicatedAndCapped(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, _ := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	first := requestExport(t, api, admin, "donors")
	again, err := api.RequestExport(ctx, admin, "donors")
	if err != nil || again.ID != first.String() {
		t.Fatal("asking again did not follow the open job", again, err)
	}
	if n := exportAudits(t, actor, "data.export_started", ""); n != 1 {
		t.Fatalf("%d start audit rows, want 1", n)
	}
	requestExport(t, api, admin, "gifts")
	requestExport(t, api, admin, "refunds")
	if _, err = api.RequestExport(ctx, admin, "messages"); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("a fourth open export was accepted", err)
	}
}

func TestReferralExportCarriesTheReferralTypeOnEveryRow(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	api, worker := newExportRig(t)
	actor, admin := staffFixture(t, pool, "SUPER_ADMIN")
	referrer, _ := donor(t, pool)
	kinds := map[string]string{}
	for _, kind := range []string{"LINK", "IRL"} {
		referred, _ := donor(t, pool)
		kinds[referred.String()] = kind
		mustExec(
			t,
			pool,
			"INSERT INTO donor_metric_projections(id,donor_id,referrer_id,referral_kind,net_minor,computed_at,next_refresh_at) VALUES($1,$2,$3,$4,3000,now(),now())",
			testID(t),
			referred,
			referrer,
			kind,
		)
	}
	rows, err := csv.NewReader(strings.NewReader(exportCSV(t, api, worker, admin, "referrals"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	column := slices.Index(rows[0], "referral_kind")
	for _, row := range rows[1:] {
		if row[column] != "LINK" && row[column] != "IRL" {
			t.Fatalf("referral exported without its type: %v", row)
		}
		if want, ok := kinds[row[0]]; ok {
			if row[column] != want || row[1] != referrer.String() {
				t.Fatalf("referral exported as %v, want %s from %s", row, want, referrer)
			}
			delete(kinds, row[0])
		}
	}
	if len(kinds) != 0 {
		t.Fatal("referrals missing from export", kinds)
	}
	var completed bool
	var count int
	if err = pool.QueryRow(ctx, "SELECT (details->>'completed')::bool,(details->>'row_count')::int FROM donor_staff_audit WHERE actor_id=$1 AND action='data.export_finished' AND target='referrals'", actor).Scan(&completed, &count); err != nil ||
		!completed ||
		count != len(rows)-1 {
		t.Fatal("finished export not audited with its row count", completed, count, err)
	}
}

func TestSafeCSVCellNeutralizesTextButNotNumbers(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{
		{"=1+1", "'=1+1"},
		{" @SUM(A1)", "' @SUM(A1)"},
		{"\tplain", "'\tplain"},
		{int64(-5), "-5"},
		{map[string]any{"reason": "=HYPERLINK()"}, `{"reason":"=HYPERLINK()"}`},
		{nil, ""},
	} {
		if got, err := safeCSVCell(c.in); err != nil || got != c.want {
			t.Fatalf("%#v: got %q %v", c.in, got, err)
		}
	}
}
