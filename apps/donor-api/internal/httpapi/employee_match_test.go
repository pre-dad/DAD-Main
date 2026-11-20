package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

type matchPaymentStub struct {
	EmployeeStaffSource
	recorded []pgtype.UUID
}

func (s *matchPaymentStub) RecordMatchPayment(_ context.Context, _ string, _ pgtype.UUID, _ int32, _ int64, key pgtype.UUID) error {
	s.recorded = append(s.recorded, key)
	return nil
}

func TestStaffMatchPaymentRequiresAnIdempotencyKey(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		status       int
		recorded     int
	}{
		{"missing", "", http.StatusUnprocessableEntity, 0},
		{"not a version 4 UUID", "00000000-0000-1000-8000-000000000001", http.StatusUnprocessableEntity, 0},
		{"valid", "00000000-0000-4000-8000-000000000001", http.StatusOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &matchPaymentStub{}
			server := NewServer("", Services{EmployeeStaff: stub})
			r := httptest.NewRequest("POST", "/donor/staff/partners/"+testID+"/match-payment", strings.NewReader(`{"year":2026,"amount_minor":500}`))
			r.Header.Set("Origin", testStaffOrigin)
			r.AddCookie(&http.Cookie{Name: staffCookie, Value: strings.Repeat("s", 43)})
			if tc.header != "" {
				r.Header.Set("Idempotency-Key", tc.header)
			}
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, r)
			if response.Code != tc.status || len(stub.recorded) != tc.recorded {
				t.Fatalf("got %d %s with %d payments; want %d with %d", response.Code, response.Body, len(stub.recorded), tc.status, tc.recorded)
			}
		})
	}
}
