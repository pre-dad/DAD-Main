package push

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegistrationPlatformsDoesNotLoadDeliveryKeys(t *testing.T) {
	getenv := func(name string) string {
		return map[string]string{"DONOR_PUSH_PLATFORMS": ",IOS,ANDROID,IOS", "DONOR_PUSH_APNS_KEY_FILE": "/nonexistent/apns.p8", "DONOR_PUSH_FCM_KEY_FILE": "/nonexistent/firebase.json"}[name]
	}
	got, err := RegistrationPlatforms(getenv)
	if err != nil || !reflect.DeepEqual(got, []string{IOS, Android}) {
		t.Fatalf("registration loaded private credentials: %v %v", got, err)
	}
	if _, err := FromEnvironment(getenv); err == nil {
		t.Fatal("delivery accepted missing private keys")
	}
	if _, err := RegistrationPlatforms(func(string) string { return "WEB" }); err == nil {
		t.Fatal("unknown registration platform accepted")
	}
	if got, err := RegistrationPlatforms(func(string) string { return "none" }); err != nil || len(got) != 0 {
		t.Fatalf("disabled registration: %v %v", got, err)
	}
}

var notification = Notification{
	ID: "5f0c6b1e-8d2a-4c3b-9e4f-1a2b3c4d5e6f", Category: "REFERRAL_CONVERTED",
	Title: "A Link Referral just gave", Body: "Someone you invited made their first gift.",
	Path: "/account/dashboard", Expires: time.Unix(1_900_000_000, 0),
}

func pkcs8(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// decodeJWT splits a compact JWS and returns its header, claims, signing
// input and signature.
func decodeJWT(t *testing.T, token string) (header, claims map[string]any, digest, signature []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	for i, target := range []*map[string]any{&header, &claims} {
		data, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(data, target) != nil {
			t.Fatalf("token part %d is not base64url JSON", i)
		}
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	return header, claims, sum[:], signature
}

type apnsCall struct {
	path, authorization string
	header              http.Header
	body                map[string]any
	proto               int
}

func apnsServer(t *testing.T, status int, reason string) (*httptest.Server, *[]apnsCall) {
	t.Helper()
	var calls []apnsCall
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, apnsCall{r.URL.Path, r.Header.Get("Authorization"), r.Header.Clone(), body, r.ProtoMajor})
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "30")
		}
		w.WriteHeader(status)
		if reason != "" {
			_, _ = io.WriteString(w, `{"reason":"`+reason+`"}`)
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &calls
}

func newTestAPNs(t *testing.T, server *httptest.Server) (*APNs, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAPNs(pkcs8(t, key), "ABC123DEFG", "TEAM123456", "org.dad.donor")
	if err != nil {
		t.Fatal(err)
	}
	a.Client, a.ProductionURL, a.SandboxURL = server.Client(), server.URL+"/production", server.URL+"/sandbox"
	return a, key
}

func TestAPNsSendsSignedAlertsOverHTTP2(t *testing.T) {
	server, calls := apnsServer(t, http.StatusOK, "")
	a, key := newTestAPNs(t, server)
	clock := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return clock }
	token := strings.Repeat("ab", 32)
	if err := a.Send(context.Background(), Device{Token: token, Environment: "PRODUCTION"}, notification); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(context.Background(), Device{Token: token, Environment: "SANDBOX"}, notification); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0].path != "/production/3/device/"+token || (*calls)[1].path != "/sandbox/3/device/"+token {
		t.Fatalf("calls %+v", *calls)
	}
	first := (*calls)[0]
	if first.proto != 2 {
		t.Fatalf("APNs requires HTTP/2, got HTTP/%d", first.proto)
	}
	for name, want := range map[string]string{
		"apns-topic": "org.dad.donor", "apns-push-type": "alert", "apns-priority": "10",
		"apns-collapse-id": notification.ID, "apns-expiration": "1900000000", "Content-Type": "application/json",
	} {
		if got := first.header.Get(name); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	want := map[string]any{
		"aps":      map[string]any{"alert": map[string]any{"title": notification.Title, "body": notification.Body}, "sound": "default"},
		"category": "REFERRAL_CONVERTED", "path": "/account/dashboard",
	}
	if !reflect.DeepEqual(first.body, want) {
		t.Fatalf("payload %v, want %v", first.body, want)
	}
	if (*calls)[1].authorization != first.authorization {
		t.Fatal("a fresh provider token was signed for each send")
	}
	bearer, ok := strings.CutPrefix(first.authorization, "bearer ")
	if !ok {
		t.Fatalf("authorization %q", first.authorization)
	}
	header, claims, digest, signature := decodeJWT(t, bearer)
	if !reflect.DeepEqual(header, map[string]any{"alg": "ES256", "kid": "ABC123DEFG"}) ||
		!reflect.DeepEqual(claims, map[string]any{"iss": "TEAM123456", "iat": float64(clock.Unix())}) {
		t.Fatalf("header %v claims %v", header, claims)
	}
	if len(signature) != 64 ||
		!ecdsa.Verify(&key.PublicKey, digest, new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("provider token signature does not verify with the key's public half")
	}

	clock = clock.Add(apnsTokenLifetime)
	if err := a.Send(context.Background(), Device{Token: token, Environment: "PRODUCTION"}, notification); err != nil {
		t.Fatal(err)
	}
	if (*calls)[2].authorization == first.authorization {
		t.Fatal("provider token was not renewed after its lifetime")
	}
}

func TestAPNsMapsResponses(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
		want   error
	}{
		{http.StatusGone, "Unregistered", ErrUnregistered},
		{http.StatusBadRequest, "BadDeviceToken", ErrUnregistered},
		{http.StatusBadRequest, "DeviceTokenNotForTopic", ErrRejected},
		{http.StatusBadRequest, "PayloadTooLarge", ErrRejected},
		{http.StatusForbidden, "InvalidProviderToken", ErrRejected},
		{http.StatusForbidden, "ExpiredProviderToken", Unavailable{}},
		{http.StatusTooManyRequests, "TooManyRequests", Unavailable{}},
		{http.StatusInternalServerError, "InternalServerError", Unavailable{}},
		{http.StatusServiceUnavailable, "ServiceUnavailable", Unavailable{After: 30 * time.Second}},
	} {
		server, calls := apnsServer(t, tc.status, tc.reason)
		a, _ := newTestAPNs(t, server)
		device := Device{Token: strings.Repeat("cd", 32), Environment: "PRODUCTION"}
		if err := a.Send(context.Background(), device, notification); !errors.Is(err, tc.want) {
			t.Errorf("%d %s: got %v, want %v", tc.status, tc.reason, err, tc.want)
		}
		if tc.status == http.StatusForbidden {
			_ = a.Send(context.Background(), device, notification)
			if (*calls)[0].authorization == (*calls)[1].authorization {
				t.Errorf("%s: the refused provider token was used again", tc.reason)
			}
		}
	}
}

type fcmServer struct {
	*httptest.Server
	exchanges   []url.Values
	sends       []map[string]any
	authorized  []string
	tokenStatus int
	sendStatus  int
	sendReply   string
}

func newFCMServer(t *testing.T) *fcmServer {
	t.Helper()
	s := &fcmServer{tokenStatus: http.StatusOK, sendStatus: http.StatusOK, sendReply: `{"name":"projects/dad-donor-app/messages/1"}`}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			s.exchanges = append(s.exchanges, r.PostForm)
			w.WriteHeader(s.tokenStatus)
			_, _ = io.WriteString(
				w,
				`{"access_token":"access-`+string(rune('0'+len(s.exchanges)))+`","expires_in":3599,"token_type":"Bearer"}`,
			)
		case "/v1/projects/dad-donor-app/messages:send":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.sends = append(s.sends, body)
			s.authorized = append(s.authorized, r.Header.Get("Authorization"))
			if s.sendStatus == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "30")
			}
			w.WriteHeader(s.sendStatus)
			_, _ = io.WriteString(w, s.sendReply)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func serviceAccount(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "dad-donor-app", "private_key": string(pkcs8(t, key)),
		"client_email": "push@dad-donor-app.iam.gserviceaccount.com", "token_uri": "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var rsaKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

func newTestFCM(t *testing.T, server *fcmServer) *FCM {
	t.Helper()
	f, err := NewFCM(serviceAccount(t, rsaKey))
	if err != nil {
		t.Fatal(err)
	}
	f.Client, f.TokenURL, f.SendURL = server.Client(), server.URL+"/token", server.URL
	return f
}

func TestFCMExchangesASignedAssertionAndCachesTheAccessToken(t *testing.T) {
	server := newFCMServer(t)
	f := newTestFCM(t, server)
	clock := time.Unix(1_899_996_400, 0)
	f.now = func() time.Time { return clock }
	device := Device{Token: "fcm-token:APA91b" + strings.Repeat("x", 140)}
	for range 2 {
		if err := f.Send(context.Background(), device, notification); err != nil {
			t.Fatal(err)
		}
	}
	if len(server.exchanges) != 1 || server.authorized[0] != "Bearer access-1" || server.authorized[1] != "Bearer access-1" {
		t.Fatalf(
			"exchanges %d, authorizations %v: the access token should be exchanged once and reused",
			len(server.exchanges),
			server.authorized,
		)
	}
	form := server.exchanges[0]
	if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant type %q", form.Get("grant_type"))
	}
	header, claims, digest, signature := decodeJWT(t, form.Get("assertion"))
	wantClaims := map[string]any{
		"iss": "push@dad-donor-app.iam.gserviceaccount.com", "scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud": server.URL + "/token", "iat": float64(clock.Unix()), "exp": float64(clock.Add(time.Hour).Unix()),
	}
	if !reflect.DeepEqual(header, map[string]any{"alg": "RS256", "typ": "JWT"}) || !reflect.DeepEqual(claims, wantClaims) {
		t.Fatalf("header %v claims %v", header, claims)
	}
	if rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, digest, signature) != nil {
		t.Fatal("assertion signature does not verify with the service account's public key")
	}
	want := map[string]any{"message": map[string]any{
		"token":        device.Token,
		"notification": map[string]any{"title": notification.Title, "body": notification.Body},
		"data":         map[string]any{"category": "REFERRAL_CONVERTED", "path": "/account/dashboard"},
		"android": map[string]any{
			"collapse_key": notification.ID,
			"ttl":          "3600s",
			"notification": map[string]any{"tag": notification.ID},
		},
	}}
	if !reflect.DeepEqual(server.sends[0], want) {
		t.Fatalf("message %v, want %v", server.sends[0], want)
	}

	clock = clock.Add(time.Hour - accessMargin)
	if err := f.Send(context.Background(), device, notification); err != nil {
		t.Fatal(err)
	}
	if len(server.exchanges) != 2 || server.authorized[2] != "Bearer access-2" {
		t.Fatalf("access token was not renewed before expiry: %v", server.authorized)
	}
}

func TestFCMMapsResponses(t *testing.T) {
	const fcmError = `{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"`
	for _, tc := range []struct {
		name        string
		tokenStatus int
		status      int
		reply       string
		want        error
	}{
		{"unregistered", 200, 404, `{"error":{"status":"NOT_FOUND","details":[` + fcmError + `UNREGISTERED"}]}}`, ErrUnregistered},
		{"invalid token", 200, 400, `{"error":{"status":"INVALID_ARGUMENT","details":[` + fcmError + `INVALID_ARGUMENT"},{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.token","description":"Invalid registration token"}]}]}}`, ErrUnregistered},
		{"invalid payload", 200, 400, `{"error":{"status":"INVALID_ARGUMENT","details":[` + fcmError + `INVALID_ARGUMENT"},{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.android.ttl"}]}]}}`, ErrRejected},
		{"sender mismatch", 200, 403, `{"error":{"status":"PERMISSION_DENIED","details":[` + fcmError + `SENDER_ID_MISMATCH"}]}}`, ErrRejected},
		{"quota", 200, 429, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[` + fcmError + `QUOTA_EXCEEDED"}]}}`, Unavailable{After: 30 * time.Second}},
		{"outage", 200, 503, `{"error":{"status":"UNAVAILABLE"}}`, Unavailable{}},
		{"expired access", 200, 401, `{"error":{"status":"UNAUTHENTICATED"}}`, Unavailable{}},
		{"refused service account", 400, 200, ``, ErrRejected},
		{"token endpoint outage", 503, 200, ``, Unavailable{}},
	} {
		server := newFCMServer(t)
		server.tokenStatus, server.sendStatus, server.sendReply = tc.tokenStatus, tc.status, tc.reply
		f := newTestFCM(t, server)
		err := f.Send(context.Background(), Device{Token: "fcm-token"}, notification)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if tc.status == http.StatusUnauthorized {
			_ = f.Send(context.Background(), Device{Token: "fcm-token"}, notification)
			if len(server.exchanges) != 2 {
				t.Errorf("%s: the refused access token was used again", tc.name)
			}
		}
	}
}

func TestFromEnvironmentLoadsOnlyCompleteProviders(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p8, account := write("AuthKey.p8", pkcs8(t, ecKey)), write("firebase.json", serviceAccount(t, rsaKey))
	apns := map[string]string{
		"DONOR_PUSH_APNS_KEY_FILE": p8,
		"DONOR_PUSH_APNS_KEY_ID":   "ABC123DEFG",
		"DONOR_PUSH_APNS_TEAM_ID":  "TEAM123456",
		"DONOR_PUSH_APNS_TOPIC":    "org.dad.donor",
	}
	with := func(base map[string]string, changes ...string) func(string) string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for i := 0; i < len(changes); i += 2 {
			env[changes[i]] = changes[i+1]
		}
		return func(name string) string { return env[name] }
	}

	config, err := FromEnvironment(with(nil))
	if err != nil || len(config.Providers()) != 0 {
		t.Fatalf("unconfigured: %v %v", config, err)
	}
	config, err = FromEnvironment(with(apns, "DONOR_PUSH_FCM_KEY_FILE", account))
	if err != nil || config.APNs == nil || config.FCM == nil || len(config.Providers()) != 2 {
		t.Fatalf("configured: %v %v", config, err)
	}
	for name, getenv := range map[string]func(string) string{
		"APNs without a key ID":   with(apns, "DONOR_PUSH_APNS_KEY_ID", ""),
		"APNs with a bad topic":   with(apns, "DONOR_PUSH_APNS_TOPIC", "donor app"),
		"APNs with an RSA key":    with(apns, "DONOR_PUSH_APNS_KEY_FILE", write("rsa.p8", pkcs8(t, rsaKey))),
		"APNs with a missing key": with(apns, "DONOR_PUSH_APNS_KEY_FILE", filepath.Join(dir, "absent.p8")),
		"FCM with a non-account":  with(nil, "DONOR_PUSH_FCM_KEY_FILE", write("other.json", []byte(`{"type":"authorized_user"}`))),
		"FCM with an EC key":      with(nil, "DONOR_PUSH_FCM_KEY_FILE", p8),
	} {
		if _, err := FromEnvironment(getenv); !errors.Is(err, ErrConfiguration) {
			t.Errorf("%s: got %v, want a configuration error", name, err)
		}
	}
}

func TestAPNsKeepsHostileTokensInsideTheDevicePath(t *testing.T) {
	server, calls := apnsServer(t, http.StatusOK, "")
	a, _ := newTestAPNs(t, server)
	const hostile = "../../2/x?y=1#z"
	if err := a.Send(context.Background(), Device{Token: hostile, Environment: "PRODUCTION"}, notification); err != nil {
		t.Fatal(err)
	}
	// Unescaped, the dots would be resolved and the query and fragment split off.
	if len(*calls) != 1 || (*calls)[0].path != "/production/3/device/"+hostile {
		t.Fatalf("device token escaped its path segment: %+v", *calls)
	}
}

// A dead network or a cancelled send is no verdict on the device: only the
// provider's answer may drop a token.
func TestProviderTransportFailuresNeverUnregisterTheDevice(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	apnsServer, _ := apnsServer(t, http.StatusOK, "")
	a, _ := newTestAPNs(t, apnsServer)
	fcmServer := newFCMServer(t)
	f := newTestFCM(t, fcmServer)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	a.ProductionURL, f.SendURL = dead.URL, dead.URL

	for name, send := range map[string]func(context.Context) error{
		"APNs": func(ctx context.Context) error {
			return a.Send(ctx, Device{Token: strings.Repeat("ab", 32), Environment: "PRODUCTION"}, notification)
		},
		"FCM": func(ctx context.Context) error { return f.Send(ctx, Device{Token: "fcm-token"}, notification) },
	} {
		for label, ctx := range map[string]context.Context{"unreachable": context.Background(), "cancelled": cancelled} {
			err := send(ctx)
			if err == nil || errors.Is(err, ErrUnregistered) || errors.Is(err, ErrRejected) {
				t.Errorf("%s %s: %v; want a retryable failure that keeps the device", name, label, err)
			}
		}
	}
}
