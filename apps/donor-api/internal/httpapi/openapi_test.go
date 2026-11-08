package httpapi

import (
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/store"
)

// The contract is JSON rather than YAML so it can be checked with the
// standard library alone.
const contractPath = "../../api/openapi.json"

type contract struct {
	Paths      map[string]map[string]contractOperation `json:"paths"`
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
	raw any
}

type contractOperation struct {
	Security    *[]map[string][]string `json:"security"`
	RateLimit   string                 `json:"x-rate-limit"`
	Clients     []string               `json:"x-clients"`
	RequestBody struct {
		Content map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

func loadContract(t *testing.T) *contract {
	t.Helper()
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var c contract
	if err = json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v", contractPath, err)
	}
	if err = json.Unmarshal(data, &c.raw); err != nil {
		t.Fatal(err)
	}
	return &c
}

// operations returns the documented operations keyed like ServeMux patterns.
func (c *contract) operations() map[string]contractOperation {
	ops := map[string]contractOperation{}
	for path, item := range c.Paths {
		for method, op := range item {
			ops[strings.ToUpper(method)+" "+path] = op
		}
	}
	return ops
}

func TestContractListsExactlyTheServedRoutes(t *testing.T) {
	documented := slices.Sorted(maps.Keys(loadContract(t).operations()))
	var served []string
	for _, route := range allRoutes() {
		served = append(served, route.pattern)
	}
	slices.Sort(served)
	for _, pattern := range served {
		if _, ok := slices.BinarySearch(documented, pattern); !ok {
			t.Errorf("%s is served but not documented in %s", pattern, contractPath)
		}
	}
	for _, pattern := range documented {
		if _, ok := slices.BinarySearch(served, pattern); !ok {
			t.Errorf("%s is documented but not served", pattern)
		}
	}
}

func TestContractSecurityMatchesRouteAuth(t *testing.T) {
	schemes := map[auth][]string{
		applicationAuth: {"applicationBearerAuth", "applicationCookieAuth"},
		noAuth:          {},
		donorAuth:       {"bearerAuth", "cookieAuth"},
		partnerAuth:     {"partnerBearerAuth", "partnerCookieAuth"},
		staffAuth:       {"staffCookieAuth"},
		stripeAuth:      {"stripeSignature"},
	}
	ops := loadContract(t).operations()
	for _, route := range allRoutes() {
		op, ok := ops[route.pattern]
		if !ok {
			continue
		}
		if op.Security == nil {
			t.Errorf("%s: security is not declared; use [] for anonymous routes", route.pattern)
			continue
		}
		var got []string
		for _, requirement := range *op.Security {
			got = slices.AppendSeq(got, maps.Keys(requirement))
		}
		slices.Sort(got)
		if want := schemes[route.auth]; !slices.Equal(got, want) {
			t.Errorf("%s: documented security %v, want %v for %s auth", route.pattern, got, want, route.auth)
		}
	}
}

// Documented clients must match what the guard admits: only routes listing
// native accept a request from the app, and every mutation names its limit.
func TestContractClientsMatchTheGuard(t *testing.T) {
	server := NewServer("", Services{})
	for pattern, op := range loadContract(t).operations() {
		mutation := !strings.HasPrefix(pattern, "GET ") && pattern != "POST /donor/stripe/webhook"
		if mutation != (op.RateLimit != "") || mutation != (len(op.Clients) > 0) {
			t.Errorf("%s: x-rate-limit %q and x-clients %v must be set exactly on guarded mutations", pattern, op.RateLimit, op.Clients)
		}
		if !mutation {
			continue
		}
		r := routeRequest(pattern, "{}")
		r.Header.Set("X-DAD-Client", "native")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		if admitted := response.Code != http.StatusForbidden; admitted != slices.Contains(op.Clients, "native") {
			t.Errorf("%s: app request got %d, but x-clients is %v", pattern, response.Code, op.Clients)
		}
	}
}

// Statuses the typed wrappers produce before any handler runs must be
// documented: an anonymous request, a foreign origin, a cross-site staff read,
// an app-only route reached from a browser, and an exhausted rate budget.
func TestContractDocumentsWrapperStatuses(t *testing.T) {
	ops := loadContract(t).operations()
	for _, route := range allRoutes() {
		op, ok := ops[route.pattern]
		if !ok || route.auth == stripeAuth {
			continue
		}
		if route.auth != noAuth && route.auth != staffAuth && route.pattern != "POST /donor/logout" {
			// Reads take no limiter here, but the ingress rejects floods before the API.
			for _, status := range []string{"401", "429"} {
				if _, documented := op.Responses[status]; !documented {
					t.Errorf("%s: authenticated route does not document %s", route.pattern, status)
				}
			}
		}
		origin := originFor(route.pattern)
		probes := map[string]func(*http.Request){
			"browser": func(r *http.Request) { r.Header.Set("Origin", origin) },
			"foreign": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.org") },
			"app":     func(r *http.Request) { r.Header.Set("X-DAD-Client", "native") },
			"cross-site": func(r *http.Request) {
				r.Header.Set("Origin", origin)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			},
		}
		server := NewServer("", Services{})
		for name, mark := range probes {
			rounds := 1
			if name == "browser" || name == "app" {
				rounds = 130
			}
			reported := false
			for range rounds {
				if reported {
					break
				}
				r := routeRequest(route.pattern, "{}")
				mark(r)
				response := httptest.NewRecorder()
				server.Handler.ServeHTTP(response, r)
				switch response.Code {
				case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
					if _, documented := op.Responses[strconv.Itoa(response.Code)]; !documented {
						t.Errorf("%s: %s request got %d, which is not documented", route.pattern, name, response.Code)
						reported = true
					}
				}
			}
		}
	}
}

// A path ID that is not a UUID answers 404 from the handler, after the wrapper
// has admitted the caller. Wrappers cannot be probed without a session store,
// so credentials the handler accepts are presented and any 404 seen must be
// documented; routes that stop earlier (401, 503) add nothing to check.
func TestContractDocumentsMalformedPathIDs(t *testing.T) {
	ops := loadContract(t).operations()
	server := NewServer("", Services{})
	var reached int
	for _, route := range allRoutes() {
		op, ok := ops[route.pattern]
		if !ok || route.auth == stripeAuth || !strings.Contains(route.pattern, "_id}") {
			continue
		}
		method, path, _ := strings.Cut(route.pattern, " ")
		path = regexp.MustCompile(`\{[a-z_]+_id\}`).ReplaceAllString(path, "not-a-uuid")
		path = wildcard.ReplaceAllString(path, testID)
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Origin", originFor(route.pattern))
		r.Header.Set("Idempotency-Key", "00000000-0000-4000-8000-000000000001")
		for _, name := range []string{donorCookie, partnerCookie, staffCookie} {
			r.AddCookie(&http.Cookie{Name: name, Value: strings.Repeat("s", 43)})
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, r)
		if response.Code != http.StatusNotFound {
			continue
		}
		reached++
		if _, documented := op.Responses["404"]; !documented {
			t.Errorf("%s: a malformed ID got 404, which is not documented", route.pattern)
		}
	}
	if reached == 0 {
		t.Fatal("no path-ID route answered 404; the probe no longer reaches the handlers")
	}
}

func TestContractReferencesResolve(t *testing.T) {
	c := loadContract(t)
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if target, ok := v["$ref"].(string); ok && c.lookup(target) == nil {
				t.Errorf("unresolved $ref %s", target)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(c.raw)
}

func (c *contract) lookup(pointer string) any {
	node := c.raw
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "#/"), "/") {
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = object[part]
	}
	return node
}

// Each JSON body the API decodes or encodes must have exactly the documented
// properties, recursively, so a renamed or added field fails here first.
func TestContractSchemasMatchGoTypes(t *testing.T) {
	responses := map[string]any{
		"GET /donor/partner/application-status":                      store.ApplicationStatus{},
		"GET /donor/public/spend":                                    store.OverallSpend{},
		"POST /donor/checkouts":                                      payments.CheckoutResult{},
		"GET /donor/me/dashboard":                                    store.DonorDashboard{},
		"GET /donor/me/profile":                                      store.Profile{},
		"POST /donor/me/preferences":                                 store.Profile{},
		"POST /donor/partner/push-device":                            store.PushRegistration{},
		"POST /donor/partner/push-device/unregister":                 store.PushRegistration{},
		"POST /donor/me/push-device":                                 store.PushRegistration{},
		"POST /donor/me/push-device/unregister":                      store.PushRegistration{},
		"GET /donor/me/conversions":                                  store.Conversions{},
		"GET /donor/me/sessions":                                     store.Sessions{},
		"POST /donor/me/sessions/{session_id}/revoke":                store.Sessions{},
		"POST /donor/me/sessions/revoke-others":                      store.Sessions{},
		"GET /donor/me/sharing":                                      store.Sharing{},
		"POST /donor/me/sharing":                                     store.Sharing{},
		"GET /donor/me/gifts":                                        store.GiftsPage{},
		"GET /donor/me/receipts/{gift_id}":                           store.Receipt{},
		"GET /donor/me/annual-summary/{year}":                        store.AnnualSummary{},
		"GET /donor/me/recurring":                                    store.RecurringDetails{},
		"GET /donor/me/recurring/amount":                             store.AmountOptions{},
		"GET /donor/me/payment-method":                               store.PaymentMethodStatus{},
		"POST /donor/me/recurring/checkout":                          payments.NativeCheckoutResult{},
		"POST /donor/me/recurring/cancel":                            store.RecurringDetails{},
		"POST /donor/me/recurring/amount":                            store.AmountChange{},
		"POST /donor/me/payment-method/setup":                        store.PaymentUpdate{},
		"POST /donor/passkeys/sign-in/options":                       store.Ceremony{},
		"GET /donor/me/passkeys":                                     store.PasskeyList{},
		"POST /donor/me/passkeys/register/options":                   store.Ceremony{},
		"POST /donor/me/passkeys/register":                           store.PasskeyList{},
		"POST /donor/me/passkeys/{passkey_id}/delete":                store.PasskeyList{},
		"POST /donor/staff/sign-in/options":                          store.Ceremony{},
		"POST /donor/staff/activate/options":                         store.Invitation{},
		"GET /donor/staff/me":                                        store.StaffView{},
		"GET /donor/staff/accounts":                                  []store.StaffView{},
		"POST /donor/staff/accounts/{staff_id}/disable":              store.AccessChange{},
		"POST /donor/staff/accounts/{staff_id}/role":                 store.StaffView{},
		"GET /donor/staff/donors":                                    store.DonorSearch{},
		"GET /donor/staff/donors/{donor_id}":                         store.DonorDetail{},
		"GET /donor/staff/donors/{donor_id}/messages":                store.MessagePage{},
		"GET /donor/staff/donors/{donor_id}/actions":                 store.ActionPage{},
		"GET /donor/staff/donors/{donor_id}/actions/pending":         store.PendingAction{},
		"GET /donor/staff/audit":                                     store.AuditPage{},
		"GET /donor/staff/failed-payments":                           store.FailedPaymentPage{},
		"GET /donor/staff/deletions":                                 store.AccountDeletionPage{},
		"POST /donor/staff/messages/{message_id}/resend":             store.QueuedMessage{},
		"GET /donor/staff/donors/{donor_id}/gifts/{gift_id}/refund":  store.RefundQuote{},
		"POST /donor/staff/donors/{donor_id}/gifts/{gift_id}/refund": store.StaffRefund{},
		"GET /donor/staff/donors/{donor_id}/cancellation":            store.RecurringDetails{},
		"POST /donor/staff/donors/{donor_id}/cancellation":           store.StaffCancellation{},
		"GET /donor/staff/exports":                                   store.ExportDatasets{},
		"GET /donor/staff/exports/jobs":                              store.ExportJobs{},
		"POST /donor/staff/exports/jobs":                             store.ExportJob{},
		"GET /donor/staff/exports/jobs/{job_id}":                     store.ExportJob{},
		"GET /donor/staff/content":                                   store.ContentInventory{},
		"GET /donor/staff/events":                                    store.StaffEventPage{},
		"GET /donor/staff/events/{event_id}":                         store.StaffEventDetail{},
		"GET /donor/staff/initiatives":                               store.StaffInitiativePage{},
		"GET /donor/staff/initiatives/{initiative_id}":               store.StaffInitiativeDetail{},
		"GET /donor/staff/partners":                                  store.StaffPartnerPage{},
		"GET /donor/staff/partners/{partnerID}":                      store.StaffPartnerDetail{},
		"POST /donor/staff/content/sources":                          store.StoredSource{},
		"GET /initiatives/{initiative_id}/spend":                     store.InitiativeSpend{},
		"GET /donor/me/today-initiative":                             store.TodayInitiative{},
		"GET /donor/me/network":                                      store.NetworkSummary{},
		"GET /events/{event_id}":                                     store.EventView{},
		"GET /donor/me/events/{event_id}":                            store.EventView{},
		"POST /donor/me/events/check-in":                             store.CheckInResult{},
		"GET /donor/partner/overview":                                store.PartnerOverview{},
		"GET /donor/partner/our-people":                              store.PartnerOurPeople{},
		"GET /donor/me/team":                                         store.DonorTeam{},
		"GET /donor/me/badges": struct {
			Badges []store.BadgeView `json:"badges"`
		}{},
		"GET /donor/partner/initiatives": struct {
			Initiatives []store.PartnerInitiative `json:"initiatives"`
		}{},
	}
	requests := map[string]any{
		"POST /donor/partner/applications":                           applicationReceiptBody{},
		"POST /donor/staff/donors/{donor_id}/recognition/recompute":  recomputeRecognitionBody{},
		"POST /donor/passkeys/sign-in":                               donorPasskeySignInBody{},
		"POST /donor/me/passkeys/register":                           registerPasskeyBody{},
		"POST /donor/checkouts":                                      checkoutBody{},
		"POST /donor/me/recurring/checkout":                          nativeCheckoutBody{},
		"POST /donor/me/recurring/cancel":                            cancelBody{},
		"POST /donor/me/recurring/amount":                            amountBody{},
		"POST /donor/staff/sign-in":                                  passkeyBody{},
		"POST /donor/staff/activate/options":                         invitationTokenBody{},
		"POST /donor/staff/activate":                                 passkeyBody{},
		"POST /donor/staff/accounts":                                 invitationBody{},
		"POST /donor/staff/accounts/{staff_id}/role":                 roleBody{},
		"POST /donor/staff/donors/{donor_id}/gifts/{gift_id}/refund": refundBody{},
		"POST /donor/staff/donors/{donor_id}/cancellation":           staffCancelBody{},
		"POST /donor/me/events/check-in":                             checkInBody{},
		"POST /donor/staff/events":                                   store.EventInput{},
		"POST /donor/staff/events/{event_id}":                        store.EventInput{},
		"POST /donor/public/partner-apply":                           store.PartnerApplicationInput{},
	}
	c := loadContract(t)
	ops := c.operations()
	for pattern, value := range responses {
		var schema map[string]any
		for status, response := range ops[pattern].Responses {
			if strings.HasPrefix(status, "2") {
				schema = response.Content["application/json"].Schema
			}
		}
		c.checkShape(t, pattern+" response", reflect.TypeOf(value), schema, true)
	}
	for pattern, value := range requests {
		c.checkShape(t, pattern+" request", reflect.TypeOf(value), ops[pattern].RequestBody.Content["application/json"].Schema, false)
	}
	c.checkShape(t, "NativeSession", reflect.TypeOf(nativeSession{}), c.Components.Schemas["NativeSession"], true)
	c.checkShape(t, "Problem", reflect.TypeOf(problem{}), c.Components.Schemas["Problem"], true)
}

// resolve follows $ref and the non-null branch of a nullable anyOf.
func (c *contract) resolve(schema map[string]any) map[string]any {
	for {
		if target, ok := schema["$ref"].(string); ok {
			schema = c.Components.Schemas[strings.TrimPrefix(target, "#/components/schemas/")]
			continue
		}
		alternatives, ok := schema["anyOf"].([]any)
		if !ok {
			return schema
		}
		for _, alternative := range alternatives {
			if branch := alternative.(map[string]any); branch["type"] != "null" {
				schema = branch
			}
		}
	}
}

// opaque types serialize as JSON scalars or free-form values.
func opaque(t reflect.Type) bool {
	if t.PkgPath() == "time" || t.PkgPath() == "github.com/jackc/pgx/v5/pgtype" {
		return true
	}
	return t.Kind() == reflect.Map || t.Kind() == reflect.Interface || (t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8)
}

// For an encoded type, a property is also required exactly when its field is never
// omitted. A decoded field may be optional either way, since absence leaves it zero.
func (c *contract) checkShape(t *testing.T, where string, typ reflect.Type, schema map[string]any, encoded bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if opaque(typ) || (typ.Kind() != reflect.Struct && typ.Kind() != reflect.Slice) {
		return
	}
	if schema == nil {
		t.Errorf("%s: no JSON schema documented for %s", where, typ)
		return
	}
	schema = c.resolve(schema)
	if typ.Kind() == reflect.Slice {
		items, _ := schema["items"].(map[string]any)
		c.checkShape(t, where+"[]", typ.Elem(), items, encoded)
		return
	}
	// A type that encodes differently per state documents each state as a oneOf
	// variant; together the variants must cover exactly its fields.
	properties := map[string]any{}
	variants, _ := schema["oneOf"].([]any)
	for _, variant := range append([]any{schema}, variants...) {
		documented, _ := c.resolve(variant.(map[string]any))["properties"].(map[string]any)
		maps.Copy(properties, documented)
	}
	fields, omitted := map[string]reflect.Type{}, map[string]bool{}
	for _, field := range reflect.VisibleFields(typ) {
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		// encoding/json promotes the fields of an untagged embedded struct.
		if !field.IsExported() || name == "-" || (field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct) {
			continue
		}
		name = cmp.Or(name, field.Name)
		fields[name] = field.Type
		omitted[name] = slices.Contains(strings.Split(options, ","), "omitempty")
	}
	got, want := slices.Sorted(maps.Keys(properties)), slices.Sorted(maps.Keys(fields))
	if !slices.Equal(got, want) {
		t.Errorf("%s: documented properties %v, %s encodes %v", where, got, typ, want)
		return
	}
	// Variants document which fields each state carries, so only they say what is required.
	if required, _ := schema["required"].([]any); encoded && variants == nil {
		for name := range fields {
			if documented := slices.Contains(required, any(name)); documented == omitted[name] {
				t.Errorf("%s.%s: documented required is %v, but omitempty is %v", where, name, documented, omitted[name])
			}
		}
	}
	for name, fieldType := range fields {
		property, _ := properties[name].(map[string]any)
		c.checkShape(t, where+"."+name, fieldType, property, encoded)
	}
}

// The all-in-one content publication was replaced by initiatives; its routes
// must stay unserved and undocumented.
func TestRetiredContentRoutesAreNotServed(t *testing.T) {
	retired := []string{"POST /donor/staff/content/publish", "GET /donor/public/content"}
	documented := loadContract(t).operations()
	for _, route := range allRoutes() {
		if slices.Contains(retired, route.pattern) {
			t.Errorf("%s is served", route.pattern)
		}
	}
	for _, pattern := range retired {
		if _, ok := documented[pattern]; ok {
			t.Errorf("%s is still documented", pattern)
		}
	}
}
