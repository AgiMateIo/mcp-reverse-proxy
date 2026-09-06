package pool

import (
	"context"
	"errors"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// A fakeProcess stands in for a running backend so that the escalation can be
// driven on virtual time. A real process cannot be: its exit is a syscall, not
// something a clock can reach.
type fakeProcess struct {
	// exitAfter is which step finally ends the process.
	exitAfter step

	mu    sync.Mutex
	steps []step
	done  chan struct{}
	// stdinErr is returned by closeStdin, as a pipe already closed by an
	// exited process would.
	stdinErr error
}

type step string

const (
	stepStdin step = "close stdin"
	stepTerm  step = "SIGTERM"
	stepKill  step = "SIGKILL"
	stepNever step = "never"
)

func newFake(exitAfter step) *fakeProcess {
	return &fakeProcess{exitAfter: exitAfter, done: make(chan struct{})}
}

func (f *fakeProcess) record(s step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, s)
	if s == f.exitAfter {
		close(f.done)
	}
}

func (f *fakeProcess) taken() []step {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.steps)
}

func (f *fakeProcess) closeStdin() error {
	f.record(stepStdin)
	return f.stdinErr
}

func (f *fakeProcess) signal(sig syscall.Signal) error {
	switch sig {
	case syscall.SIGTERM:
		f.record(stepTerm)
	case syscall.SIGKILL:
		f.record(stepKill)
	}
	return nil
}

func (f *fakeProcess) exited() <-chan struct{} { return f.done }

// Task 4.13: closing standard input is the ordinary stop signal, and each
// further step is taken only because the previous one was not enough.
func TestStopEscalates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		exitAfter step
		want      []step
	}{
		{
			name:      "a well-behaved backend exits on EOF",
			exitAfter: stepStdin,
			want:      []step{stepStdin},
		},
		{
			name:      "one that ignores EOF is asked to terminate",
			exitAfter: stepTerm,
			want:      []step{stepStdin, stepTerm},
		},
		{
			name:      "one that ignores SIGTERM is killed",
			exitAfter: stepKill,
			want:      []step{stepStdin, stepTerm, stepKill},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFake(tt.exitAfter)
				if err := stop(t.Context(), f, DefaultStopPolicy); err != nil {
					t.Errorf("stop: %v", err)
				}
				if got := f.taken(); !slices.Equal(got, tt.want) {
					t.Errorf("steps = %v, want %v", got, tt.want)
				}
			})
		})
	}
}

// Escalation waits the whole grace period and not a moment longer: a shorter
// wait would kill a backend that was still shutting down cleanly, and a longer
// one would stall the pool.
func TestStopWaitsExactlyTheGracePeriod(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		policy := StopPolicy{AfterStdin: 3 * time.Second, AfterTerm: 7 * time.Second}
		f := newFake(stepKill)
		start := time.Now()
		if err := stop(t.Context(), f, policy); err != nil {
			t.Errorf("stop: %v", err)
		}
		if got, want := time.Since(start), policy.AfterStdin+policy.AfterTerm; got != want {
			t.Errorf("stopping took %v, want %v", got, want)
		}
	})
}

// A pipe that is already closed because the process is already gone is not a
// failure to stop it.
func TestStopToleratesAClosedPipe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake(stepStdin)
		f.stdinErr = errors.New("file already closed")
		if err := stop(t.Context(), f, DefaultStopPolicy); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
}

// A backend that survives even SIGKILL is reported rather than waited on
// forever.
func TestStopReportsAnUnkillableBackend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := newFake(stepNever)
		err := stop(ctx, f, DefaultStopPolicy)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if got := f.taken(); !slices.Equal(got, []step{stepStdin, stepTerm, stepKill}) {
			t.Errorf("steps = %v, want every step to have been tried", got)
		}
	})
}
