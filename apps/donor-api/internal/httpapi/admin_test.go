package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
)

type actionsStub struct {
	AdminSource
	cursors []string
	target  pgtype.UUID
	kind    string
}

func (s *actionsStub) PendingAction(
	_ context.Context,
	session string,
	_ pgtype.UUID,
	target pgtype.UUID,
	kind string,
) (store.PendingAction, error) {
	if session != "staff-session" {
		return store.PendingAction{}, store.ErrStaffDenied
	}
	s.target, s.kind = target, kind
	return store.PendingAction{}, nil
}

func TestPendingActionValidatesTargetAndKind(t *testing.T) {
	stub := &actionsStub{}
	server := NewServer("", Services{Admin: stub})
	id := "7d6f6b1c-6f4e-4c3e-9a51-6f1d9c1e2a3b"
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"?target_id=" + id + "&kind=REFUND", 200},
		{"?target_id=" + id + "&kind=CANCEL", 200},
		{"?target_id=" + id + "&kind=UNKNOWN", 422},
		{"?target_id=bad&kind=REFUND", 422},
		{"?kind=REFUND", 422},
	} {
		stub.kind = ""
		r := httptest.NewRequest("GET", "/donor/staff/donors/"+id+"/actions/pending"+tc.query, nil)
		r.AddCookie(&http.Cookie{Name: staffCookie, Value: "staff-session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, response.Code, response.Body)
		}
		if tc.status == 200 && (stub.target.String() != id || stub.kind == "") {
			t.Fatal("lookup filters not forwarded")
		}
		if tc.status != 200 && stub.kind != "" {
			t.Fatal("invalid lookup reached store")
		}
	}
}

func (s *actionsStub) DonorActions(_ context.Context, session string, _ pgtype.UUID, cursor string) (store.ActionPage, error) {
	s.cursors = append(s.cursors, cursor)
	if session != "staff-session" {
		return store.ActionPage{}, store.ErrStaffDenied
	}
	if cursor != "" {
		return store.ActionPage{Actions: []queries.StaffActionPageRow{}}, nil
	}
	next := "next-position"
	return store.ActionPage{Actions: make([]queries.StaffActionPageRow, 50), NextCursor: &next}, nil
}

func TestStaffActionHistoryIsPaged(t *testing.T) {
	stub := &actionsStub{}
	server := NewServer("", Services{Admin: stub})
	get := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/donor/staff/donors/7d6f6b1c-6f4e-4c3e-9a51-6f1d9c1e2a3b/actions"+query, nil)
		r.AddCookie(&http.Cookie{Name: staffCookie, Value: "staff-session"})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response
	}
	for _, tc := range []struct {
		query string
		rows  int
		next  any
	}{{"", 50, "next-position"}, {"?cursor=next-position", 0, nil}} {
		response := get(tc.query)
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 200 || len(body) != 2 {
			t.Fatalf("%q: %d %s", tc.query, response.Code, response.Body)
		}
		rows, ok := body["actions"].([]any)
		if !ok || len(rows) != tc.rows || body["next_cursor"] != tc.next {
			t.Fatalf("%q: %s", tc.query, response.Body)
		}
	}
	if len(stub.cursors) != 2 || stub.cursors[0] != "" || stub.cursors[1] != "next-position" {
		t.Fatalf("cursors passed to store: %v", stub.cursors)
	}
	for _, query := range []string{"?cursor=" + strings.Repeat("c", maxCursorLength+1)} {
		if response := get(query); response.Code != 422 {
			t.Fatalf("%s: got %d", query, response.Code)
		}
	}
	if len(stub.cursors) != 2 {
		t.Fatal("oversized cursor reached the store")
	}
}

func TestStaffErrorsMapToStatuses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		detail string
	}{
		{store.ErrStaffDenied, 401, "Passkey, invitation, or session invalid or expired"},
		{store.ErrStaffForbidden, 403, "Permission denied"},
		{store.ErrStaffInput, 422, "Invalid request fields"},
		{store.ErrStaffExists, 409, "A staff account with this email already exists"},
		{store.ErrStaffConflict, 409, "Staff access changed or conflicts with this request"},
		{store.EmailRoleError{Role: "DONOR"}, 409, "This email already has a donor account. Use a different email for this login."},
		{store.EmailRoleError{Role: "PARTNER"}, 409, "This email already signs in to a partner organization. Use a different email for this login."},
	} {
		if status, detail := staffStatus(tc.err); status != tc.status || detail != tc.detail {
			t.Errorf("%v: got %d %q, want %d %q", tc.err, status, detail, tc.status, tc.detail)
		}
	}
}

type queueStub struct {
	AdminSource
	state, cursor string
	calls         int
}

func (s *queueStub) FailedPayments(_ context.Context, session, state, cursor string) (store.FailedPaymentPage, error) {
	if session != "staff-session" {
		return store.FailedPaymentPage{}, store.ErrStaffDenied
	}
	s.calls++
	s.state, s.cursor = state, cursor
	next := "next-page"
	return store.FailedPaymentPage{Payments: []store.FailedPayment{}, NextCursor: &next}, nil
}

func (s *queueStub) AccountDeletions(_ context.Context, session, cursor string) (store.AccountDeletionPage, error) {
	if session != "staff-session" {
		return store.AccountDeletionPage{}, store.ErrStaffForbidden
	}
	s.calls++
	s.cursor = cursor
	return store.AccountDeletionPage{Deletions: []store.AccountDeletion{}}, nil
}

func TestStaffQueuesForwardFiltersAndBoundTheCursor(t *testing.T) {
	stub := &queueStub{}
	server := NewServer("", Services{Admin: stub})
	get := func(target string, session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		if session != "" {
			r.AddCookie(&http.Cookie{Name: staffCookie, Value: session})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		return response
	}
	if response := get("/donor/staff/failed-payments?state=lapsed&cursor=abc", "staff-session"); response.Code != 200 ||
		stub.state != "lapsed" || stub.cursor != "abc" {
		t.Fatalf("filters not forwarded: %d %s", response.Code, response.Body)
	}
	var body struct {
		Payments   []any   `json:"payments"`
		NextCursor *string `json:"next_cursor"`
	}
	if response := get("/donor/staff/failed-payments", "staff-session"); json.Unmarshal(response.Body.Bytes(), &body) != nil ||
		body.NextCursor == nil || *body.NextCursor != "next-page" || body.Payments == nil {
		t.Fatalf("page not served as documented: %s", response.Body)
	}
	if response := get("/donor/staff/deletions?cursor=xyz", "staff-session"); response.Code != 200 || stub.cursor != "xyz" {
		t.Fatalf("deletion cursor not forwarded: %d", response.Code)
	}
	calls := stub.calls
	long := strings.Repeat("a", maxCursorLength+1)
	for _, path := range []string{"/donor/staff/failed-payments", "/donor/staff/deletions"} {
		if response := get(path+"?cursor="+long, "staff-session"); response.Code != 422 {
			t.Fatalf("%s accepted an oversize cursor: %d", path, response.Code)
		}
		if response := get(path, ""); response.Code != 401 {
			t.Fatalf("%s served without a staff session: %d", path, response.Code)
		}
	}
	if stub.calls != calls {
		t.Fatal("an invalid request reached the store")
	}
	if response := get("/donor/staff/deletions", "other"); response.Code != 403 {
		t.Fatalf("store refusal not mapped to 403: %d", response.Code)
	}
}
