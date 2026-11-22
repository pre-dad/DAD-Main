package payments

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// DefaultRequestsPerSecond is Stripe's default sandbox limit per account.
const DefaultRequestsPerSecond = 25.0

const (
	BucketWorker    = "worker"    // provider reads that apply events
	BucketCheckout  = "checkout"  // hosted checkout and card setup for donors
	BucketStaff     = "staff"     // refunds, cancellations and donor subscription changes
	BucketReconcile = "reconcile" // the daily reconciliation job
)

// shares divide the account's ceiling so that a referral spike's checkouts,
// a backlog of events, and staff refunds cannot starve one another. They sum
// to one.
var shares = map[string]float64{
	BucketWorker:    0.4,
	BucketCheckout:  0.3,
	BucketStaff:     0.2,
	BucketReconcile: 0.1,
}

// BucketRate is a bucket's sustained rate in requests per second at the given
// account ceiling, and its burst: one second of that rate, and at least one.
func BucketRate(bucket string, ceiling float64) (rate, burst float64, ok bool) {
	share, ok := shares[bucket]
	rate = share * ceiling
	return rate, max(rate, 1), ok
}

// RequestsPerSecond reads DONOR_STRIPE_REQUESTS_PER_SECOND, the account's
// request ceiling, which every process calling Stripe must share.
func RequestsPerSecond(getenv func(string) string) (float64, error) {
	value := getenv("DONOR_STRIPE_REQUESTS_PER_SECOND")
	if value == "" {
		return DefaultRequestsPerSecond, nil
	}
	ceiling, err := strconv.ParseFloat(value, 64)
	if err != nil || !(ceiling >= 1 && ceiling <= 10_000) {
		return 0, errors.New("DONOR_STRIPE_REQUESTS_PER_SECOND must be a number from 1 to 10000")
	}
	return ceiling, nil
}

// Budget paces Stripe requests. Acquire returns once the named bucket admits
// one request, or with an error when it cannot before ctx ends.
type Budget interface {
	Acquire(ctx context.Context, bucket string) error
}

// ErrUnknownBucket reports a request from a bucket with no share.
var ErrUnknownBucket = errors.New("unknown Stripe request bucket")

// bucket is a token bucket refilled lazily from elapsed time.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(rate, burst float64) *bucket {
	if rate <= 0 {
		panic("payments: bucket needs a positive refill rate")
	}
	return &bucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// reserve spends a token and returns zero, or returns how long until one is
// available.
func (b *bucket) reserve(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// RateBudget is a Budget kept in one process. It suits a process that is the
// only caller of its buckets, such as the daily reconciliation job; processes
// that run as replicas share a store.StripeBudget instead.
type RateBudget struct {
	buckets map[string]*bucket
}

func NewRateBudget(ceiling float64) *RateBudget {
	buckets := map[string]*bucket{}
	for name := range shares {
		rate, burst, _ := BucketRate(name, ceiling)
		buckets[name] = newBucket(rate, burst)
	}
	return &RateBudget{buckets: buckets}
}

var DefaultRateBudget = sync.OnceValue(func() *RateBudget { return NewRateBudget(DefaultRequestsPerSecond) })

func (r *RateBudget) Acquire(ctx context.Context, name string) error {
	b, ok := r.buckets[name]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownBucket, name)
	}
	for {
		wait := b.reserve(time.Now())
		if wait <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
