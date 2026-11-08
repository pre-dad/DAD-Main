package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With no services configured, a request that passes validation answers 503;
// any other status shows the handler judged the input without storage.
func TestHandlersValidateInputBeforeCallingServices(t *testing.T) {
	const key = "8f4f52bb-99f6-490e-8313-5156cc4f8b45"
	reason := `"reason":"Duplicate gift"`
	for _, tc := range []struct {
		method, path, body string
		key                bool
		status             int
	}{
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false}`, false, 503},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":null,"push_streak_milestone":false}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":-1,"referral_emails":true,"marketing_emails":false}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false,"push_streak_lapsed":true,"push_badge_earned":false,"rankings_opted_out":true,"network_visible":false,"volunteer":true,"volunteer_interests":["food-bank","mentoring"]}`, false, 503},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false,"push_streak_lapsed":true,"push_badge_earned":false,"rankings_opted_out":true,"network_visible":false,"volunteer":true,"volunteer_interests":[""]}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false,"push_streak_lapsed":true,"push_badge_earned":false,"rankings_opted_out":true,"network_visible":false,"volunteer":true,"volunteer_interests":[" padded"]}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false,"push_streak_lapsed":true,"push_badge_earned":false,"rankings_opted_out":true,"network_visible":false,"volunteer":true,"volunteer_interests":["a","a"]}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false,"push_streak_lapsed":true,"push_badge_earned":false,"rankings_opted_out":true,"network_visible":false,"volunteer":true,"volunteer_interests":["` + strings.Repeat("i", 61) + `"]}`, false, 422},
		{"POST", "/donor/me/preferences", `{"version":0,"referral_emails":true,"marketing_emails":false,"push_referral_converted":true,"push_streak_at_risk":true,"push_streak_milestone":false,"push_streak_lapsed":true,"push_badge_earned":false,"rankings_opted_out":true,"network_visible":false,"volunteer":true,"volunteer_interests":["i0","i1","i2","i3","i4","i5","i6","i7","i8","i9","i10","i11","i12","i13","i14","i15","i16","i17","i18","i19","i20"]}`, false, 422},
		{"POST", "/donor/me/sharing", `{"public_name":"Sam"}`, false, 503},
		{"POST", "/donor/me/sharing", `{"public_name":""}`, false, 422},
		{"POST", "/donor/me/sharing", `{"public_name":"` + strings.Repeat("n", 81) + `"}`, false, 422},
		{"POST", "/donor/me/sharing", `{"public_name":"Sam","replace_code":null}`, false, 422},
		{"POST", "/donor/me/email-change", `{"email":"` + strings.Repeat("e", 321) + `"}`, false, 422},
		{"POST", "/donor/email-change/confirm", `{"token":"short"}`, false, 422},
		{"POST", "/donor/login", `{}`, false, 422},
		{"POST", "/donor/claim", `{"token":"short"}`, false, 422},
		{"POST", "/donor/referrals/code", `{"kind":"QR"}`, false, 422},
		{"GET", "/donor/me/gifts?offset=50", "", false, 503},
		{"GET", "/donor/me/conversions?since=2026-10-01T12:00:00.5Z", "", false, 503},
		{"GET", "/donor/me/conversions?since=yesterday", "", false, 422},
		{"GET", "/donor/me/gifts?offset=-1", "", false, 422},
		{"GET", "/donor/me/receipts/not-a-uuid", "", false, 404},
		{"GET", "/donor/me/annual-summary/1999", "", false, 422},
		{"POST", "/donor/me/recurring/cancel", `{"subscription_id":"` + testID + `","period_end":"2026-11-01T00:00:00Z"}`, true, 503},
		{"POST", "/donor/me/recurring/cancel", `{"subscription_id":"` + testID + `","period_end":"2026-11-01T00:00:00Z"}`, false, 422},
		{"POST", "/donor/me/recurring/cancel", `{"subscription_id":"` + testID + `"}`, true, 422},
		{"POST", "/donor/me/recurring/amount", `{"subscription_id":"` + testID + `","amount_minor":99,"previous_minor":3000,"effective_at":"2026-11-01T00:00:00Z"}`, true, 422},
		{"POST", "/donor/me/recurring/amount", `{"subscription_id":"` + testID + `","amount_minor":4500,"previous_minor":3000,"effective_at":"2026-11-01T00:00:00Z"}`, true, 422},
		{"POST", "/donor/me/recurring/amount", `{"subscription_id":"` + testID + `","amount_minor":9000,"previous_minor":4500,"effective_at":"2026-11-01T00:00:00Z"}`, true, 503},
		{"POST", "/donor/me/payment-method/setup", "", true, 503},
		{"POST", "/donor/me/payment-method/setup", "", false, 422},
		{"POST", "/donor/staff/sign-in/options", "", false, 503},
		{"POST", "/donor/staff/sign-in", `{"ceremony":"` + strings.Repeat("c", 43) + `","credential":{"id":"x"}}`, false, 503},
		{"POST", "/donor/staff/sign-in", `{"ceremony":"short","credential":{"id":"x"}}`, false, 422},
		{"POST", "/donor/staff/sign-in", `{"ceremony":"` + strings.Repeat("c", 43) + `"}`, false, 422},
		{"POST", "/donor/staff/sign-in", `{"ceremony":"` + strings.Repeat("c", 43) + `","credential":"` + strings.Repeat("x", 65<<10) + `"}`, false, 422},
		{"POST", "/donor/staff/sign-in", `{"email":"staff@example.org","password":"secret","code":"123456"}`, false, 422},
		{"POST", "/donor/staff/activate/options", `{"token":"short"}`, false, 422},
		{"POST", "/donor/staff/activate", `{"ceremony":"` + strings.Repeat("c", 43) + `","credential":{"id":"\u0000"}}`, false, 422},
		{"POST", "/donor/staff/accounts", `{"email":"staff@example.org","name":"Sam","role":"OWNER"}`, false, 422},
		{"POST", "/donor/staff/accounts/not-a-uuid/disable", "", false, 404},
		{"POST", "/donor/staff/accounts/" + testID + "/role", `{"role":"SUPER_ADMIN"}`, false, 503},
		{"POST", "/donor/staff/accounts/" + testID + "/role", `{"role":"OWNER"}`, false, 422},
		{"GET", "/donor/staff/donors?cursor=" + strings.Repeat("c", 601), "", false, 422},
		{"GET", "/donor/staff/donors?q=" + strings.Repeat("q", 321), "", false, 422},
		{"GET", "/donor/staff/donors?q=a%00b", "", false, 422},
		{"GET", "/donor/staff/donors/not-a-uuid", "", false, 404},
		{"POST", "/donor/staff/donors/" + testID + "/gifts/not-a-uuid/refund", `{"amount_minor":100,` + reason + `}`, true, 404},
		{"GET", "/donor/referrals/code", "", false, 503},
		{"GET", "/donor/referrals/%00%27%22", "", false, 404},
		{"POST", "/donor/referrals/%00%27%22", `{}`, false, 404},
		{"POST", "/donor/me/sharing", `{"public_name":"Sam\u0000"}`, false, 422},
		{"POST", "/donor/checkouts", `{"email":"donor@example.org","amount_minor":3000,"frequency":"MONTHLY\u0000"}`, true, 422},
		{"POST", "/donor/staff/content/sources?name=a%00b", "data", false, 422},
		{"GET", "/donor/staff/donors/" + testID + "?gifts_offset=-5", "", false, 422},
		{"GET", "/donor/staff/donors/" + testID + "/actions/pending?target_id=" + testID + "&kind=REFUND", "", false, 503},
		{"GET", "/donor/staff/donors/" + testID + "/actions/pending?kind=REFUND", "", false, 422},
		{"POST", "/donor/staff/messages/" + testID + "/resend", "", false, 422},
		{"POST", "/donor/staff/donors/" + testID + "/gifts/" + testID + "/refund", `{"amount_minor":100,` + reason + `}`, true, 503},
		{"POST", "/donor/staff/donors/" + testID + "/gifts/" + testID + "/refund", `{"amount_minor":100,` + reason + `}`, false, 422},
		{"POST", "/donor/staff/donors/" + testID + "/gifts/" + testID + "/refund", `{"amount_minor":0,` + reason + `}`, true, 422},
		{"POST", "/donor/staff/donors/" + testID + "/gifts/" + testID + "/refund", `{"amount_minor":100,"reason":"no"}`, true, 422},
		{"POST", "/donor/staff/donors/" + testID + "/cancellation", `{"subscription_id":"` + testID + `",` + reason + `}`, true, 422},
		{"GET", "/donor/staff/exports/jobs/not-a-uuid", "", false, 404},
		{"GET", "/donor/staff/exports/jobs/not-a-uuid/file", "", false, 404},
		{"GET", "/donor/staff/exports/gifts.csv", "", false, 404},
		{"POST", "/donor/staff/exports/jobs", `{"dataset":"gifts","extra":1}`, false, 422},
		{"POST", "/donor/staff/exports/jobs", `{"dataset":"gifts"}`, false, 503},
		{"GET", "/donor/staff/content/sources/not-a-uuid", "", false, 404},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Origin", originFor(tc.path))
		r.AddCookie(&http.Cookie{Name: donorCookie, Value: "session"})
		r.AddCookie(&http.Cookie{Name: staffCookie, Value: "session"})
		if tc.key {
			r.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		NewServer("", Services{EmailConfigured: true}).Handler.ServeHTTP(response, r)
		if response.Code != tc.status {
			t.Errorf("%s %s %s (key %v): got %d %s, want %d", tc.method, tc.path, tc.body, tc.key, response.Code, response.Body, tc.status)
		}
	}
}
