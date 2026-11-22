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

// TestInsightLayoutsAreAuthoredAndApprovedByDifferentStaff walks the approval
// flow over HTTP against real storage: the routes' authorization, CSRF and
// validation answers, the two-person rule, and the single live layout.
func TestInsightLayoutsAreAuthoredAndApprovedByDifferentStaff(t *testing.T) {
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
	if _, err = owner.Exec(ctx, "DELETE FROM donor_insight_specs"); err != nil {
		t.Fatal(err)
	}
	staff := func(role string) string {
		id, _ := store.NewID()
		session, _ := store.NewID()
		token := "insight-http-" + session.String()
		if _, err := owner.Exec(ctx, "INSERT INTO donor_staff(id,email,name,role) VALUES($1,$2,'Insight test',$3)", id, id.String()+"@example.org", role); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, "INSERT INTO donor_staff_sessions(id,staff_id,token_hash,expires_at) VALUES($1,$2,$3,$4)", session, id, store.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, sql := range []string{
				"DELETE FROM donor_insight_specs WHERE author_id=$1 OR approver_id=$1 OR retired_by=$1",
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
	author, approver, operator := staff("SUPER_ADMIN"), staff("SUPER_ADMIN"), staff("OP_GROWTH")

	const staffOrigin = "https://staff.example.org"
	service := &store.StaffService{DB: dbrolestest.Open(t, dbroles.API)}
	server := httpapi.NewServer(
		"",
		httpapi.Services{
			Origin:      "https://donor.example.org",
			StaffOrigin: staffOrigin,
			Staff:       service,
			Insights:    store.InsightService{Staff: service},
		},
	)
	call := func(method, path, body, session, origin string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Content-Type", "application/json")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "__Host-dad_staff_session", Value: session})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response
	}
	as := func(session, method, path, body string) *httptest.ResponseRecorder {
		return call(method, path, body, session, staffOrigin)
	}
	const valid = `{"spec":[{"type":"Heading","title":"Your impact"},{"type":"Metric","bind":"/impact/given_minor","format":"usd_minor","caption":"given"}]}`

	if got := call("POST", "/donor/staff/insights", valid, "", staffOrigin).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous create: %d", got)
	}
	if got := call("GET", "/donor/staff/insights", "", "", staffOrigin).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", got)
	}
	if got := call("POST", "/donor/staff/insights", valid, author, "https://donor.example.org").Code; got != http.StatusForbidden {
		t.Fatalf("create from the public origin: %d", got)
	}
	if got := call("POST", "/donor/staff/insights", valid, author, "").Code; got != http.StatusForbidden {
		t.Fatalf("create without an Origin: %d", got)
	}
	if got := as(operator, "POST", "/donor/staff/insights", valid).Code; got != http.StatusForbidden {
		t.Fatalf("operator create: %d", got)
	}
	if got := as(operator, "GET", "/donor/staff/insights", "").Code; got != http.StatusForbidden {
		t.Fatalf("operator list: %d", got)
	}

	invalid := as(
		author,
		"POST",
		"/donor/staff/insights",
		`{"spec":[{"type":"LinkButton","navigate":"/partner"},{"type":"Text","bind":"/nowhere"}]}`,
	)
	var problem struct {
		Detail string `json:"detail"`
		Errors []struct{ Path, Code, Message string }
	}
	err = json.Unmarshal(invalid.Body.Bytes(), &problem)
	if invalid.Code != http.StatusUnprocessableEntity || err != nil || len(problem.Errors) != 2 {
		t.Fatalf("invalid spec: %d %s", invalid.Code, invalid.Body)
	}
	if problem.Errors[0].Path != "/0/navigate" || problem.Errors[0].Code != "NAV_FORBIDDEN" || problem.Errors[1].Code != "BIND_UNRESOLVED" {
		t.Fatalf("errors: %s", invalid.Body)
	}
	for _, body := range []string{`{}`, `{"spec":null}`, `{"spec":[],"extra":1}`, `not json`} {
		if got := as(author, "POST", "/donor/staff/insights", body).Code; got != http.StatusUnprocessableEntity {
			t.Errorf("create %q: %d", body, got)
		}
	}
	if got := as(author, "POST", "/donor/staff/insights", `{"spec":[{"type":"Future","note":"a\u0000b"}]}`).Code; got != http.StatusUnprocessableEntity {
		t.Errorf("NUL in a spec: %d", got)
	}

	created := as(author, "POST", "/donor/staff/insights", valid)
	var spec store.InsightSpec
	if err = json.Unmarshal(created.Body.Bytes(), &spec); created.Code != http.StatusOK || err != nil || spec.State != "draft" {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	path := "/donor/staff/insights/" + spec.ID.String()
	if got := as(approver, "POST", path+"/approve", `{}`).Code; got != http.StatusConflict {
		t.Fatalf("approving a draft: %d", got)
	}
	if got := as(approver, "POST", path+"/submit", `{}`).Code; got != http.StatusForbidden {
		t.Fatalf("another member submitting: %d", got)
	}
	if got := as(author, "POST", path+"/submit", `{}`).Code; got != http.StatusOK {
		t.Fatalf("submit: %d", got)
	}
	self := as(author, "POST", path+"/approve", `{}`)
	if self.Code != http.StatusForbidden || !strings.Contains(self.Body.String(), "different staff member") {
		t.Fatalf("self approval: %d %s", self.Code, self.Body)
	}
	if got := as(author, "POST", path+"/reject", `{"reason":"mine"}`).Code; got != http.StatusForbidden {
		t.Fatalf("self rejection: %d", got)
	}
	if got := as(approver, "POST", path+"/reject", `{"reason":""}`).Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("rejection without a reason: %d", got)
	}
	if got := as(operator, "POST", path+"/approve", `{}`).Code; got != http.StatusForbidden {
		t.Fatalf("operator approval: %d", got)
	}
	approved := as(approver, "POST", path+"/approve", `{}`)
	if err = json.Unmarshal(approved.Body.Bytes(), &spec); approved.Code != http.StatusOK || err != nil || spec.State != "live" {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}
	if got := as(approver, "POST", path+"/approve", `{}`).Code; got != http.StatusConflict {
		t.Fatalf("approving twice: %d", got)
	}

	listed := as(operator, "GET", "/donor/staff/insights", "")
	if listed.Code != http.StatusForbidden {
		t.Fatalf("operator list: %d", listed.Code)
	}
	listed = as(approver, "GET", "/donor/staff/insights?state=live", "")
	var page store.InsightSpecPage
	if err = json.Unmarshal(listed.Body.Bytes(), &page); listed.Code != http.StatusOK || err != nil || len(page.Specs) != 1 ||
		strings.Contains(listed.Body.String(), `"spec":`) {
		t.Fatalf("list: %d %s", listed.Code, listed.Body)
	}
	if got := as(approver, "GET", "/donor/staff/insights?state=bogus", "").Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("bad state: %d", got)
	}
	if got := as(approver, "GET", "/donor/staff/insights/not-a-uuid", "").Code; got != http.StatusNotFound {
		t.Fatalf("malformed id: %d", got)
	}
	if got := as(approver, "GET", "/donor/staff/insights/"+strings.Replace(spec.ID.String(), spec.ID.String()[:4], "0000", 1), "").Code; got != http.StatusNotFound {
		t.Fatalf("unknown id: %d", got)
	}
	if got := as(approver, "POST", path+"/retire", `{}`).Code; got != http.StatusOK {
		t.Fatalf("retire: %d", got)
	}
	if got := as(approver, "POST", path+"/retire", `{}`).Code; got != http.StatusConflict {
		t.Fatalf("retiring twice: %d", got)
	}
	if got := call("POST", path+"/retire", `{}`, approver, "https://donor.example.org").Code; got != http.StatusForbidden {
		t.Fatalf("retire from the public origin: %d", got)
	}
}
