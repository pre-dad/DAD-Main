package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dad/donor-api/internal/store"
)

type dashboardStub struct {
	err   error
	token string
	calls int
}

func (s *dashboardStub) DonorDashboard(ctx context.Context, token string) (store.DonorDashboard, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded database access")
	}
	s.calls++
	s.token = token
	return store.DonorDashboard{Mode: "test", State: "preparing"}, s.err
}

func TestDashboardAuthenticationAndPrivateErrors(t *testing.T) {
	for _, tc := range []struct {
		token  string
		err    error
		status int
	}{
		{"", nil, 401}, {"existing-session", nil, 200}, {"invalid", store.ErrDenied, 401}, {"existing-session", errors.New("private database details"), 503},
	} {
		stub := &dashboardStub{err: tc.err}
		server := NewServer("", Services{Dashboard: stub})
		req := httptest.NewRequest("GET", "/donor/me/dashboard", nil)
		if tc.token != "" {
			req.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: tc.token})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("session %q error %v: got %d %s, want %d uncached", tc.token, tc.err, response.Code, response.Body, tc.status)
		}
		if strings.Contains(response.Body.String(), "private database") {
			t.Fatal("database error leaked")
		}
		if tc.token == "" && stub.calls != 0 {
			t.Fatalf("anonymous request reached the database %d times", stub.calls)
		}
		if stub.token != tc.token {
			t.Fatalf("store saw session %q, want %q", stub.token, tc.token)
		}
	}
}

func TestDashboardInsightsFieldIsOmittedOrCarriesTheLayout(t *testing.T) {
	panel := &store.InsightsPanel{CatalogVersion: 1, Spec: []byte(`[{"type":"Heading","title":"Hi"}]`)}
	for name, tc := range map[string]struct {
		insights *store.InsightsPanel
		want     string
	}{
		"none": {nil, `"impact":{"given_minor":null,"currency":"","initiatives_funded":0,"events_volunteered":0}}`},
		"live": {panel, `"insights":{"catalog_version":1,"spec_id":null,"spec":[{"type":"Heading","title":"Hi"}]}`},
	} {
		body, err := json.Marshal(store.DonorDashboard{Mode: "test", State: "ready", Insights: tc.insights})
		if err != nil || !strings.Contains(string(body), tc.want) || (tc.insights == nil && strings.Contains(string(body), "insights")) {
			t.Errorf("%s: %s, %v", name, body, err)
		}
	}
	body, err := json.Marshal(store.DonorDashboard{Mode: "test", State: "preparing", Insights: panel})
	if err != nil || strings.Contains(string(body), "insights") {
		t.Errorf("a dashboard still preparing carries no layout: %s, %v", body, err)
	}
}
