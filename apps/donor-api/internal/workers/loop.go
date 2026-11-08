package workers

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	// StepBudget bounds one step of a worker whose external calls are short.
	StepBudget = 2 * time.Minute
	// ProcessorStepBudget covers a Stripe event's worst case, up to five
	// processor requests that each may time out after 15 s and retry twice, so
	// a slow processor ends in a recorded failure rather than an expired step.
	ProcessorStepBudget = 5 * time.Minute
	maxBackoff          = time.Minute
)

// Run repeats step until ctx ends. Each step owns one transaction; cancellation
// interrupts idle waits and bounded processor requests, and an interrupted
// transaction remains available for replay.
//
// With once, Run returns after a single step, including its error. Otherwise a
// failing step is logged and retried with exponential backoff, so a transient
// database or provider outage does not stop the worker; persistent failures
// surface through queue age in the monitor.
func Run(ctx context.Context, once bool, idle, budget time.Duration, step func(context.Context) (bool, error)) error {
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		work, cancel := context.WithTimeout(ctx, budget)
		worked, err := step(work)
		cancel()
		if once {
			return err
		}
		wait := time.Duration(0)
		switch {
		case err != nil && ctx.Err() == nil:
			failures++
			wait = min(idle<<min(failures-1, 16), maxBackoff)
			// Error text may carry row values, so only the type is logged.
			slog.Error("worker step failed", "cause", fmt.Sprintf("%T", err), "consecutive_failures", failures, "retry_in", wait.String())
		case err == nil:
			failures = 0
			if !worked {
				wait = idle
			}
		}
		if wait > 0 && !sleep(ctx, wait) {
			return ctx.Err()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RunConcurrent runs n copies of Run over the same step. The step must claim
// its work with FOR UPDATE SKIP LOCKED so copies never take the same item.
// With once every copy runs its step and the first error is returned; otherwise
// the first copy to return ends the others and its error is returned.
func RunConcurrent(ctx context.Context, n int, once bool, idle, budget time.Duration, step func(context.Context) (bool, error)) error {
	if n <= 1 {
		return Run(ctx, once, idle, budget, step)
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	results := make(chan error, n)
	for range n {
		go func() { results <- Run(ctx, once, idle, budget, step) }()
	}
	if once {
		// Cancelling here would let a slower copy skip its step.
		var first error
		for range n {
			if err := <-results; first == nil {
				first = err
			}
		}
		return first
	}
	first := <-results
	stop()
	for range n - 1 {
		<-results
	}
	return first
}
