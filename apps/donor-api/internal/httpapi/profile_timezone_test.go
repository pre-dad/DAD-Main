package httpapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/geo"
	"dad/donor-api/internal/store"
)

type profileTimezoneSource struct {
	DonorSource
	seen *store.ProfileUpdate
}

func (source profileTimezoneSource) SaveProfile(_ context.Context, _ string, update store.ProfileUpdate) (store.Profile, error) {
	if source.seen != nil {
		*source.seen = update
	}
	return store.Profile{}, nil
}

func TestProfileAcceptsTimezoneCorrection(t *testing.T) {
	var seen store.ProfileUpdate
	server := NewServer("", Services{Donor: profileTimezoneSource{seen: &seen}})
	for _, test := range []struct {
		name, field string
		status      int
		present     bool
		value       string
	}{
		{"correction", `,"timezone_override":"America/New_York"`, 200, true, "America/New_York"},
		{"omit", "", 200, false, ""},
		{"reset", `,"timezone_override":""`, 200, true, ""},
		{"null", `,"timezone_override":null`, 422, false, ""},
		{"boolean", `,"timezone_override":true`, 422, false, ""},
		{"object", `,"timezone_override":{}`, 422, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen = store.ProfileUpdate{}
			request := httptest.NewRequest("POST", "/donor/me/profile", strings.NewReader(`{"zip":"60601"`+test.field+`}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-DAD-Client", "native")
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("p", 43))
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if (seen.TimezoneOverride != nil) != test.present || (test.present && *seen.TimezoneOverride != test.value) {
				t.Fatalf("correction forwarded as %v", seen.TimezoneOverride)
			}
		})
	}
}

func TestProfileTimezoneChoicesMatchServerValidation(t *testing.T) {
	schema := loadContract(t).Paths["/donor/me/profile"]["post"].RequestBody.Content["application/json"].Schema
	choices := schema["properties"].(map[string]any)["timezone_override"].(map[string]any)["enum"].([]any)
	for _, choice := range choices {
		zone := fmt.Sprint(choice)
		if zone == "" {
			continue
		}
		if !geo.ValidTimezoneOverride(zone) {
			t.Fatalf("contract zone %q is not accepted", zone)
		}
		if _, err := time.LoadLocation(zone); err != nil {
			t.Fatal(err)
		}
	}
}
