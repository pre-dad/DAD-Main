// Command loadtest exercises the donor API with bounded concurrency, as a
// burst (high concurrency, short) or a soak (moderate concurrency, long).
//
// Given a seed fixture it runs a weighted mix of what a referral spike
// brings: referral landings and their captures, public spend totals, and signed-in
// donors reading their dashboards and giving history through browser cookies
// or the app's bearer sessions. Given -url instead it reads those endpoints in
// equal measure. It reports latency percentiles, statuses and errors per
// scenario and overall as JSON.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	requestTimeout = 10 * time.Second
	maxBodyBytes   = 1 << 20
	// bucketWidth is the latency histogram's resolution.
	bucketWidth = 100 * time.Microsecond
	buckets     = int(requestTimeout/bucketWidth) + 1
)

// clientBase is RFC 2544's benchmarking range: simulated client addresses
// that can never belong to a real visitor.
var clientBase = netip.MustParseAddr("198.18.0.0")

// user is one simulated client: a browser or the donor app, signed in or not.
type user struct {
	r       *rand.Rand
	native  bool
	address netip.Addr
}

// scenario builds one kind of request and names the status that counts as success.
type scenario struct {
	name     string
	weight   int
	expect   int
	signedIn bool // sent with the user's session; visitors are anonymous
	build    func(u *user) *http.Request
}

type config struct {
	scenarios   []scenario
	duration    time.Duration
	concurrency int
	sessions    []string
	nativeShare float64
	clientIPs   bool
}

// fixture is written by cmd/seed.
type fixture struct {
	Sessions      []string `json:"sessions"`
	ReferralCodes []string `json:"referral_codes"`
	EventIDs      []string `json:"event_ids"`
}

// defaultMix approximates a referral spike: most traffic lands on referral
// links, and a minority of donors check their recognition. The event-day and
// engagement scenarios are opt-in at 0 so the referral-spike shape stays the
// same; they are listed so -mix accepts them.
var defaultMix = map[string]int{
	"referral-lookup": 30, "referral-capture": 12, "referral-missing": 3, "spend": 15,
	"dashboard": 18, "gifts": 7, "sharing": 5, "profile": 4, "recurring": 4, "annual-summary": 2,
	"events": 0, "event-detail": 0, "event-reserve": 0, "today-initiative": 0, "team": 0,
}

// newRequest builds a request for a target parseTargets accepted, which
// http.NewRequest cannot refuse.
func newRequest(method, target string, body io.Reader) *http.Request {
	r, err := http.NewRequest(method, target, body)
	if err != nil {
		panic(err)
	}
	return r
}

func get(target string) func(*user) *http.Request {
	return func(*user) *http.Request { return newRequest(http.MethodGet, target, nil) }
}

// seededMix is the default mix against an API whose donors cmd/seed created.
func seededMix(api, origin string, f fixture, weights map[string]int) ([]scenario, error) {
	if len(f.ReferralCodes) == 0 {
		return nil, errors.New("the fixture holds no referral codes")
	}
	code := func(u *user) string { return url.PathEscape(f.ReferralCodes[u.r.IntN(len(f.ReferralCodes))]) }
	event := func(u *user) string { return url.PathEscape(f.EventIDs[u.r.IntN(len(f.EventIDs))]) }
	year := strconv.Itoa(time.Now().Year())
	all := []scenario{
		{name: "referral-lookup", expect: http.StatusOK, build: func(u *user) *http.Request {
			return newRequest(http.MethodGet, api+"/donor/referrals/"+code(u), nil)
		}},
		{name: "referral-capture", expect: http.StatusOK, build: func(u *user) *http.Request {
			kind := "LINK"
			if u.r.IntN(3) == 0 {
				kind = "IRL"
			}
			r := newRequest(http.MethodPost, api+"/donor/referrals/"+code(u), strings.NewReader(`{"kind":"`+kind+`"}`))
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/json")
			return r
		}},
		{name: "referral-missing", expect: http.StatusNotFound, build: func(u *user) *http.Request {
			// Issued codes encode 24 bytes; 32 can never name a referrer.
			unknown := make([]byte, 32)
			for i := range unknown {
				unknown[i] = byte(u.r.Uint32())
			}
			return newRequest(http.MethodGet, api+"/donor/referrals/"+base64.RawURLEncoding.EncodeToString(unknown), nil)
		}},
		{name: "spend", expect: http.StatusOK, build: get(api + "/donor/public/spend")},
		{name: "dashboard", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/dashboard")},
		{name: "gifts", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/gifts?offset=0")},
		{name: "sharing", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/sharing")},
		{name: "profile", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/profile")},
		{name: "recurring", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/recurring")},
		{name: "annual-summary", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/annual-summary/" + year)},
		// Engagement reads: the dashboard soak exercises the daily-rotation
		// lookup and the per-donor team leaderboard alongside the dashboard.
		{name: "today-initiative", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/today-initiative")},
		{name: "team", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/team")},
		// Event-day spike: the near-city list, the public detail of a specific
		// event, and reserving a seat (which takes the event row lock and runs
		// the capacity/waitlist path — the real contention point on event day).
		{name: "events", expect: http.StatusOK, signedIn: true, build: get(api + "/donor/me/events")},
		{name: "event-detail", expect: http.StatusOK, build: func(u *user) *http.Request {
			return newRequest(http.MethodGet, api+"/events/"+event(u), nil)
		}},
		{name: "event-reserve", expect: http.StatusOK, signedIn: true, build: func(u *user) *http.Request {
			r := newRequest(http.MethodPost, api+"/donor/me/events/"+event(u)+"/reserve", nil)
			r.Header.Set("Origin", origin)
			return r
		}},
	}
	var mix []scenario
	for _, s := range all {
		weight, ok := weights[s.name]
		if !ok {
			weight = defaultMix[s.name]
		}
		if weight > 0 {
			s.weight = weight
			mix = append(mix, s)
		}
	}
	for name := range weights {
		if _, known := defaultMix[name]; !known {
			return nil, fmt.Errorf("unknown scenario %q", name)
		}
	}
	if len(mix) == 0 {
		return nil, errors.New("the mix has no scenarios")
	}
	// The event scenarios index f.EventIDs; a mix that enables one needs a
	// fixture that carries events (re-seed to refresh an older fixture).
	if len(f.EventIDs) == 0 {
		for _, s := range mix {
			if s.name == "events" || s.name == "event-detail" || s.name == "event-reserve" {
				return nil, fmt.Errorf("scenario %q needs event ids; the fixture holds none", s.name)
			}
		}
	}
	return mix, nil
}

// urlMix reads each target equally; scenarios are named by host, path and
// query.
func urlMix(list string) ([]scenario, error) {
	targets, err := parseTargets(list)
	if err != nil {
		return nil, err
	}
	mix := make([]scenario, len(targets))
	for i, target := range targets {
		u, _ := url.Parse(target)
		name := u.Host + u.RequestURI()
		if slices.ContainsFunc(mix[:i], func(s scenario) bool { return s.name == name }) {
			return nil, fmt.Errorf("target %s is listed twice", name)
		}
		mix[i] = scenario{name: name, weight: 1, expect: http.StatusOK, signedIn: true, build: get(target)}
	}
	return mix, nil
}

func sameHost(targets []string) bool {
	hosts := map[string]bool{}
	for _, target := range targets {
		u, _ := url.Parse(target)
		hosts[u.Host] = true
	}
	return len(hosts) <= 1
}

func parseTargets(list string) ([]string, error) {
	var targets []string
	for target := range strings.SplitSeq(list, ",") {
		u, err := url.Parse(strings.TrimSpace(target))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return nil, fmt.Errorf("invalid target URL %q", target)
		}
		targets = append(targets, u.String())
	}
	return targets, nil
}

// parseWeights reads "name=weight,..." pairs.
func parseWeights(list string) (map[string]int, error) {
	weights := map[string]int{}
	if list == "" {
		return weights, nil
	}
	for pair := range strings.SplitSeq(list, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		weight, err := strconv.Atoi(value)
		if !ok || err != nil || weight < 0 || weight > 1000 {
			return nil, fmt.Errorf("invalid mix entry %q", pair)
		}
		weights[name] = weight
	}
	return weights, nil
}

// budgets is a p99 budget for every scenario, with per-scenario overrides.
type budgets struct {
	all       time.Duration
	scenarios map[string]time.Duration
}

// parseBudgets reads "250ms" or "250ms,dashboard=400ms"; 0 disables the
// default. A name runs to the last "=", since -url names carry queries.
func parseBudgets(list string) (budgets, error) {
	b := budgets{scenarios: map[string]time.Duration{}}
	for i, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		cut := strings.LastIndex(part, "=")
		name, value, named := part[:max(cut, 0)], part[cut+1:], cut >= 0
		budget, err := time.ParseDuration(value)
		if err != nil || budget < 0 || (named && budget == 0) || (!named && i > 0) {
			return b, fmt.Errorf("invalid p99 budget %q", part)
		}
		if named {
			b.scenarios[name] = budget
		} else {
			b.all = budget
		}
	}
	return b, nil
}

func (b budgets) of(name string) time.Duration {
	if budget, ok := b.scenarios[name]; ok {
		return budget
	}
	return b.all
}

// histogram counts one scenario's outcomes. Every field is updated
// concurrently by the virtual users.
type histogram struct {
	latency   [buckets]atomic.Int64
	statuses  [600]atomic.Int64
	transport atomic.Int64
	timeouts  atomic.Int64
	oversized atomic.Int64
	failed    atomic.Int64
	requests  atomic.Int64
	slowest   atomic.Int64
}

func (h *histogram) record(elapsed time.Duration) {
	h.latency[min(int(elapsed/bucketWidth), buckets-1)].Add(1)
	h.requests.Add(1)
	for {
		slowest := h.slowest.Load()
		if int64(elapsed) <= slowest || h.slowest.CompareAndSwap(slowest, int64(elapsed)) {
			return
		}
	}
}

type stats struct {
	Requests          int64            `json:"requests"`
	Failed            int64            `json:"failed"`
	RequestsPerSecond float64          `json:"requests_per_second"`
	P50ms             float64          `json:"p50_ms"`
	P95ms             float64          `json:"p95_ms"`
	P99ms             float64          `json:"p99_ms"`
	MaxMs             float64          `json:"max_ms"`
	Statuses          map[string]int64 `json:"statuses,omitempty"`
	Errors            map[string]int64 `json:"errors,omitempty"`
}

type result struct {
	stats
	Concurrency int              `json:"concurrency"`
	Seconds     float64          `json:"seconds"`
	Scenarios   map[string]stats `json:"scenarios"`
}

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// summarize reports the histograms' combined counts and percentiles. A
// percentile is the upper edge of the bucket holding it, or the slowest
// request if that is lower.
func summarize(seconds float64, hs ...*histogram) stats {
	var s stats
	counts := make([]int64, buckets)
	var slowest int64
	for _, h := range hs {
		s.Requests += h.requests.Load()
		s.Failed += h.failed.Load()
		slowest = max(slowest, h.slowest.Load())
		for i := range counts {
			counts[i] += h.latency[i].Load()
		}
		for status := range h.statuses {
			if n := h.statuses[status].Load(); n > 0 {
				if s.Statuses == nil {
					s.Statuses = map[string]int64{}
				}
				s.Statuses[strconv.Itoa(status)] += n
			}
		}
		for name, n := range map[string]int64{"transport": h.transport.Load(), "timeout": h.timeouts.Load(), "oversized": h.oversized.Load()} {
			if n > 0 {
				if s.Errors == nil {
					s.Errors = map[string]int64{}
				}
				s.Errors[name] += n
			}
		}
	}
	percentile := func(percent int64) float64 {
		threshold := (s.Requests*percent + 99) / 100
		var count int64
		for i, n := range counts {
			if count += n; count >= threshold && threshold > 0 {
				return milliseconds(min(time.Duration(i+1)*bucketWidth, time.Duration(slowest)))
			}
		}
		return 0
	}
	s.P50ms, s.P95ms, s.P99ms = percentile(50), percentile(95), percentile(99)
	s.MaxMs = milliseconds(time.Duration(slowest))
	if seconds > 0 {
		s.RequestsPerSecond = float64(s.Requests) / seconds
	}
	return s
}

// load runs cfg's virtual users until the duration ends or ctx is cancelled.
// Each picks scenarios by weight. A request fails on a transport error, a
// status other than the scenario's, a timeout, or a body over maxBodyBytes.
func load(ctx context.Context, cfg config) result {
	transport := &http.Transport{
		MaxIdleConns:        cfg.concurrency,
		MaxIdleConnsPerHost: cfg.concurrency,
		MaxConnsPerHost:     cfg.concurrency,
		IdleConnTimeout:     30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	total := 0
	for _, s := range cfg.scenarios {
		total += s.weight
	}
	histograms := make([]*histogram, len(cfg.scenarios))
	for i := range histograms {
		histograms[i] = &histogram{}
	}
	var visitors atomic.Uint32
	start := time.Now()
	deadline := start.Add(cfg.duration)
	var wg sync.WaitGroup
	for i := range cfg.concurrency {
		// App users are spread evenly through the numbering, so any concurrency
		// gets close to the requested share.
		u := &user{
			r:       rand.New(rand.NewPCG(uint64(i), 0)),
			native:  int(float64(i+1)*cfg.nativeShare) > int(float64(i)*cfg.nativeShare),
			address: clientAddress(uint32(i)),
		}
		wg.Go(func() {
			for time.Now().Before(deadline) && ctx.Err() == nil {
				pick := u.r.IntN(total)
				index := 0
				for pick >= cfg.scenarios[index].weight {
					pick -= cfg.scenarios[index].weight
					index++
				}
				s := cfg.scenarios[index]
				request := s.build(u).WithContext(ctx)
				if s.signedIn && len(cfg.sessions) > 0 {
					// Each signed-in request is a donor drawn from the fixture, so the
					// whole seeded population is read, not one donor per user.
					authorize(request, cfg.sessions[u.r.IntN(len(cfg.sessions))], u.native)
				}
				if cfg.clientIPs {
					address := u.address
					if request.Method == http.MethodPost {
						// Each capture is a new visitor, as in a real spike; the upper half
						// of the /15 gives 65,536 distinct visitors before addresses repeat.
						address = clientAddress(1<<16 + visitors.Add(1)%(1<<16))
					}
					request.Header.Set("X-Real-IP", address.String())
				}
				fetch(client, request, s.expect, histograms[index])
			}
		})
	}
	wg.Wait()

	seconds := time.Since(start).Seconds()
	r := result{stats: summarize(seconds, histograms...), Concurrency: cfg.concurrency, Seconds: seconds, Scenarios: map[string]stats{}}
	for i, s := range cfg.scenarios {
		r.Scenarios[s.name] = summarize(seconds, histograms[i])
	}
	return r
}

// clientAddress numbers simulated clients within clientBase's /15.
func clientAddress(n uint32) netip.Addr {
	base := clientBase.As4()
	value := (uint32(base[0])<<24 | uint32(base[1])<<16) + n%(1<<17)
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

// authorize presents a donor session as the browser does, by cookie, or as
// the donor app does, by bearer token with no cookie.
func authorize(r *http.Request, session string, native bool) {
	switch {
	case native:
		r.Header.Set("X-DAD-Client", "native")
		r.Header.Set("Authorization", "Bearer "+session)
	case strings.Contains(session, "="):
		r.Header.Set("Cookie", session) // DONOR_LOAD_COOKIE is a whole cookie header
	default:
		r.Header.Set("Cookie", "dad_donor_session="+session)
	}
}

// fetch sends request and records its outcome, unless the run was
// interrupted while it was in flight.
func fetch(client *http.Client, request *http.Request, expect int, h *histogram) {
	began := time.Now()
	response, err := client.Do(request)
	var size int64
	if err == nil {
		size, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxBodyBytes+1))
		if closeErr := response.Body.Close(); err == nil {
			err = closeErr
		}
	}
	if request.Context().Err() != nil {
		return
	}
	h.record(time.Since(began))
	if response != nil {
		h.statuses[min(max(response.StatusCode, 0), len(h.statuses)-1)].Add(1)
	}
	switch {
	case os.IsTimeout(err):
		h.timeouts.Add(1)
	case err != nil:
		h.transport.Add(1)
	case size > maxBodyBytes:
		h.oversized.Add(1)
	case response.StatusCode != expect:
	default:
		return
	}
	h.failed.Add(1)
}

func readFixture(path string) (fixture, error) {
	var f fixture
	data, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err = json.Unmarshal(data, &f); err != nil {
		return f, errors.New("the fixture is not cmd/seed output")
	}
	return f, nil
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	api := flags.String("api", "http://127.0.0.1:8002", "API base URL for the seeded mix")
	fixturePath := flags.String("fixture", "", "cmd/seed fixture; runs the seeded donor mix")
	mix := flags.String("mix", "", "scenario weights overriding the default mix, such as dashboard=30,gifts=0")
	origin := flags.String(
		"origin",
		"http://localhost:3000",
		"browser Origin on referral captures; must equal the API's DONOR_PUBLIC_ORIGIN",
	)
	nativeShare := flags.Float64("native", 0.25, "share of signed-in users that are the donor app (bearer) rather than a browser (cookie)")
	clientIPs := flags.Bool(
		"client-ips",
		false,
		"send a distinct X-Real-IP per user and per captured visitor; the API must trust this host as a proxy",
	)
	targets := flags.String("url", "", "comma-separated endpoints read in equal measure instead of the seeded mix")
	duration := flags.Duration("duration", time.Minute, "test duration (up to 1h)")
	concurrency := flags.Int("concurrency", 16, "concurrent users (1–2048)")
	maxP99 := flags.String("max-p99", "0", "p99 budget for every scenario, optionally with overrides: 250ms,dashboard=400ms (0 disables)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	budget, err := parseBudgets(*maxP99)
	if err != nil {
		return err
	}
	if *duration <= 0 || *duration > time.Hour || *concurrency < 1 || *concurrency > 2048 || *nativeShare < 0 || *nativeShare > 1 ||
		(*targets == "") == (*fixturePath == "") {
		return errors.New("invalid load-test arguments; pass either -fixture or -url")
	}
	cfg := config{duration: *duration, concurrency: *concurrency, nativeShare: *nativeShare, clientIPs: *clientIPs}
	if *targets != "" {
		flags.Visit(func(f *flag.Flag) {
			if slices.Contains([]string{"api", "mix", "origin", "native"}, f.Name) {
				err = fmt.Errorf("-%s applies to the seeded mix, not -url", f.Name)
			}
		})
		if err != nil {
			return err
		}
		if cfg.scenarios, err = urlMix(*targets); err != nil {
			return err
		}
		if cookie := getenv("DONOR_LOAD_COOKIE"); cookie != "" {
			if hosts, _ := parseTargets(*targets); !sameHost(hosts) {
				return errors.New("DONOR_LOAD_COOKIE is sent to every -url target, so they must share one host")
			}
			cfg.sessions, cfg.nativeShare = []string{cookie}, 0
		}
	} else {
		base, err := parseTargets(*api)
		if err != nil || len(base) != 1 {
			return errors.New("invalid -api URL")
		}
		f, err := readFixture(*fixturePath)
		if err != nil {
			return err
		}
		weights, err := parseWeights(*mix)
		if err != nil {
			return err
		}
		if cfg.scenarios, err = seededMix(strings.TrimRight(base[0], "/"), *origin, f, weights); err != nil {
			return err
		}
		cfg.sessions = f.Sessions
	}
	for name := range budget.scenarios {
		if !slices.ContainsFunc(cfg.scenarios, func(s scenario) bool { return s.name == name }) {
			return fmt.Errorf("p99 budget names unknown scenario %q", name)
		}
	}

	r := load(ctx, cfg)
	if err := json.NewEncoder(out).Encode(r); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("load test interrupted: %w", err)
	}
	if r.Requests == 0 || r.Failed != 0 {
		return errors.New("load test encountered failed requests")
	}
	for _, s := range cfg.scenarios {
		if b := budget.of(s.name); b > 0 && milliseconds(b) < r.Scenarios[s.name].P99ms {
			return fmt.Errorf("%s p99 latency %.1fms exceeded the %s budget", s.name, r.Scenarios[s.name].P99ms, b)
		}
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
