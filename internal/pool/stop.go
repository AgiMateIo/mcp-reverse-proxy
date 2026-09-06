// Package pool owns the gateway's backend child processes: it is the only
// place that starts one, and the only place that stops one.
package pool

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// StopPolicy is how long a backend is given at each step of being stopped.
type StopPolicy struct {
	// AfterStdin is how long the process may take to exit on its own once its
	// standard input is closed. Closing stdin is the ordinary stop signal; a
	// well-behaved MCP server reads EOF and returns.
	AfterStdin time.Duration
	// AfterTerm is how long it may take after SIGTERM, before SIGKILL.
	AfterTerm time.Duration
}

// DefaultStopPolicy is used when a deployment sets none.
var DefaultStopPolicy = StopPolicy{AfterStdin: 5 * time.Second, AfterTerm: 5 * time.Second}

// A stoppable is the part of a running process that stopping needs. Isolating
// it keeps the escalation testable on virtual time: a real process cannot be
// driven by a clock, but the decision of when to escalate can.
type stoppable interface {
	// closeStdin sends the ordinary stop signal.
	closeStdin() error
	// signal sends sig to the process group, reaching every descendant.
	signal(sig syscall.Signal) error
	// exited is closed once the process is gone.
	exited() <-chan struct{}
}

// stop escalates from the polite signal to the final one, waiting at each step
// only as long as the policy allows.
func stop(ctx context.Context, p stoppable, policy StopPolicy) error {
	var errs []error
	if err := p.closeStdin(); err != nil {
		// A process that already exited has a closed pipe; that is not a
		// failure to stop it.
		errs = append(errs, err)
	}
	if waitFor(ctx, p.exited(), policy.AfterStdin) {
		return nil
	}
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{
		{syscall.SIGTERM, policy.AfterTerm},
		{syscall.SIGKILL, 0},
	} {
		if err := p.signal(step.sig); err != nil {
			errs = append(errs, err)
			continue
		}
		if waitFor(ctx, p.exited(), step.grace) {
			return nil
		}
	}
	// SIGKILL cannot be caught, so reaching here means the process is
	// unkillable or the wait was cut short.
	select {
	case <-p.exited():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop backend: %w", errors.Join(append(errs, ctx.Err())...))
	}
}

// waitFor reports whether done closed within grace. A zero grace means "check,
// do not wait": the final signal is not negotiable.
func waitFor(ctx context.Context, done <-chan struct{}, grace time.Duration) bool {
	if grace <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}
