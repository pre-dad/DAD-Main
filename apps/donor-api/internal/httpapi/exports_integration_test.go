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

// TestStaffExportsRunInTheBackgroundAndDownloadOnlyForTheirRequester walks the
// export flow over HTTP against real storage: request, poll, download, and the
// refusals that protect a finished file.
func TestStaffExportsRunInTheBackgroundAndDownloadOnlyForTheirRequester(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || dsn == "" ||
		(config.ConnConfig.Database != "donor_go" && !strings.HasPrefix(config.ConnConfig.Database, "donor_go_")) {
		t.Fatal("TEST_DATABASE_URL must name a migrated donor_go test database")
	}
	owner, err := store.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	staff := func(role string) string {
		id, err := store.NewID()
		if err != nil {
			t.Fatal(err)
		}
		session, _ := store.NewID()
		token := "export-http-" + session.String()
		if _, err = owner.Exec(ctx, "INSERT INTO donor_staff(id,email,name,role) VALUES($1,$2,'Export test',$3)", id, id.String()+"@example.org", role); err != nil {
			t.Fatal(err)
		}
		if _, err = owner.Exec(ctx, "INSERT INTO donor_staff_sessions(id,staff_id,token_hash,expires_at) VALUES($1,$2,$3,$4)", session, id, store.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, sql := range []string{
				"DELETE FROM donor_export_jobs WHERE actor_id=$1",
				"DELETE FROM donor_staff_audit WHERE actor_id=$1",
				"DELETE FROM donor_staff_sessions WHERE staff_id=$1",
				"DELETE FROM donor_staff WHERE id=$1",
			} {
				if _, err := owner.Exec(ctx, sql, id); err != nil {
					t.Error(err)
				}
			}
		})
		return token
	}
	admin, other, operator := staff("SUPER_ADMIN"), staff("SUPER_ADMIN"), staff("OP_PARTNERSHIPS")

	const staffOrigin = "https://staff.example.org"
	service := &store.StaffService{DB: dbrolestest.Open(t, dbroles.API)}
	server := httpapi.NewServer(
		"",
		httpapi.Services{Origin: "https://donor.example.org", StaffOrigin: staffOrigin, Staff: service, Exports: service},
	)
	call := func(method, path, body, session string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", staffOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "__Host-dad_staff_session", Value: session})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response
	}

	if got := call("POST", "/donor/staff/exports/jobs", `{"dataset":"donors"}`, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous request: %d", got)
	}
	if got := call("POST", "/donor/staff/exports/jobs", `{"dataset":"donors"}`, operator).Code; got != http.StatusForbidden {
		t.Fatalf("operator request: %d", got)
	}
	if got := call("POST", "/donor/staff/exports/jobs", `{"dataset":"nope"}`, admin).Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("unknown dataset: %d", got)
	}
	created := call("POST", "/donor/staff/exports/jobs", `{"dataset":"donors"}`, admin)
	var job store.ExportJob
	if err = json.Unmarshal(created.Body.Bytes(), &job); created.Code != http.StatusOK || err != nil || job.State != "queued" ||
		job.Dataset != "donors" {
		t.Fatalf("request: %d %s", created.Code, created.Body)
	}
	path := "/donor/staff/exports/jobs/" + job.ID
	if got := call("GET", path+"/file", "", admin).Code; got != http.StatusConflict {
		t.Fatalf("download before it is built: %d", got)
	}

	worker := &store.ExportWorker{DB: dbrolestest.Open(t, dbroles.ProjectionWorker)}
	for {
		worked, err := worker.ProcessNext(ctx, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	status := call("GET", path, "", admin)
	if err = json.Unmarshal(status.Body.Bytes(), &job); status.Code != http.StatusOK || err != nil || job.State != "complete" {
		t.Fatalf("status: %d %s", status.Code, status.Body)
	}

	file := call("GET", path+"/file", "", admin)
	header := file.Header()
	if file.Code != http.StatusOK || !strings.HasPrefix(file.Body.String(), "id,created_at,email,") ||
		header.Get("Content-Disposition") != `attachment; filename="dad-donors.csv"` ||
		!strings.HasPrefix(header.Get("Content-Type"), "text/csv") ||
		header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" ||
		header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", file.Code, header)
	}
	for _, c := range []struct {
		name, session string
		want          int
	}{{"another staff member", other, http.StatusNotFound}, {"an operator", operator, http.StatusNotFound}, {"no session", "", http.StatusUnauthorized}} {
		if got := call("GET", path+"/file", "", c.session).Code; got != c.want {
			t.Errorf("download by %s: %d, want %d", c.name, got, c.want)
		}
		if got := call("GET", path, "", c.session).Code; got != c.want {
			t.Errorf("status for %s: %d, want %d", c.name, got, c.want)
		}
	}
	listed := call("GET", "/donor/staff/exports/jobs", "", admin)
	var jobs store.ExportJobs
	if err = json.Unmarshal(listed.Body.Bytes(), &jobs); err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].ID != job.ID {
		t.Fatalf("list: %d %s", listed.Code, listed.Body)
	}
}
