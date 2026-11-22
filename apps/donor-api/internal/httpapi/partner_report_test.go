package httpapi

import (
	"context"
	"dad/donor-api/internal/store"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type reportRefusal struct {
	PartnerPortalSource
	err error
}

func (s reportRefusal) ExportReport(context.Context, string) ([]byte, error) { return nil, s.err }

func TestPartnerReportQuotaResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"daily", store.PartnerReportDailyLimit{RetryAt: time.Now().Add(time.Hour)}, 429, "PARTNER_REPORT_DAILY_LIMIT"},
		{"size", store.ErrPartnerReportTooLarge, 413, "PARTNER_REPORT_TOO_LARGE"},
		{"storage", store.ErrPartnerReportStorage, 409, "PARTNER_REPORT_STORAGE_LIMIT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewServer("", Services{PartnerPortal: reportRefusal{err: test.err}})
			request := httptest.NewRequest("GET", "/donor/partner/report.pdf", nil)
			request.Header.Set("X-DAD-Client", "native")
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("p", 43))
			response := httptest.NewRecorder()
			handler.Handler.ServeHTTP(response, request)
			var body problem
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status || body.Code != test.code || body.Detail == "" {
				t.Fatalf("status=%d body=%+v", response.Code, body)
			}
			if test.status == 429 {
				delay, err := strconv.Atoi(response.Header().Get("Retry-After"))
				if err != nil || delay < 3590 || delay > 3600 {
					t.Fatalf("retry delay=%d err=%v", delay, err)
				}
			}
		})
	}
}
