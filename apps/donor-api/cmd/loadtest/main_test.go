package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadCountsOutcomesPerScenario(t *testing.T) {
	var good, bad, cookies atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "dad_donor_session=synthetic" {
			cookies.Add(1)
		}
		switch r.URL.Path {
		case "/ok":
			good.Add(1)
		case "/large":
			bad.Add(1)
			_, _ = w.Write([]byte(strings.Repeat("x", maxBodyBytes+1)))
		default:
			bad.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	scenarios, err := urlMix(server.URL + "/ok," + server.URL + "/unavailable," + server.URL + "/large")
	if err != nil {
		t.Fatal(err)
	}
	r := load(
		context.Background(),
		config{scenarios: scenarios, duration: 200 * time.Millisecond, concurrency: 4, sessions: []string{"dad_donor_session=synthetic"}},
	)
	if r.Requests == 0 || r.Requests != good.Load()+bad.Load() || cookies.Load() != r.Requests {
		t.Fatalf("reported %d requests; server saw %d, %d with the session cookie", r.Requests, good.Load()+bad.Load(), cookies.Load())
	}
	host := strings.TrimPrefix(server.URL, "http://")
	ok, unavailable, large := r.Scenarios[host+"/ok"], r.Scenarios[host+"/unavailable"], r.Scenarios[host+"/large"]
	if r.Failed != bad.Load() || ok.Failed != 0 || unavailable.Failed != unavailable.Requests ||
		large.Errors["oversized"] != large.Requests {
		t.Fatalf("failures misattributed: %+v", r)
	}
	if ok.Statuses["200"] != ok.Requests || unavailable.Statuses["503"] != unavailable.Requests || ok.Requests != good.Load() {
		t.Fatalf("statuses misreported: %+v %+v", ok, unavailable)
	}
	if ok.Requests == 0 || unavailable.Requests == 0 || large.Requests == 0 {
		t.Fatal("the mix did not reach every scenario")
	}
	if r.P50ms > r.P95ms || r.P95ms > r.P99ms || r.P99ms > r.MaxMs {
		t.Fatalf("percentiles out of order: %+v", r.stats)
	}
}

// TestSeededMixActsLikeBrowsersAppsAndVisitors checks each request carries
// exactly one kind of credential and that captures look like distinct visitors.
func TestSeededMixActsLikeBrowsersAppsAndVisitors(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	var visitorAddresses, userAddresses = map[string]bool{}, map[string]bool{}
	var problems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		cookie, bearer := r.Header.Get("Cookie"), r.Header.Get("Authorization")
		switch path := r.URL.Path; {
		case strings.HasPrefix(path, "/donor/me/"):
			seen["signed in"]++
			if native := r.Header.Get("X-DAD-Client") == "native"; native {
				seen["native"]++
				if cookie != "" || !strings.HasPrefix(bearer, "Bearer token-") {
					problems = append(problems, "app request without a bearer token alone")
				}
			} else if !strings.HasPrefix(cookie, "dad_donor_session=token-") || bearer != "" {
				problems = append(problems, "browser request without its session cookie alone")
			}
			userAddresses[r.Header.Get("X-Real-IP")] = true
			if path == "/donor/me/dashboard" {
				problems = append(problems, "a scenario weighted 0 was requested")
			}
		case r.Method == http.MethodPost:
			seen["capture"]++
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Origin") != "https://donor.example.org" || cookie != "" || bearer != "" ||
				!strings.Contains(string(body), `"kind"`) {
				problems = append(problems, "capture without the site's Origin or with credentials")
			}
			visitorAddresses[r.Header.Get("X-Real-IP")] = true
		case strings.HasPrefix(path, "/donor/referrals/"):
			code := strings.TrimPrefix(path, "/donor/referrals/")
			if code == "codeA" || code == "codeB" {
				seen["lookup"]++
			} else {
				seen["missing"]++
				w.WriteHeader(http.StatusNotFound)
				return
			}
		default:
			seen[path]++
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	scenarios, err := seededMix(
		server.URL,
		"https://donor.example.org",
		fixture{ReferralCodes: []string{"codeA", "codeB"}},
		map[string]int{"dashboard": 0},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := load(context.Background(), config{scenarios: scenarios, duration: 300 * time.Millisecond, concurrency: 8,
		sessions: []string{"token-1", "token-2", "token-3"}, nativeShare: 0.5, clientIPs: true})
	if len(problems) > 0 {
		t.Fatal(problems[0])
	}
	if r.Failed != 0 || seen["lookup"] == 0 || seen["missing"] == 0 || seen["capture"] < 2 || seen["/donor/public/spend"] == 0 {
		t.Fatalf("mix incomplete: %v %+v", seen, r.stats)
	}
	if seen["native"] == 0 || seen["native"] == seen["signed in"] {
		t.Fatalf("signed-in users were not split between browser and app: %v", seen)
	}
	if len(visitorAddresses) != seen["capture"] || len(userAddresses) < 2 || visitorAddresses[""] {
		t.Fatalf("simulated clients do not have distinct addresses: %d captures from %d addresses", seen["capture"], len(visitorAddresses))
	}
	if _, ok := r.Scenarios["dashboard"]; ok {
		t.Fatal("a disabled scenario was reported")
	}
}

func TestSeededMixRejectsUnknownScenarios(t *testing.T) {
	f := fixture{ReferralCodes: []string{"code"}}
	if _, err := seededMix("http://api", "http://web", f, map[string]int{"dashbord": 5}); err == nil {
		t.Fatal("misspelt scenario accepted")
	}
	if _, err := seededMix("http://api", "http://web", fixture{}, nil); err == nil {
		t.Fatal("fixture without referral codes accepted")
	}
}

// TestEventScenariosNeedEventIDs covers the event-day mix: the event scenarios
// are refused when the fixture carries no events, and build URLs from a
// fixture event id when it does.
func TestEventScenariosNeedEventIDs(t *testing.T) {
	codes := fixture{ReferralCodes: []string{"code"}}
	for _, name := range []string{"events", "event-detail", "event-reserve"} {
		if _, err := seededMix("http://api", "http://web", codes, map[string]int{name: 5}); err == nil {
			t.Fatalf("scenario %q accepted with no event ids in the fixture", name)
		}
	}
	withEvents := fixture{ReferralCodes: []string{"code"}, EventIDs: []string{"11111111-1111-4111-8111-111111111111"}}
	scenarios, err := seededMix("http://api", "http://web", withEvents, map[string]int{"event-detail": 5, "event-reserve": 3})
	if err != nil {
		t.Fatal(err)
	}
	u := &user{r: rand.New(rand.NewPCG(1, 0))}
	for _, s := range scenarios {
		req := s.build(u)
		switch s.name {
		case "event-detail":
			if req.Method != http.MethodGet || !strings.Contains(req.URL.Path, "11111111-1111-4111-8111-111111111111") {
				t.Fatalf("event-detail did not read the fixture event: %s", req.URL)
			}
		case "event-reserve":
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/reserve") || req.Header.Get("Origin") != "http://web" {
				t.Fatalf("event-reserve is not a reserve POST with the site Origin: %s", req.URL)
			}
		}
	}
}

func TestRunEnforcesPerScenarioBudgets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	targets := server.URL + "/fast?page=1," + server.URL + "/slow"
	args := func(budget string) []string {
		return []string{"-url", targets, "-duration", "200ms", "-concurrency", "2", "-max-p99", budget}
	}
	noEnv := func(string) string { return "" }
	if err := run(context.Background(), args("1s,"+host+"/fast?page=1=1s"), noEnv, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), args("1s,"+host+"/slow=5ms"), noEnv, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "/slow") {
		t.Fatalf("slow scenario passed its budget: %v", err)
	}
	for name, invalid := range map[string][]string{
		"unknown budget":   args("1s," + host + "/missing=5ms"),
		"neither mode":     {"-duration", "1s"},
		"duplicate target": {"-url", server.URL + "/fast," + server.URL + "/fast"},
		"seeded-only -mix": {"-url", targets, "-mix", "dashboard=1"},
		"seeded-only -api": {"-url", targets, "-api", server.URL},
	} {
		if err := run(context.Background(), invalid, noEnv, io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	err := run(context.Background(), args("fast"), noEnv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `invalid p99 budget "fast"`) {
		t.Fatalf("the budget error was not reported: %v", err)
	}
	err = run(context.Background(), []string{"-url", targets, "-origin", "https://donor.example.org"}, noEnv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-origin applies to the seeded mix") {
		t.Fatalf("-origin was ignored with -url: %v", err)
	}
}

// TestInterruptedRunReportsWhatCompleted cancels a run while requests are in
// flight: they are neither failures nor latencies, and the partial result is
// still written.
func TestInterruptedRunReportsWhatCompleted(t *testing.T) {
	var served atomic.Int64
	inFlight := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n := served.Add(1); {
		case n == 24:
			close(inFlight)
			fallthrough
		case n > 20:
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-inFlight:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	var out strings.Builder
	err := run(ctx, []string{"-url", server.URL + "/", "-duration", "1m", "-concurrency", "4"}, func(string) string { return "" }, &out)
	var r result
	if jsonErr := json.Unmarshal([]byte(out.String()), &r); jsonErr != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run: %v, result %q", err, out.String())
	}
	if r.Requests != 20 || r.Failed != 0 {
		t.Fatalf("interrupted requests were counted: %+v", r.stats)
	}
}

func TestRunReadsTheSeedFixture(t *testing.T) {
	var signedIn atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "dad_donor_session=fixture-token" || r.Header.Get("Authorization") == "Bearer fixture-token" {
			signedIn.Add(1)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "fixture.json")
	data, _ := json.Marshal(fixture{Sessions: []string{"fixture-token"}, ReferralCodes: []string{"code"}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	args := []string{
		"-api",
		server.URL,
		"-fixture",
		path,
		"-mix",
		"profile=1,referral-lookup=0,referral-capture=0,referral-missing=0,spend=0,dashboard=0,gifts=0,sharing=0,recurring=0,annual-summary=0",
		"-duration",
		"100ms",
		"-concurrency",
		"2",
	}
	if err := run(context.Background(), args, func(string) string { return "" }, &out); err != nil {
		t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal([]byte(out.String()), &r); err != nil || r.Scenarios["profile"].Requests == 0 ||
		r.Scenarios["profile"].Requests != signedIn.Load() {
		t.Fatalf("fixture sessions not used: %s %v", out.String(), err)
	}
}

func TestParsersRejectUnsafeInput(t *testing.T) {
	if targets, err := parseTargets("http://127.0.0.1:8002/a, https://dad.example/b"); err != nil || len(targets) != 2 {
		t.Fatalf("valid targets rejected: %v %v", targets, err)
	}
	for _, invalid := range []string{"", "ftp://host/x", "http://user:pass@host/x", "/relative"} {
		if _, err := parseTargets(invalid); err == nil {
			t.Errorf("target %q accepted", invalid)
		}
	}
	if b, err := parseBudgets("250ms,dashboard=400ms"); err != nil || b.of("dashboard") != 400*time.Millisecond ||
		b.of("spend") != 250*time.Millisecond {
		t.Fatalf("budgets misread: %+v %v", b, err)
	}
	for _, invalid := range []string{"fast", "-1ms", "dashboard=0", "1s,2s", "1s,dashboard"} {
		if _, err := parseBudgets(invalid); err == nil {
			t.Errorf("budget %q accepted", invalid)
		}
	}
	for _, invalid := range []string{"dashboard", "dashboard=-1", "dashboard=x"} {
		if _, err := parseWeights(invalid); err == nil {
			t.Errorf("mix %q accepted", invalid)
		}
	}
}

func TestSummarizeReportsBucketEdgesUpToTheSlowest(t *testing.T) {
	h := &histogram{}
	for range 98 {
		h.record(time.Millisecond)
	}
	h.record(50 * time.Millisecond)
	h.record(70 * time.Millisecond)
	s := summarize(2, h)
	if s.Requests != 100 || s.P50ms != 1.1 || s.P95ms != 1.1 || s.P99ms != 50.1 || s.MaxMs != 70 || s.RequestsPerSecond != 50 {
		t.Fatalf("summary %+v", s)
	}
	uniform := &histogram{}
	for range 100 {
		uniform.record(time.Millisecond)
	}
	if s = summarize(1, uniform); s.P50ms != 1 || s.P99ms != 1 || s.MaxMs != 1 {
		t.Fatalf("percentiles exceed the slowest request: %+v", s)
	}
}

func TestSessionCookieIsNotSentAcrossHosts(t *testing.T) {
	env := func(name string) string {
		if name == "DONOR_LOAD_COOKIE" {
			return "session=abc"
		}
		return ""
	}
	err := run(context.Background(), []string{"-url", "http://a.example.org/,http://b.example.org/", "-duration", "1s"}, env, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "share one host") {
		t.Fatalf("got %v, want the multi-host cookie run refused", err)
	}
}
